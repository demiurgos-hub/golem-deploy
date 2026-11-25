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
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Browser side of the Inspector listener.
const (
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
// logs in itself, agents use the agent endpoint, metrics and health stay
// internal.
var blockedPaths = map[string]bool{"/auth/login": true, "/ws/agent": true, "/metrics": true, "/health": true, "/live": true, "/ready": true}

// reservedPrefixes are the hub's paths outside its static UI (the hub's
// own list). Everything under them needs a session; the rest is the
// static UI, relayed without one.
var reservedPrefixes = []string{"/api", "/auth", "/ws", "/health", "/live", "/ready", "/metrics"}

func reservedPath(p string) bool {
	for _, prefix := range reservedPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// proxyKey carries a relayed request's proxyInfo.
type proxyKey struct{}

// proxyInfo is what the proxy knows about a relayed request: its session
// (nil for the static UI) and whether a WebSocket upgrade offered the
// marker subprotocol.
type proxyInfo struct {
	sess    *session
	offered bool
}

func proxyInfoFrom(ctx context.Context) proxyInfo {
	v, _ := ctx.Value(proxyKey{}).(proxyInfo)
	return v
}

// hubError writes the hub's JSON error shape.
func hubError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
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
		// The hub cleans paths too (it redirects to the clean path), so
		// both spellings are classified and the stricter rule wins.
		p := r.URL.Path
		clean := path.Clean("/" + p)
		switch {
		case blockedPaths[p] || blockedPaths[clean]:
			hubError(w, http.StatusNotFound, "not_found")
		case strings.HasPrefix(p, ownPrefix) || strings.HasPrefix(clean, ownPrefix):
			s.handleOwn(w, r)
		case p == "/auth/logout":
			s.handleLogout(w, r)
		case reservedPath(p) || reservedPath(clean):
			s.handleProxy(w, r)
		default:
			s.handleStatic(w, r)
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

func (s *Service) tlsListener() bool { return s.cfg.TLSConfig != nil }

// sessionFor returns the request's valid session: the presented key names
// a live session whose account still exists, is approved and has the same
// token version. A session that fails the check is ended. unavailable is
// true when the database did not answer (the session is kept).
func (s *Service) sessionFor(r *http.Request) (sess *session, unavailable bool) {
	key, _ := presentedKey(r)
	if key == "" {
		return nil, false
	}
	sess, expired := s.sessions.lookup(key)
	if expired != nil {
		s.endSession(expired)
	}
	if sess == nil {
		return nil, false
	}
	acct, err := s.cfg.Store.GetAccount(r.Context(), sess.accountID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, true // the database did not answer: refuse this request only
	}
	if err != nil || !acct.Approved || acct.TokenVersion != sess.tokenVersion {
		s.endSession(sess)
		return nil, false
	}
	return sess, false
}

// unauthenticated answers a request without a valid session. The header
// tells the boot script to drop its key.
func (s *Service) unauthenticated(w http.ResponseWriter) {
	w.Header().Set(SessionHeader, "ended")
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

// originOK checks the Origin of a WebSocket upgrade, a ticket exchange or
// another request that is not GET or HEAD.
func (s *Service) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o != "" && strings.EqualFold(o, s.expectedOrigin(r))
}

// handleLogout forwards a sign-out to the hub, then ends the daemon
// session.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	sess, unavailable := s.sessionFor(r)
	if unavailable {
		hubError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if sess == nil {
		s.unauthenticated(w)
		return
	}
	if !s.originOK(r) {
		hubError(w, http.StatusForbidden, "origin_not_allowed")
		return
	}
	s.logger.Info("inspector session ended by sign-out", "account_id", sess.accountID)
	s.endSessionSync(r.Context(), sess)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"authenticated":false}`+"\n")
}

// handleStatic relays a GET or HEAD of the hub's static UI without any
// credentials: no session is looked up and none reaches the hub.
func (s *Service) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hubError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if isWebSocket(r) {
		hubError(w, http.StatusNotFound, "not_found")
		return
	}
	if _, port, serving := s.agentTarget(); !serving || port == 0 {
		hubError(w, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	r.Body, r.ContentLength = http.NoBody, 0
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(requestTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(requestTimeout))
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, proxyKey{}, proxyInfo{})))
}

// handleProxy relays a request of a valid session to the hub.
func (s *Service) handleProxy(w http.ResponseWriter, r *http.Request) {
	sess, unavailable := s.sessionFor(r)
	if unavailable {
		hubError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if sess == nil {
		s.unauthenticated(w)
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
		s.unauthenticated(w)
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
	_, offered := presentedKey(r)
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, proxyKey{}, proxyInfo{sess: sess, offered: upgrade && offered})))
}

// newBrowserProxy relays browser requests to the hub with only the
// session's hub cookie: the browser's cookies, credentials, subprotocols
// and forwarding headers are dropped, the Origin is rewritten to the hub's
// fixed origin, the hub's Set-Cookie never reaches the browser, and HTML
// documents get the boot script.
func (s *Service) newBrowserProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			_, port, _ := s.agentTarget()
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)})
			pr.Out.Host = hubHost
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			// The Inspector UI offers no subprotocols of its own: these
			// are the marker and the key.
			pr.Out.Header.Del("Sec-WebSocket-Protocol")
			// Documents are read to insert the boot script.
			pr.Out.Header.Set("Accept-Encoding", "identity")
			if sess := proxyInfoFrom(pr.In.Context()).sess; sess != nil {
				if sid := sess.hubSession(); sid != "" {
					pr.Out.Header.Set("Cookie", hubCookie+"="+sid)
				}
			}
			if pr.Out.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", HubOrigin)
			}
		},
		Transport: &retryTransport{s: s, base: newHubTransport()},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			resp.Header.Del(SessionHeader)
			if resp.StatusCode == http.StatusSwitchingProtocols {
				// Browsers fail a connection that offered subprotocols
				// and got none back.
				resp.Header.Del("Sec-WebSocket-Protocol")
				if resp.Request != nil && proxyInfoFrom(resp.Request.Context()).offered {
					resp.Header.Set("Sec-WebSocket-Protocol", WebSocketProtocol)
				}
				return nil
			}
			return injectBoot(resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the browser left, or the session ended
			}
			if errors.Is(err, errDocument) {
				s.logger.Warn("an Inspector page from the hub could not carry the boot script and was not served")
				s.page(w, http.StatusBadGateway, "Inspector unavailable",
					"The Inspector page could not be prepared. Try again in a moment from Golem Deploy.")
				return
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
	sess := proxyInfoFrom(req.Context()).sess
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
