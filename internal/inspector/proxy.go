package inspector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// maxSessionAnswer bounds the hub's /auth/session answer, which the
	// daemon reads to tell a lost hub session.
	maxSessionAnswer = 4 << 10
	// entryFailuresPerWindow bounds failed entries per client address.
	entryFailuresPerWindow = 10
	entryWindow            = 15 * time.Minute
	// reloginInterval is the least time between two hub logins for one
	// session.
	reloginInterval = 3 * time.Second
	// requestTimeout bounds a request that is not a relayed WebSocket.
	requestTimeout = 2 * time.Minute
	// rejectDrain bounds the time Go spends reading the body of a refused
	// request before the connection closes.
	rejectDrain = time.Second
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
// (nil for the static UI), whether a WebSocket upgrade offered the marker
// subprotocol, and the hub snapshot (port and secrets) the request uses
// from start to end.
type proxyInfo struct {
	sess    *session
	offered bool
	view    hubView
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

// hasBody reports whether r announces a request body.
func hasBody(r *http.Request) bool {
	return r.ContentLength != 0 || len(r.TransferEncoding) > 0
}

// reject answers a refused request. A request with a body gets
// "Connection: close" and only a short time to finish sending it: what
// is left of it is never read as another request.
func reject(w http.ResponseWriter, r *http.Request, status int, code string) {
	if hasBody(r) {
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(rejectDrain))
	}
	hubError(w, status, code)
}

// prepare starts every request of the listener: it sets the read and
// write deadlines (a relayed WebSocket clears them when it takes over the
// connection) and refuses what the listener never serves: announced
// trailers, a GET or HEAD with a body, an upgrade to anything but a
// WebSocket, and a WebSocket upgrade with a body. It reports whether
// handling goes on.
func (s *Service) prepare(w http.ResponseWriter, r *http.Request) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.reqTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(s.reqTimeout))
	switch {
	case len(r.Header.Values("Trailer")) > 0 || len(r.Trailer) > 0:
		reject(w, r, http.StatusBadRequest, "bad_request")
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && hasBody(r):
		reject(w, r, http.StatusBadRequest, "bad_request")
	case asksUpgrade(r) && (!isWebSocket(r) || hasBody(r)):
		reject(w, r, http.StatusBadRequest, "upgrade_not_allowed")
	default:
		return true
	}
	return false
}

// browserHandler serves browsers: on the TLS side of a listener that
// also accepts plaintext agents, or on a plaintext listener, where it also
// serves the agent endpoint.
func (s *Service) browserHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.prepare(w, r) {
			return
		}
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
			reject(w, r, http.StatusMisdirectedRequest, "bad_host")
			return
		}
		// The hub cleans paths too (it redirects to the clean path), so
		// both spellings are classified and the stricter rule wins.
		p := r.URL.Path
		clean := path.Clean("/" + p)
		switch {
		case blockedPaths[p] || blockedPaths[clean]:
			reject(w, r, http.StatusNotFound, "not_found")
		case asksUpgrade(r) && !(strings.HasPrefix(p, "/ws/") && strings.HasPrefix(clean, "/ws/")):
			// Only the hub's WebSockets under /ws/ are relayed.
			reject(w, r, http.StatusBadRequest, "upgrade_not_allowed")
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
	return ph != "" && canonicalHost(netpolicy.Hostname(host)) == canonicalHost(ph)
}

func (s *Service) tlsListener() bool { return s.cfg.TLSConfig != nil }

// knownKey reports whether r presents the key of a live session, without
// asking the database. An expired session it finds is ended.
func (s *Service) knownKey(r *http.Request) bool {
	key, _ := presentedKey(r)
	if key == "" {
		return false
	}
	sess, expired := s.sessions.lookup(key)
	if expired != nil {
		s.endSession(expired)
	}
	return sess != nil
}

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
func (s *Service) unauthenticated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(SessionHeader, "ended")
	reject(w, r, http.StatusUnauthorized, "auth_required")
}

// expectedOrigin is the origin browsers use for the Inspector, in
// canonical form.
func (s *Service) expectedOrigin(r *http.Request) (string, bool) {
	if s.publicOrigin != "" {
		return s.publicOrigin, true
	}
	scheme := "http"
	if s.tlsListener() {
		scheme = "https"
	}
	o, err := canonicalOrigin(scheme + "://" + r.Host)
	return o, err == nil
}

// originOK checks the Origin of a WebSocket upgrade, a ticket exchange or
// another request that is not GET or HEAD: exactly one Origin header,
// equal to the Inspector's origin once both are in canonical form.
func (s *Service) originOK(r *http.Request) bool {
	vs := r.Header.Values("Origin")
	if len(vs) != 1 {
		return false
	}
	got, err := canonicalOrigin(vs[0])
	if err != nil {
		return false
	}
	want, ok := s.expectedOrigin(r)
	return ok && got == want
}

// handleLogout ends the daemon session at once, then forwards the
// sign-out to the hub. Only POST without a body is accepted (405 for other
// methods, 400 for a body): nothing happens before what the client sent
// was read.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if hasBody(r) {
		reject(w, r, http.StatusBadRequest, "bad_request")
		return
	}
	if !s.knownKey(r) {
		s.unauthenticated(w, r)
		return
	}
	if !s.originOK(r) {
		reject(w, r, http.StatusForbidden, "origin_not_allowed")
		return
	}
	sess, unavailable := s.sessionFor(r)
	if unavailable {
		reject(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if sess == nil {
		s.unauthenticated(w, r)
		return
	}
	s.logger.Info("inspector session ended by sign-out", "account_id", sess.accountID)
	s.sessions.remove(sess)
	if sid := sess.end(); sid != "" {
		if v := s.hubNow(); v.port != 0 {
			lctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			s.logoutAt(lctx, v, sid)
			cancel()
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"authenticated":false}`+"\n")
}

// handleStatic relays a GET or HEAD of the hub's static UI without any
// credentials: no session is looked up and none reaches the hub.
func (s *Service) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		reject(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	v, serving := s.view()
	if !serving {
		reject(w, r, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.reqTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, proxyKey{}, proxyInfo{view: v})
	s.proxy.ServeHTTP(newProxyWriter(w, ctx), r.WithContext(ctx))
}

// handleProxy relays a request of a valid session to the hub. The cheap
// checks come first; the body is read next; then the session is checked
// against its account and the request is registered with it in one step,
// so a session that ended meanwhile never starts a request.
func (s *Service) handleProxy(w http.ResponseWriter, r *http.Request) {
	if !s.knownKey(r) {
		s.unauthenticated(w, r)
		return
	}
	upgrade := isWebSocket(r)
	if (upgrade || (r.Method != http.MethodGet && r.Method != http.MethodHead)) && !s.originOK(r) {
		reject(w, r, http.StatusForbidden, "origin_not_allowed")
		return
	}
	if s.hooks.afterKeyCheck != nil {
		s.hooks.afterKeyCheck()
	}
	var body []byte
	if hasBody(r) {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBody))
		if err != nil {
			reject(w, r, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
	}
	sess, unavailable := s.sessionFor(r)
	if unavailable {
		hubError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if sess == nil {
		s.unauthenticated(w, r)
		return
	}
	v, serving := s.view()
	if !serving {
		hubError(w, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	ctx, release, ok := sess.track(r.Context(), s.cfg.Now())
	if !ok {
		s.unauthenticated(w, r)
		return
	}
	defer release()
	// The answer is never written after the session expired: the write
	// deadline is the earlier of the request timeout and the expiry (a
	// relayed WebSocket clears it and ends with the session instead).
	left := sess.expires.Sub(s.cfg.Now())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(min(s.reqTimeout, left)))
	if !upgrade {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.reqTimeout)
		defer cancel()
	}
	_, offered := presentedKey(r)
	ctx = context.WithValue(ctx, proxyKey{}, proxyInfo{sess: sess, offered: upgrade && offered, view: v})
	out := r.WithContext(ctx)
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.TransferEncoding = nil
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	s.proxy.ServeHTTP(newProxyWriter(w, ctx), out)
}

// newBrowserProxy relays browser requests to the hub with only the
// session's hub cookie: the browser's cookies, credentials, subprotocols
// and forwarding headers are dropped, the Origin is rewritten to the hub's
// fixed origin, the hub's Set-Cookie never reaches the browser, and HTML
// documents get the boot script.
func (s *Service) newBrowserProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			info := proxyInfoFrom(pr.In.Context())
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(info.view.port)})
			pr.Out.Host = hubHost
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			// The Inspector UI offers no subprotocols of its own: these
			// are the marker and the key.
			pr.Out.Header.Del("Sec-WebSocket-Protocol")
			// Documents are read to insert the boot script.
			pr.Out.Header.Set("Accept-Encoding", "identity")
			if info.sess != nil {
				if sid := info.sess.hubSession(); sid != "" {
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
				if !webSocketAnswer(resp) {
					return errBadUpgrade
				}
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
			info := proxyInfoFrom(r.Context())
			switch {
			case r.Context().Err() != nil:
				// The browser left, the request ran out of time or its
				// session ended. Answer anyway: never an empty 200.
				if info.sess != nil && info.sess.isEnded() {
					w.Header().Set(SessionHeader, "ended")
					hubError(w, http.StatusUnauthorized, "auth_required")
					return
				}
				hubError(w, http.StatusServiceUnavailable, "hub_unavailable")
			case errors.Is(err, errDocument):
				s.logger.Warn("an Inspector page from the hub could not carry the boot script and was not served")
				s.page(w, http.StatusBadGateway, "Inspector unavailable",
					"The Inspector page could not be prepared. Try again in a moment from Golem Deploy.")
			case errors.Is(err, errHubSessionLost), errors.Is(err, errHubUnverified):
				hubError(w, http.StatusServiceUnavailable, "hub_unavailable")
			default:
				s.logger.Debug("inspector proxy request failed", "err", err)
				hubError(w, http.StatusBadGateway, "hub_unavailable")
			}
		},
		ErrorLog: s.proxyLog,
	}
}

// errBadUpgrade: the hub switched protocols for something that is not the
// WebSocket upgrade the request asked for. Returning it from
// ModifyResponse closes the hub's connection.
var errBadUpgrade = errors.New("inspector: the hub switched to a protocol that was not asked for")

// webSocketAnswer reports whether a 101 from the hub answers a WebSocket
// upgrade of the request with a WebSocket upgrade.
func webSocketAnswer(resp *http.Response) bool {
	return resp.Request != nil && isWebSocket(resp.Request) && headerHasToken(resp.Header, "Connection", "upgrade") &&
		strings.EqualFold(strings.TrimSpace(resp.Header.Get("Upgrade")), "websocket") && len(resp.Header.Values("Upgrade")) == 1
}

// retryTransport sends a request only to a hub port Docker confirmed
// within hubCheckInterval (the session's hub cookie never goes elsewhere,
// and neither does a browser's request for the Inspector's own pages). It
// logs in again, once, when the hub no longer knows the session's hub
// session (the hub restarted), and repeats the request with the new
// session. When that fails the request fails (503): the hub's "not signed
// in" answer never reaches the browser.
type retryTransport struct {
	s    *Service
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	info := proxyInfoFrom(req.Context())
	if !t.s.freshHub(req.Context(), info.view) {
		return nil, errHubUnverified
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, t.s.hubUnreachable(req.Context(), info.view.port, err)
	}
	if info.sess == nil {
		return resp, nil
	}
	lost, err := hubSessionLost(req, resp)
	if err != nil {
		return nil, err
	}
	if !lost {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return nil, errHubSessionLost
	}
	old := ""
	if ck, err := req.Cookie(hubCookie); err == nil {
		old = ck.Value
	}
	sid, ok := t.s.relogin(req.Context(), info.sess, old, info.view)
	if !ok {
		return nil, errHubSessionLost
	}
	again := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		again.Body = body
	}
	again.Header.Set("Cookie", hubCookie+"="+sid)
	resp, err = t.base.RoundTrip(again)
	if err != nil {
		return nil, t.s.hubUnreachable(req.Context(), info.view.port, err)
	}
	if lost, err := hubSessionLost(again, resp); err != nil || lost {
		if lost {
			_ = resp.Body.Close()
			return nil, errHubSessionLost
		}
		return nil, err
	}
	return resp, nil
}

// hubSessionLost reports whether the hub answered as if the request had no
// session: 401, or {"authenticated":false} from /auth/session. An
// /auth/session answer too large to check is errHubAnswer (a 502).
func hubSessionLost(req *http.Request, resp *http.Response) (bool, error) {
	if resp.StatusCode == http.StatusUnauthorized {
		return true, nil
	}
	if req.URL.Path != "/auth/session" || resp.StatusCode != http.StatusOK {
		return false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSessionAnswer+1))
	_ = resp.Body.Close()
	if err != nil {
		return false, err
	}
	if len(raw) > maxSessionAnswer {
		return false, errHubAnswer
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var v struct {
		Authenticated *bool `json:"authenticated"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Authenticated != nil && !*v.Authenticated {
		return true, nil
	}
	return false, nil
}

// relogin replaces the session's hub session (at most once per
// reloginInterval; never while hub logins are paused) at the hub of the
// request's snapshot v. It returns the current hub session when another
// request already replaced old.
func (s *Service) relogin(ctx context.Context, sess *session, old string, v hubView) (string, bool) {
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
	sid, err := s.hubLogin(lctx, v)
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

// guardKey names a hub generation for the login guard: the hub container
// and the secrets' generation.
func guardKey(v hubView) string {
	return v.container + "/" + strconv.FormatInt(v.sec.Generation, 10)
}

// hubLogin logs in at the hub of snapshot v with the daemon's password.
// Logins run one at a time; none is tried while the guard holds logins at
// that hub generation, and the password is sent only after Docker
// confirmed that the container runs and publishes exactly v.port.
func (s *Service) hubLogin(ctx context.Context, v hubView) (string, error) {
	if v.sec == nil || v.port == 0 || v.container == "" {
		return "", errHubLogin
	}
	select {
	case s.loginSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.loginSem }()
	key := guardKey(v)
	if held, cause := s.guard.blocked(key, s.cfg.Now()); held {
		return "", fmt.Errorf("%w: %w", errHubBlocked, cause)
	}
	if err := s.verifyHub(ctx, v.container, v.port); err != nil {
		return "", fmt.Errorf("%w: %w", errHubLogin, err)
	}
	if s.hooks.beforeHubLogin != nil {
		s.hooks.beforeHubLogin()
	}
	sid, err := s.hub.login(ctx, v.port, v.sec.Password)
	if err != nil && isTransport(err) {
		_ = s.hubUnreachable(ctx, v.port, err)
	}
	s.guard.record(key, s.cfg.Now(), err)
	if errors.Is(err, errHubCredentials) || errors.Is(err, errHubRateLimited) {
		s.logger.Warn("the Inspector hub refused the daemon's login; logins pause until the hub or its secrets change, or its limit ends", "err", err)
	}
	return sid, err
}

// logoutHub ends a hub session in the background (best effort).
func (s *Service) logoutHub(sid string) {
	if sid == "" {
		return
	}
	v := s.hubNow()
	if v.port == 0 {
		return
	}
	s.goBG(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.logoutAt(ctx, v, sid)
	})
}

// logoutAt ends hub session sid at the hub of snapshot v, only while Docker
// confirmed v's port within hubCheckInterval (renewed first when older).
func (s *Service) logoutAt(ctx context.Context, v hubView, sid string) {
	if !s.freshHub(ctx, v) {
		return
	}
	if err := s.hub.logout(ctx, v.port, sid); err != nil {
		_ = s.hubUnreachable(ctx, v.port, err)
	}
}

// endSession ends a session: its streams close and its hub session is
// logged out in the background.
func (s *Service) endSession(sess *session) {
	s.sessions.remove(sess)
	s.logoutHub(sess.end())
}
