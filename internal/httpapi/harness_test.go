package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

const testPassword = "correct-horse-battery"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeDocker bool

func (f fakeDocker) Reachable(context.Context) bool { return bool(f) }

type harness struct {
	t        *testing.T
	srv      *Server
	st       *store.Store
	sessions *auth.Sessions
	hasher   *auth.Hasher
	runner   *jobs.Runner
	clock    *fakeClock
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	clock := &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"), store.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessions(bytes.Repeat([]byte{9}, auth.JWTKeySize), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	hasher, err := auth.NewHasher(bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := jobs.New(st, logDir, jobs.Options{Workers: 2})
	cfg := Config{Listen: "127.0.0.1:0", Version: "test"}
	for _, m := range mutate {
		m(&cfg)
	}
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>spa</html>")
	})
	srv, err := New(cfg, Deps{Store: st, Sessions: sessions, Hasher: hasher, Secrets: box, Jobs: runner, Docker: fakeDocker(true), SPA: spa, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
		_ = st.Close()
	})
	return &harness{t: t, srv: srv, st: st, sessions: sessions, hasher: hasher, runner: runner, clock: clock}
}

func (h *harness) account(name string, role store.Role, approved bool) store.Account {
	h.t.Helper()
	hash, err := h.hasher.Hash(testPassword)
	if err != nil {
		h.t.Fatal(err)
	}
	a, err := h.st.CreateAccount(context.Background(), store.NewAccount{Username: name, PasswordHash: hash, Role: role, Approved: approved})
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// session issues a token for the account's current token_version.
func (h *harness) session(a store.Account) string {
	h.t.Helper()
	cur, err := h.st.GetAccount(context.Background(), a.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	tok, _, err := h.sessions.Issue(cur.ID, cur.TokenVersion, h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) apiToken(a store.Account, expires *time.Time) string {
	h.t.Helper()
	secret, hash, prefix, err := auth.NewAPIToken()
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.st.CreateAPIToken(context.Background(), store.NewAPIToken{AccountID: a.ID, Name: "t", TokenHash: hash, Prefix: prefix, ExpiresAt: expires}); err != nil {
		h.t.Fatal(err)
	}
	return secret
}

// do sends a request. body may be nil, a string (sent raw as JSON) or a
// value (marshalled).
func (h *harness) do(method, path, token string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "192.0.2.10:5555"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rr, req)
	return rr
}

func decode[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(rr.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T from %d response: %v", v, rr.Code, err)
	}
	return v
}

// errBody asserts the response is an error envelope with status and code.
func errBody(t *testing.T, rr *httptest.ResponseRecorder, status int, code apitypes.ErrorCode) apitypes.ErrorBody {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status %d, want %d; body %s", rr.Code, status, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	e := decode[apitypes.ErrorResponse](t, rr).Error
	if e.Code != code {
		t.Fatalf("code %q, want %q (message %q)", e.Code, code, e.Message)
	}
	if e.RequestID == "" || e.RequestID != rr.Header().Get("X-Request-Id") {
		t.Fatalf("request id %q vs header %q", e.RequestID, rr.Header().Get("X-Request-Id"))
	}
	return e
}
