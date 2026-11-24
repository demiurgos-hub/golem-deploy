package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/certs"
)

type ctxKey int

const (
	reqInfoKey ctxKey = iota
	identityKey
	routeAccessKey
)

// reqInfo is shared, mutable per-request state for the access log: the auth
// layer fills in the account id. clientIP is resolved once by outer.
type reqInfo struct {
	id        string
	accountID int64
	clientIP  string
	// clientGone is set when the request failed only because the client
	// cancelled it (see writeError); the access log then reports 499.
	clientGone bool
}

// StatusClientClosedRequest is the access-log status of a request the
// client cancelled before a response could be written (nginx's 499). It is
// never sent: there is nobody left to send it to.
const StatusClientClosedRequest = 499

// routeAccessFrom returns the access level of the route serving r.
func routeAccessFrom(r *http.Request) (Access, bool) {
	a, ok := r.Context().Value(routeAccessKey).(Access)
	return a, ok
}

func requestIDFrom(ctx context.Context) string {
	if ri, ok := ctx.Value(reqInfoKey).(*reqInfo); ok {
		return ri.id
	}
	return ""
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusWriter records the status and size of a response. It implements
// Unwrap so http.ResponseController can reach the underlying writer for
// Flush and deadline control.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush supports handlers that type-assert http.Flusher.
func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// outer is the middleware applied to every request: request id, client
// address resolution, access log, panic recovery, security headers and the
// Host allowlist.
func (s *Server) outer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Captured up front: downstream handlers (the SPA fallback) may
		// rewrite r.URL.
		method, path := r.Method, r.URL.Path
		ri := &reqInfo{id: newRequestID(), clientIP: s.resolveClientIP(r)}
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey, ri))
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set("X-Request-Id", ri.id)
		s.setSecurityHeaders(sw.Header(), isAPIPath(r.URL.Path))

		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.logger.Error("panic in handler", "request_id", ri.id, "panic", v, "stack", string(debug.Stack()))
				if sw.status == 0 {
					s.writeError(sw, r, internal(errors.New("panic")))
				}
			}
			status := sw.status
			level := slog.LevelInfo
			switch {
			case ri.clientGone:
				status, level = StatusClientClosedRequest, slog.LevelDebug
			case status == 0 && r.Context().Err() != nil:
				// The handler noticed the client leaving and wrote nothing.
				status, level = StatusClientClosedRequest, slog.LevelDebug
			case status == 0:
				status = http.StatusOK
			}
			attrs := []any{
				"method", method,
				"path", path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", sw.written,
				"request_id", ri.id,
				"remote", ri.clientIP,
			}
			if ri.accountID != 0 {
				attrs = append(attrs, "account_id", ri.accountID)
			}
			s.logger.Log(context.WithoutCancel(r.Context()), level, "http request", attrs...)
		}()
		if !s.hostAllowed(r.Host) {
			s.logBadHost(r, ri.id)
			s.writeError(sw, r, errBadHost)
			return
		}
		next.ServeHTTP(sw, r)
	})
}

const (
	cspSPA = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	cspAPI = "default-src 'none'; frame-ancestors 'none'"
)

func (s *Server) setSecurityHeaders(h http.Header, api bool) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if api {
		h.Set("Content-Security-Policy", cspAPI)
		h.Set("Cache-Control", "no-store")
	} else {
		h.Set("Content-Security-Policy", cspSPA)
	}
	if s.cfg.tls() {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func isAPIPath(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }

// Game ports can be assigned while a page is open. Allow connections to the
// effective game host only, with no change to the other CSP directives.
func (s *Server) allowGameConnections(h http.Header, ctx context.Context) {
	if s.servers == nil {
		return
	}
	raw, err := s.servers.GameHost(ctx)
	if err != nil {
		return
	}
	host, err := certs.ValidateHost(raw)
	if err != nil || host == "" || strings.Contains(host, ":") {
		// CSP host source expressions cannot express IPv6 literals.
		return
	}
	connect := "connect-src 'self' https://" + host + ":* wss://" + host + ":*"
	h.Set("Content-Security-Policy", strings.Replace(cspSPA, "connect-src 'self'", connect, 1))
}

// clientIP returns the client address key used for rate limiting and the
// access log: an IPv4 address or an IPv6 /64. It is the TCP peer, or, when
// the peer is a trusted proxy (--trusted-proxy), the client named by
// X-Forwarded-For (see resolveClientIP).
func clientIP(r *http.Request) string {
	if ri, ok := r.Context().Value(reqInfoKey).(*reqInfo); ok && ri.clientIP != "" {
		return ri.clientIP
	}
	return peerKey(r.RemoteAddr)
}
