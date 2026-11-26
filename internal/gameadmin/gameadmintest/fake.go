// Package gameadmintest is a fake Golem Engine admin endpoint for tests:
// an httptest server on 127.0.0.1 that answers /ready, /info, /stats,
// POST /snapshot and GET /profile/{kind} like golem/host, and can misbehave
// like a hostile game.
package gameadmintest

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Behaviours of a hostile game.
const (
	Normal = ""
	// Huge answers every request with a body larger than the client cap.
	Huge = "huge"
	// Control answers with control characters, bidi overrides and
	// over-long strings in every string field and absurd numbers.
	Control = "control"
	// Redirect answers 302 to another host.
	Redirect = "redirect"
	// HTML answers 200 with a non-JSON content type.
	HTML = "html"
	// Gzip answers with a gzip Content-Encoding.
	Gzip = "gzip"
)

// Server is the fake endpoint. Fields may be changed while it runs
// (under Lock/Unlock).
type Server struct {
	*httptest.Server
	Port int

	mu sync.Mutex
	// ExpectToken, when set, returns the token requests must carry;
	// otherwise any bearer token is accepted.
	ExpectToken func() string
	// NotReadyFor is the number of /ready calls answered 503 "starting"
	// before the game is ready; NeverReady keeps answering 503.
	NotReadyFor int
	NeverReady  bool
	Mode        string
	// Fingerprint, Sessions and Entities are reported by /info and /stats.
	Fingerprint string
	Sessions    int
	Entities    int
	// SnapshotStatus is the status of POST /snapshot (0: 200);
	// SnapshotBusyFor answers that many snapshots 409 first. OnSnapshot,
	// when set, runs (without the lock) before a 200 answer, e.g. to write
	// the snapshot into a fake volume. Snapshots counts the requests.
	SnapshotStatus  int
	SnapshotBusyFor int
	OnSnapshot      func()
	Snapshots       int
	// OnBusy, when set, runs (without the lock) before a 409 answer.
	OnBusy func()
	// ProfileStatus is the status of GET /profile/{kind} (0: 200 with
	// ProfileBody, by default a few gzip bytes, and ProfileContentType, by
	// default application/octet-stream). OnProfile, when set, runs (without
	// the lock) before the answer, e.g. to hold a capture open.
	ProfileStatus      int
	ProfileBody        []byte
	ProfileContentType string
	OnProfile          func()
	// Auth records the Authorization header of every request.
	Auth  []string
	Paths []string
}

// New starts a fake endpoint, closed when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Fingerprint: "sf-0123456789abcdef", Sessions: 3, Entities: 42}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	s.Port, _ = strconv.Atoi(port)
	return s
}

// Set changes fields under the lock.
func (s *Server) Set(f func(s *Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Requests returns the recorded Authorization headers and paths.
func (s *Server) Requests() (auth, paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Auth...), append([]string(nil), s.Paths...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/snapshot" {
		s.snapshot(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/profile/") {
		s.profile(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Auth = append(s.Auth, r.Header.Get("Authorization"))
	s.Paths = append(s.Paths, r.URL.Path)
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if scheme != "Bearer" || tok == "" || (s.ExpectToken != nil && tok != s.ExpectToken()) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch s.Mode {
	case Huge:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true,"name":"` + strings.Repeat("A", 1<<20) + `"}`))
		return
	case Redirect:
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		return
	case HTML:
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`{"ready":true}`))
		return
	case Gzip:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte{0x1f, 0x8b})
		return
	}
	evil := "\x1b[31mevil\u202e\u2028\x00" + strings.Repeat("x", 500)
	switch r.URL.Path {
	case "/ready":
		if s.NeverReady || s.NotReadyFor > 0 {
			s.NotReadyFor--
			reason := "starting"
			if s.Mode == Control {
				reason = evil
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": reason})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ready": true})
	case "/info":
		info := map[string]any{
			"name": "fake-game", "schema_fingerprint": s.Fingerprint, "engine_version": "v0.4.0-rc.1", "go_version": "go1.26.0",
			"transport": "webtransport", "tick_rate": 20, "started_at": "2026-11-24T10:00:00Z", "certificate_hashes_published": true,
			"webtransport_certificate_hashes": []map[string]string{{"algorithm": "sha-256", "value": strings.Repeat("ab", 32)}},
			"http_certificate_hashes":         []map[string]string{}, "extra": map[string]string{},
		}
		if s.Mode == Control {
			for _, k := range []string{"name", "schema_fingerprint", "engine_version", "go_version", "transport"} {
				info[k] = evil
			}
			info["tick_rate"] = -5
		}
		writeJSON(w, http.StatusOK, info)
	case "/stats":
		stats := map[string]any{
			"uptime_seconds": 12.5, "sessions": s.Sessions, "entities": s.Entities,
			"tick": map[string]any{"tick": 250, "rate": 20, "budget_ms": 50, "last_ms": 1.25, "max_ms": 3.5, "overruns": 0},
		}
		if s.Mode == Control {
			stats["sessions"], stats["entities"], stats["uptime_seconds"] = -1, 1e300, -3
		}
		writeJSON(w, http.StatusOK, stats)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Auth = append(s.Auth, r.Header.Get("Authorization"))
	s.Paths = append(s.Paths, r.URL.RequestURI())
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if scheme != "Bearer" || tok == "" || (s.ExpectToken != nil && tok != s.ExpectToken()) {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	status, body, ct, hook := s.ProfileStatus, s.ProfileBody, s.ProfileContentType, s.OnProfile
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	codes := map[int]string{http.StatusNotFound: "profiling_disabled", http.StatusConflict: "profile_in_progress",
		http.StatusBadRequest: "invalid_profile", http.StatusServiceUnavailable: "not_ready"}
	if status != 0 && status != http.StatusOK {
		writeJSON(w, status, map[string]string{"error": codes[status], "message": "fake " + codes[status]})
		return
	}
	if body == nil {
		body = []byte{0x1f, 0x8b, 0x08, 0x00, 'p', 'r', 'o', 'f'}
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="profile"`)
	_, _ = w.Write(body)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Auth = append(s.Auth, r.Header.Get("Authorization"))
	s.Paths = append(s.Paths, r.URL.Path)
	s.Snapshots++
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if scheme != "Bearer" || tok == "" || (s.ExpectToken != nil && tok != s.ExpectToken()) {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		s.mu.Unlock()
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed", "message": "use POST"})
		return
	}
	status, hook, busy := s.SnapshotStatus, s.OnSnapshot, s.OnBusy
	if s.SnapshotBusyFor > 0 {
		s.SnapshotBusyFor--
		status = http.StatusConflict
	}
	s.mu.Unlock()
	if status == http.StatusConflict && busy != nil {
		busy()
	}
	codes := map[int]string{http.StatusConflict: "snapshot_in_progress", http.StatusNotImplemented: "not_implemented",
		http.StatusGatewayTimeout: "snapshot_timeout", http.StatusInternalServerError: "snapshot_failed", http.StatusServiceUnavailable: "not_ready"}
	if status != 0 && status != http.StatusOK {
		writeJSON(w, status, map[string]string{"code": codes[status], "message": "fake " + codes[status]})
		return
	}
	if hook != nil {
		hook()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duration_ms": 12})
}
