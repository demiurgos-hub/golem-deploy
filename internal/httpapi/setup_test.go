package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// setupToken stores a fresh setup token valid for ttl and returns it.
func (h *harness) setupToken(ttl time.Duration) string {
	h.t.Helper()
	tok, hash, err := auth.NewSetupToken()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetSetupToken(context.Background(), hash, h.clock.Now().Add(ttl)); err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func setup(h *harness, token, username string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/v1/setup", "", apitypes.SetupRequest{Token: token, Username: username, Password: testPassword})
}

func authOptions(t *testing.T, h *harness) apitypes.AuthOptions {
	t.Helper()
	rr := h.do("GET", "/api/v1/auth/options", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("options: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.AuthOptions](t, rr)
}

func TestSetupCreatesTheFirstAdmin(t *testing.T) {
	h := newHarness(t)
	var logs bytes.Buffer
	h.srv.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// Accounts that are not approved admins do not end the setup.
	h.account("pending", store.RoleAdmin, false)
	h.account("dev", store.RoleDeveloper, true)
	if !authOptions(t, h).SetupRequired {
		t.Fatal("setup not required on a fresh installation")
	}
	tok := h.setupToken(time.Hour)

	rr := setup(h, tok, "Root")
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body.String())
	}
	resp := decode[apitypes.SessionResponse](t, rr)
	if resp.Account.Username != "root" || resp.Account.Role != apitypes.RoleAdmin || !resp.Account.Approved || resp.Token == "" {
		t.Fatalf("setup response %+v", resp)
	}
	if rr := h.do("GET", "/api/v1/accounts", resp.Token, nil); rr.Code != http.StatusOK {
		t.Fatalf("the new session is not an admin session: %d", rr.Code)
	}
	mustLogin(t, h, "root")
	if authOptions(t, h).SetupRequired {
		t.Fatal("setup still required")
	}
	// The token works once.
	errBody(t, setup(h, tok, "second"), http.StatusForbidden, apitypes.ErrorCodeSetupLinkInvalid)

	out := logs.String()
	if !strings.Contains(out, "first admin account created with the setup link") || !strings.Contains(out, "username=root") || !strings.Contains(out, "client="+clientIPForTests) {
		t.Fatalf("missing success log line:\n%s", out)
	}
	if strings.Contains(out, tok) || strings.Contains(out, auth.HashSetupToken(tok)) {
		t.Fatalf("the token reached the log:\n%s", out)
	}
	if strings.Contains(rr.Body.String(), tok) {
		t.Fatal("the token is in a response")
	}
}

// Every refused token gets the same answer, and no account is created.
func TestSetupRefusals(t *testing.T) {
	h := newHarness(t)
	tok := h.setupToken(time.Minute)
	strip := func(rr *httptest.ResponseRecorder) string {
		b := rr.Body.String()
		return b[:strings.Index(b, `"request_id"`)]
	}
	var bodies []string
	check := func(name string, rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		bodies = append(bodies, strip(rr))
		errBody(t, rr, http.StatusForbidden, apitypes.ErrorCodeSetupLinkInvalid)
		h.srv.setupIP.Reset(clientIPForTests)
	}
	check("missing", setup(h, "", "root"))
	check("wrong", setup(h, tok+"x", "root"))
	check("no field", h.do("POST", "/api/v1/setup", "", `{"username":"root","password":"`+testPassword+`"}`))
	// A wrong token is refused before the username and password are looked at.
	check("wrong with bad fields", h.do("POST", "/api/v1/setup", "", apitypes.SetupRequest{Token: "nope", Username: "!", Password: "x"}))
	h.clock.Add(time.Minute)
	check("expired", setup(h, tok, "root"))

	tok = h.setupToken(time.Hour)
	h.account("admin1", store.RoleAdmin, true) // deletes the token
	check("admin created meanwhile", setup(h, tok, "root"))
	for i, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("case %d body differs:\n%s\n%s", i, b, bodies[0])
		}
	}
	c, err := h.st.CountAccounts(context.Background())
	if err != nil || c.Total != 1 {
		t.Fatalf("accounts %+v %v", c, err)
	}
}

func TestSetupValidationKeepsTheToken(t *testing.T) {
	h := newHarness(t)
	tok := h.setupToken(time.Hour)
	for i := 0; i < 2*loginFailuresPerWindow; i++ {
		e := errBody(t, h.do("POST", "/api/v1/setup", "", apitypes.SetupRequest{Token: tok, Username: "a!", Password: testPassword}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
		if e.Field != "username" {
			t.Fatalf("field %q", e.Field)
		}
	}
	e := errBody(t, h.do("POST", "/api/v1/setup", "", apitypes.SetupRequest{Token: tok, Username: "root", Password: "short"}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "password" {
		t.Fatalf("field %q", e.Field)
	}
	if n := h.srv.setupIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("validation errors with a valid token were counted: %d", n)
	}
	if rr := setup(h, tok, "root"); rr.Code != http.StatusCreated {
		t.Fatalf("setup after validation errors: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSetupRateLimit(t *testing.T) {
	h := newHarness(t)
	tok := h.setupToken(time.Hour)
	for i := 0; i < loginFailuresPerWindow; i++ {
		errBody(t, setup(h, "wrong-token", "root"), http.StatusForbidden, apitypes.ErrorCodeSetupLinkInvalid)
	}
	// Even the right token is refused while limited, the way login is.
	rr := setup(h, tok, "root")
	errBody(t, rr, http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Setup failures do not count against login.
	if n := h.srv.loginIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("setup failures counted against login: %d", n)
	}
	if r := login(h, "nobody", testPassword); r.code != http.StatusUnauthorized {
		t.Fatalf("login after setup failures: %d", r.code)
	}
	h.clock.Add(loginWindow)
	if rr := setup(h, tok, "root"); rr.Code != http.StatusCreated {
		t.Fatalf("setup after the window: %d %s", rr.Code, rr.Body.String())
	}
	if n := h.srv.setupIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("a successful setup was counted: %d", n)
	}
}

// Login failures do not count against setup, and setup keeps its own
// counter after the first admin exists.
func TestSetupAndLoginLimitsAreSeparate(t *testing.T) {
	h := newHarness(t)
	tok := h.setupToken(time.Hour)
	for i := 0; i < loginFailuresPerWindow; i++ {
		if r := login(h, "nobody", testPassword); r.code != http.StatusUnauthorized {
			t.Fatalf("login %d: %d", i, r.code)
		}
	}
	if r := login(h, "nobody", testPassword); r.code != http.StatusTooManyRequests {
		t.Fatalf("login over the limit: %d", r.code)
	}
	if n := h.srv.setupIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("login failures counted against setup: %d", n)
	}
	if rr := setup(h, tok, "root"); rr.Code != http.StatusCreated {
		t.Fatalf("setup while login is limited: %d %s", rr.Code, rr.Body.String())
	}

	// Old links after setup use up only the setup counter.
	h.clock.Add(loginWindow)
	for i := 0; i < loginFailuresPerWindow+1; i++ {
		setup(h, tok, "other")
	}
	if n := h.srv.loginIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("setup requests after setup counted against login: %d", n)
	}
	if r := login(h, "root", testPassword); r.code != http.StatusOK {
		t.Fatalf("login after old setup links: %d", r.code)
	}
}

// A wrong token never reaches the password hashing: with every bcrypt slot
// taken it is still answered 403 at once, while a valid token waits for a
// slot (503) and is not used up.
func TestSetupWrongTokenDoesNotHash(t *testing.T) {
	h := newHarness(t)
	tok := h.setupToken(time.Hour)
	h.srv.hashSlots = auth.NewSlots(1, 20*time.Millisecond)
	hold, err := h.srv.hashSlots.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	errBody(t, setup(h, "wrong-token", "root"), http.StatusForbidden, apitypes.ErrorCodeSetupLinkInvalid)
	errBody(t, setup(h, tok, "root"), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	hold()
	if rr := setup(h, tok, "root"); rr.Code != http.StatusCreated {
		t.Fatalf("setup once a slot is free: %d %s", rr.Code, rr.Body.String())
	}
}

func TestSetupConcurrentUseCreatesOneAccount(t *testing.T) {
	h := newHarness(t)
	tok := h.setupToken(time.Hour)
	// Fewer requests than the per-client limit, which every request
	// reserves before its token is checked.
	const n = loginFailuresPerWindow - 1
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = setup(h, tok, "root"+string(rune('a'+i))).Code
		}()
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusForbidden:
		default:
			t.Fatalf("unexpected status %d in %v", c, codes)
		}
	}
	c, err := h.st.CountAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 || c.Total != 1 || c.ApprovedAdmins != 1 {
		t.Fatalf("created %d, accounts %+v (codes %v)", created, c, codes)
	}
}
