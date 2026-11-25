package inspector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
)

// Browser side of the Inspector listener.
const (
	// EnterPath consumes a ticket and starts a session.
	EnterPath = "/_golem-deploy/enter"
	// maxProxyBody bounds request bodies relayed to the hub (they are
	// buffered so a request can be repeated after a hub restart).
	maxProxyBody = 64 << 10
	// entryFailuresPerWindow bounds failed entries per client address.
	entryFailuresPerWindow = 10
	entryWindow            = 15 * time.Minute
	// reloginInterval is the least time between two hub logins for one
	// session.
	reloginInterval = 3 * time.Second
	// requestTimeout bounds a proxied request that is not a WebSocket.
	requestTimeout = 2 * time.Minute
)

// blockedPaths are never forwarded to the hub from browsers: the daemon
// logs in itself, agents use the agent endpoint, metrics stay internal.
var blockedPaths = map[string]bool{"/auth/login": true, "/ws/agent": true, "/metrics": true}

type sessionKey struct{}

// hubError writes the hub's JSON error shape.
func hubError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// page writes one of the daemon's small static pages.
func (s *Service) page(w http.ResponseWriter, status int, title, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<title>`+title+`</title><style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;line-height:1.5;color:#222;background:#fff}`+
		`@media (prefers-color-scheme:dark){body{color:#ddd;background:#111}}</style></head><body><h1>`+title+`</h1><p>`+text+`</p></body></html>`)
}

// browserHandler serves browsers: on the TLS side of a listener that
// also accepts plaintext agents, or on a plaintext listener, where it also
// serves the agent endpoint.
func (s *Service) browserHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tlsListener() && r.URL.Path == "/ws/agent" {
			// Agents authenticate with their token and are checked by
			// peer address; their Host is an address (or
			// host.docker.internal), not a panel host name.
			s.handleAgent(w, r)
			return
		}
		if s.tlsListener() {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !s.hostAllowed(r.Host) {
			hubError(w, http.StatusMisdirectedRequest, "bad_host")
			return
		}
		p := r.URL.Path
		if blockedPaths[p] || blockedPaths[path.Clean("/"+p)] {
			hubError(w, http.StatusNotFound, "not_found")
			return
		}
		switch p {
		case EnterPath:
			s.handleEnter(w, r)
		case "/auth/logout":
			s.handleLogout(w, r)
		default:
			s.handleProxy(w, r)
		}
	})
}

// hostAllowed applies the panel's Host policy; the host of
// --inspector-public-url is allowed too.
func (s *Service) hostAllowed(host string) bool {
	if s.cfg.Policy.HostAllowed(host) {
		return true
	}
	ph := PublicHost(s.cfg.PublicURL)
	return ph != "" && netpolicy.Hostname(host) == strings.ToLower(strings.Trim(ph, "[]"))
}

// secureCookie reports whether the session cookie is marked Secure.
func (s *Service) secureCookie() bool {
	return s.tlsListener() || strings.HasPrefix(s.cfg.PublicURL, "https://")
}

func (s *Service) tlsListener() bool { return s.cfg.TLSConfig != nil }

func (s *Service) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureCookie(),
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// handleEnter consumes a ticket, logs in at the hub for the new session,
// sets the session cookie and redirects to the Inspector.
func (s *Service) handleEnter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// Not HEAD either: a link preview must not use up the ticket.
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	res, wait, ok := s.entryLimit.Reserve(s.cfg.Policy.ClientIP(r))
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		s.page(w, http.StatusTooManyRequests, "Too many attempts",
			"Too many Inspector links failed from this address. Wait a few minutes, then open the Inspector again from Golem Deploy.")
		return
	}
	tk, ok := s.tickets.consume(r.URL.Query().Get("ticket"))
	if !ok {
		s.page(w, http.StatusForbidden, "Inspector link not valid",
			"This Inspector link was already used or has expired. Open the Inspector again from Golem Deploy.")
		return
	}
	acct, err := s.cfg.Store.GetAccount(r.Context(), tk.accountID)
	if err != nil || !acct.Approved || acct.TokenVersion != tk.tokenVersion {
		s.page(w, http.StatusForbidden, "Inspector link not valid",
			"The account of this Inspector link can no longer open it. Sign in to Golem Deploy and open the Inspector again.")
		return
	}
	res.Refund()
	if _, _, serving := s.agentTarget(); !serving {
		s.page(w, http.StatusServiceUnavailable, "Inspector not running",
			"The Inspector is not running. Check its state in Golem Deploy.")
		return
	}
	lctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	sid, err := s.hubLogin(lctx)
	cancel()
	if err != nil {
		s.logger.Warn("could not open an Inspector session at the hub", "account_id", acct.ID, "err", err)
		s.page(w, http.StatusServiceUnavailable, "Inspector unavailable",
			"The Inspector could not be opened right now. Try again in a moment from Golem Deploy.")
		return
	}
	cookie, _, evicted, err := s.sessions.create(acct.ID, acct.TokenVersion, sid)
	if err != nil {
		s.logoutHub(sid)
		s.page(w, http.StatusInternalServerError, "Inspector unavailable", "The Inspector session could not be created.")
		return
	}
	for _, old := range evicted {
		s.endSession(old)
	}
	s.logger.Info("inspector session started", "account_id", acct.ID)
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: cookie, Path: "/", HttpOnly: true, Secure: s.secureCookie(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(SessionMaxAge / time.Second)})
	target := "/"
	if tk.target != "" {
		target = "/?targetId=" + url.QueryEscape(tk.target)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// sessionFor returns the request's valid session: the cookie names a live
// session whose account still exists, is approved and has the same token
// version. A session that fails the check is ended.
func (s *Service) sessionFor(r *http.Request) *session {
	ck, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil
	}
	sess, expired := s.sessions.lookup(ck.Value)
	if expired != nil {
		s.endSession(expired)
	}
	if sess == nil {
		return nil
	}
	acct, err := s.cfg.Store.GetAccount(r.Context(), sess.accountID)
	if err != nil || !acct.Approved || acct.TokenVersion != sess.tokenVersion {
		if err == nil || !isTransient(r.Context(), err) {
			s.endSession(sess)
		}
		return nil
	}
	return sess
}

func isTransient(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// unauthenticated answers a request without a valid session: a static page
// for a page navigation, the hub's JSON error otherwise.
func (s *Service) unauthenticated(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(SessionCookie); err == nil {
		s.clearCookie(w)
	}
	if r.Method == http.MethodGet && !isWebSocket(r) && strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.page(w, http.StatusUnauthorized, "Inspector session ended",
			"Your Inspector session has ended. Open the Inspector again from Golem Deploy.")
		return
	}
	hubError(w, http.StatusUnauthorized, "auth_required")
}

// expectedOrigin is the origin browsers use for the Inspector.
func (s *Service) expectedOrigin(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if s.tlsListener() {
		scheme = "https"
	}
	return scheme + "://" + strings.ToLower(r.Host)
}

// originOK checks the Origin of a WebSocket upgrade or a request that is
// not GET or HEAD.
func (s *Service) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o != "" && strings.EqualFold(o, s.expectedOrigin(r))
}

// handleLogout forwards a sign-out to the hub, then ends the daemon
// session and clears the cookie.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	sess := s.sessionFor(r)
	if sess == nil {
		s.unauthenticated(w, r)
		return
	}
	if !s.originOK(r) {
		hubError(w, http.StatusForbidden, "origin_not_allowed")
		return
	}
	s.logger.Info("inspector session ended by sign-out", "account_id", sess.accountID)
	s.endSessionSync(r.Context(), sess)
	s.clearCookie(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"authenticated":false}`+"\n")
}

// handleProxy relays a request of a valid session to the hub.
func (s *Service) handleProxy(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionFor(r)
	if sess == nil {
		s.unauthenticated(w, r)
		return
	}
	upgrade := isWebSocket(r)
	if (upgrade || (r.Method != http.MethodGet && r.Method != http.MethodHead)) && !s.originOK(r) {
		hubError(w, http.StatusForbidden, "origin_not_allowed")
		return
	}
	if _, port, serving := s.agentTarget(); !serving || port == 0 {
		hubError(w, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	if r.Body != nil && r.Body != http.NoBody && r.Method != http.MethodGet && r.Method != http.MethodHead {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBody))
		if err != nil {
			hubError(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	ctx, release, ok := sess.track(r.Context())
	if !ok {
		s.unauthenticated(w, r)
		return
	}
	defer release()
	rc := http.NewResponseController(w)
	if !upgrade {
		_ = rc.SetReadDeadline(time.Now().Add(requestTimeout))
		_ = rc.SetWriteDeadline(time.Now().Add(requestTimeout))
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, sessionKey{}, sess)))
}

// newBrowserProxy relays browser requests to the hub with only the
// session's hub cookie: the browser's cookies, credentials and forwarding
// headers are dropped, the Origin is rewritten to the hub's fixed origin
// and the hub's Set-Cookie never reaches the browser.
func (s *Service) newBrowserProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			_, port, _ := s.agentTarget()
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)})
			pr.Out.Host = hubHost
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			if sess, _ := pr.In.Context().Value(sessionKey{}).(*session); sess != nil {
				if sid := sess.hubSession(); sid != "" {
					pr.Out.Header.Set("Cookie", hubCookie+"="+sid)
				}
			}
			if pr.Out.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", HubOrigin)
			}
		},
		Transport:      &retryTransport{s: s, base: newHubTransport()},
		ModifyResponse: func(resp *http.Response) error { resp.Header.Del("Set-Cookie"); return nil },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the browser left, or the session ended
			}
			s.logger.Debug("inspector proxy request failed", "err", err)
			// Docker may have restarted the hub on another port.
			s.refreshHub(context.WithoutCancel(r.Context()))
			hubError(w, http.StatusBadGateway, "hub_unavailable")
		},
		ErrorLog: s.proxyLog,
	}
}

// retryTransport logs in again, once, when the hub no longer knows the
// session's hub session (the hub restarted), and repeats the request with
// the new session.
type retryTransport struct {
	s    *Service
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	sess, _ := req.Context().Value(sessionKey{}).(*session)
	if sess == nil {
		return resp, nil
	}
	var lost bool
	lost, resp = hubSessionLost(req, resp)
	if !lost {
		return resp, nil
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, nil
	}
	old := ""
	if ck, err := req.Cookie(hubCookie); err == nil {
		old = ck.Value
	}
	sid, ok := t.s.relogin(req.Context(), sess, old)
	if !ok {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	again := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		again.Body = body
	}
	again.Header.Set("Cookie", hubCookie+"="+sid)
	return t.base.RoundTrip(again)
}

// hubSessionLost reports whether the hub answered as if the request had no
// session: 401, or {"authenticated":false} from /auth/session.
func hubSessionLost(req *http.Request, resp *http.Response) (bool, *http.Response) {
	if resp.StatusCode == http.StatusUnauthorized {
		return true, resp
	}
	if req.URL.Path != "/auth/session" || resp.StatusCode != http.StatusOK {
		return false, resp
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return false, resp
	}
	var v struct {
		Authenticated *bool `json:"authenticated"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Authenticated != nil && !*v.Authenticated {
		return true, resp
	}
	return false, resp
}

// relogin replaces the session's hub session (at most once per
// reloginInterval; never while hub logins are paused). It returns the
// current hub session when another request already replaced old.
func (s *Service) relogin(ctx context.Context, sess *session, old string) (string, bool) {
	sess.loginMu.Lock()
	defer sess.loginMu.Unlock()
	sess.mu.Lock()
	cur, ended, last := sess.hubSID, sess.ended, sess.lastLogin
	if ended {
		sess.mu.Unlock()
		return "", false
	}
	if cur != "" && cur != old {
		sess.mu.Unlock()
		return cur, true
	}
	now := s.cfg.Now()
	if now.Sub(last) < reloginInterval {
		sess.mu.Unlock()
		return "", false
	}
	sess.lastLogin = now
	sess.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sid, err := s.hubLogin(lctx)
	if err != nil {
		s.logger.Info("could not renew an Inspector session at the hub", "account_id", sess.accountID, "err", err)
		return "", false
	}
	sess.mu.Lock()
	if sess.ended {
		sess.mu.Unlock()
		s.logoutHub(sid)
		return "", false
	}
	sess.hubSID = sid
	sess.mu.Unlock()
	return sid, true
}

// hubLogin logs in at the running hub with the daemon's password.
func (s *Service) hubLogin(ctx context.Context) (string, error) {
	now := s.cfg.Now()
	if s.guard.blocked(now) {
		return "", errHubBlocked
	}
	s.mu.Lock()
	port := s.hubPort
	pw := ""
	if s.sec != nil {
		pw = s.sec.Password
	}
	s.mu.Unlock()
	if port == 0 || pw == "" {
		return "", errHubLogin
	}
	sid, err := s.hub.login(ctx, port, pw)
	s.guard.record(now, err)
	if err != nil && (errors.Is(err, errHubCredentials) || errors.Is(err, errHubRateLimited)) {
		s.logger.Warn("the Inspector hub refused the daemon's login; logins pause until the hub is applied again or its limit ends", "err", err)
	}
	return sid, err
}

// logoutHub ends a hub session in the background (best effort).
func (s *Service) logoutHub(sid string) {
	if sid == "" {
		return
	}
	s.mu.Lock()
	port := s.hubPort
	s.mu.Unlock()
	if port == 0 {
		return
	}
	s.bg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.hub.logout(ctx, port, sid)
	})
}

// endSession ends a session: its streams close and its hub session is
// logged out in the background.
func (s *Service) endSession(sess *session) {
	s.sessions.remove(sess)
	s.logoutHub(sess.end())
}

// endSessionSync is endSession with the hub logout done first, inline.
func (s *Service) endSessionSync(ctx context.Context, sess *session) {
	s.sessions.remove(sess)
	s.mu.Lock()
	port := s.hubPort
	s.mu.Unlock()
	if sid := sess.hubSession(); sid != "" && port != 0 {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = s.hub.logout(lctx, port, sid)
		cancel()
	}
	sess.end()
}
