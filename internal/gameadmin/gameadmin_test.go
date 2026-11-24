package gameadmin_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
)

const token = "0123456789abcdef0123456789abcdef"

func client(t *testing.T, port int) *gameadmin.Client {
	t.Helper()
	c, err := gameadmin.New(port, token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestContract(t *testing.T) {
	ctx := context.Background()
	s := gameadmintest.New(t)
	s.Set(func(s *gameadmintest.Server) { s.NotReadyFor = 1; s.ExpectToken = func() string { return token } })
	c := client(t, s.Port)
	r, err := c.Ready(ctx)
	if err != nil || r.Ready || r.Reason != "starting" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := c.Ready(ctx); err != nil || !r.Ready {
		t.Fatalf("%+v %v", r, err)
	}
	info, err := c.Info(ctx)
	if err != nil || info.SchemaFingerprint != "sf-0123456789abcdef" || info.EngineVersion != "v0.4.0-rc.1" || info.Transport != "webtransport" ||
		info.TickRate != 20 || len(info.WebTransportCertificateHashes) != 1 || !info.CertificateHashesPublished {
		t.Fatalf("%+v %v", info, err)
	}
	st, err := c.Stats(ctx)
	if err != nil || st.Sessions != 3 || st.Entities != 42 || st.Tick != 250 || st.TickRate != 20 || st.MaxMs != 3.5 {
		t.Fatalf("%+v %v", st, err)
	}
	auth, paths := s.Requests()
	for _, a := range auth {
		if a != "Bearer "+token {
			t.Fatalf("auth header %q", a)
		}
	}
	if strings.Join(paths, ",") != "/ready,/ready,/info,/stats" {
		t.Fatalf("paths %v", paths)
	}
	// A wrong token is refused by the game.
	bad, _ := gameadmin.New(s.Port, "ffffffffffffffffffff")
	if _, err := bad.Ready(ctx); !errors.Is(err, gameadmin.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestHostileAnswersAreContained(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{gameadmintest.Huge, gameadmintest.Redirect, gameadmintest.HTML, gameadmintest.Gzip} {
		s := gameadmintest.New(t)
		s.Set(func(s *gameadmintest.Server) { s.Mode = mode })
		c := client(t, s.Port)
		if _, err := c.Ready(ctx); !errors.Is(err, gameadmin.ErrBadResponse) {
			t.Errorf("%s ready: %v", mode, err)
		}
		if _, err := c.Info(ctx); !errors.Is(err, gameadmin.ErrBadResponse) {
			t.Errorf("%s info: %v", mode, err)
		}
		if _, paths := s.Requests(); len(paths) != 2 {
			t.Errorf("%s: redirect followed? %v", mode, paths)
		}
	}

	s := gameadmintest.New(t)
	s.Set(func(s *gameadmintest.Server) { s.Mode = gameadmintest.Control; s.NeverReady = true })
	c := client(t, s.Port)
	r, err := c.Ready(ctx)
	if err != nil || r.Ready {
		t.Fatalf("%+v %v", r, err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{r.Reason, info.Name, info.EngineVersion, info.GoVersion} {
		if len(v) > gameadmin.MaxString {
			t.Errorf("unbounded string (%d bytes)", len(v))
		}
		for _, ch := range v {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) || ch == '\u2028' {
				t.Errorf("control character %U kept in %q", ch, v)
			}
		}
	}
	if info.SchemaFingerprint != "" || info.Transport != "unknown" || info.TickRate != 0 {
		t.Fatalf("%+v", info)
	}
	if st.Sessions != 0 || st.Entities != 1<<31 || st.UptimeSeconds != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestUnreachableAndSlow(t *testing.T) {
	ctx := context.Background()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	if _, err := client(t, port).Ready(ctx); !errors.Is(err, gameadmin.ErrUnreachable) {
		t.Fatalf("got %v", err)
	}
	// A game that never answers is bounded by the request timeout.
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(block)
	_, p, _ := net.SplitHostPort(slow.Listener.Addr().String())
	sp, _ := strconv.Atoi(p)
	start := time.Now()
	_, err = client(t, sp).Ready(ctx)
	if !errors.Is(err, gameadmin.ErrUnreachable) || time.Since(start) > gameadmin.RequestTimeout+2*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("token in error")
	}
}

func TestNewRejects(t *testing.T) {
	for _, tc := range []struct {
		port  int
		token string
	}{{0, token}, {70000, token}, {8080, "short"}, {8080, token + " x"}, {8080, token + "\n"}} {
		if _, err := gameadmin.New(tc.port, tc.token); err == nil {
			t.Errorf("%+v accepted", tc)
		}
	}
	if gameadmin.Clean(" a\x1b[1mb\u202ec\u200b ", 10) != "a[1mbc" {
		t.Fatalf("%q", gameadmin.Clean(" a\x1b[1mb\u202ec\u200b ", 10))
	}
	if got := gameadmin.Clean(strings.Repeat("é", 100), 11); got != strings.Repeat("é", 5) {
		t.Fatalf("%q", got)
	}
}

// POST /snapshot: success, the engine's error statuses, and answers
// outside the contract.
func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	s := gameadmintest.New(t)
	wrote := 0
	s.Set(func(s *gameadmintest.Server) {
		s.ExpectToken = func() string { return token }
		s.OnSnapshot = func() { wrote++ }
	})
	c := client(t, s.Port)
	if d, err := c.Snapshot(ctx); err != nil || d != 12*time.Millisecond || wrote != 1 {
		t.Fatalf("%v %v %d", d, err, wrote)
	}
	for status, want := range map[int]error{
		http.StatusConflict:            gameadmin.ErrSnapshotInProgress,
		http.StatusNotImplemented:      gameadmin.ErrSnapshotUnsupported,
		http.StatusGatewayTimeout:      gameadmin.ErrSnapshotTimeout,
		http.StatusInternalServerError: gameadmin.ErrSnapshotFailed,
		http.StatusServiceUnavailable:  gameadmin.ErrNotReady,
	} {
		s.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = status })
		if _, err := c.Snapshot(ctx); !errors.Is(err, want) {
			t.Errorf("%d: %v", status, err)
		}
	}
	s.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 0; s.SnapshotBusyFor = 1 })
	if _, err := c.Snapshot(ctx); !errors.Is(err, gameadmin.ErrSnapshotInProgress) {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	_, paths := s.Requests()
	if paths[len(paths)-1] != "/snapshot" {
		t.Fatalf("%v", paths)
	}
	// Outside the contract: other statuses, other content, a wrong token.
	for name, h := range map[string]http.HandlerFunc{
		"teapot": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(`{}`))
		},
		"html": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
		"not ok": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":false}`))
		},
		"get": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.ContentLength > 0 {
				w.WriteHeader(http.StatusTeapot)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"duration_ms":1e300}`))
		},
	} {
		srv := httptest.NewServer(h)
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		p, _ := strconv.Atoi(port)
		d, err := client(t, p).Snapshot(ctx)
		srv.Close()
		switch name {
		case "get":
			if err != nil || d != time.Hour {
				t.Errorf("%s: %v %v", name, d, err)
			}
		default:
			if !errors.Is(err, gameadmin.ErrBadResponse) {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	bad, _ := gameadmin.New(s.Port, "ffffffffffffffffffffffffffffffff")
	if _, err := bad.Snapshot(ctx); !errors.Is(err, gameadmin.ErrUnauthorized) {
		t.Fatal(err)
	}
}
