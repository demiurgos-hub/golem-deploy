package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const (
	reqInfoKey ctxKey = iota
	identityKey
)

// reqInfo is shared, mutable per-request state for the access log: the auth
// layer fills in the account id.
type reqInfo struct {
	id        string
	accountID int64
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

// outer is the middleware applied to every request: request id, access log,
// panic recovery and security headers.
func (s *Server) outer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Captured up front: downstream handlers (the SPA fallback) may
		// rewrite r.URL.
		method, path := r.Method, r.URL.Path
		ri := &reqInfo{id: newRequestID()}
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
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"method", method,
				"path", path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", sw.written,
				"request_id", ri.id,
				"remote", clientIP(r),
			}
			if ri.accountID != 0 {
				attrs = append(attrs, "account_id", ri.accountID)
			}
			s.logger.Info("http request", attrs...)
		}()
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

// clientIP returns the TCP peer address used for rate limiting. Forwarded
// headers are deliberately ignored. IPv6 peers are grouped by /64 because
// one host commonly controls a whole /64.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
