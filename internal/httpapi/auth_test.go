package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func login(h *harness, username, password string) *httpResult {
	rr := h.do("POST", "/api/v1/auth/login", "", apitypes.LoginRequest{Username: username, Password: password})
	return &httpResult{code: rr.Code, body: rr.Body.String(), header: rr.Header()}
}

type httpResult struct {
	code   int
	body   string
	header http.Header
}

func mustLogin(t *testing.T, h *harness, username string) apitypes.SessionResponse {
	t.Helper()
	rr := h.do("POST", "/api/v1/auth/login", "", apitypes.LoginRequest{Username: username, Password: testPassword})
	if rr.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", username, rr.Code, rr.Body.String())
	}
	return decode[apitypes.SessionResponse](t, rr)
}

func TestLoginSuccess(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleAdmin, true)
	resp := mustLogin(t, h, "ALICE")
	if resp.Token == "" || resp.Account.Username != "alice" || resp.Account.Role != apitypes.RoleAdmin {
		t.Fatalf("login response %+v", resp)
	}
	if !resp.ExpiresAt.Equal(h.clock.Now().Add(auth.SessionTTL)) || !resp.SessionExpiresAt.Equal(h.clock.Now().Add(auth.SessionMaxAge)) {
		t.Fatalf("expiry %v / %v", resp.ExpiresAt, resp.SessionExpiresAt)
	}
	if rr := h.do("GET", "/api/v1/account", resp.Token, nil); rr.Code != http.StatusOK {
		t.Fatalf("me: %d %s", rr.Code, rr.Body.String())
	}
}

// Finding 3: unknown user, wrong password and unapproved account must be
// indistinguishable.
func TestLoginResponsesIdenticalForUnknownWrongUnapproved(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	h.account("pending", store.RoleDeveloper, false)

	results := []*httpResult{
		login(h, "nobody", testPassword),
		login(h, "alice", "wrong-password-123"),
		login(h, "pending", testPassword),
	}
	strip := func(r *httpResult) string {
		// Only the request id may differ.
		i := strings.Index(r.body, `"request_id"`)
		if i < 0 {
			t.Fatalf("no request id in %s", r.body)
		}
		return r.body[:i]
	}
	for i, r := range results {
		if r.code != http.StatusUnauthorized {
			t.Fatalf("case %d: status %d", i, r.code)
		}
		if strip(r) != strip(results[0]) {
			t.Fatalf("case %d body differs:\n%s\n%s", i, r.body, results[0].body)
		}
		if !strings.Contains(r.body, string(apitypes.ErrorCodeInvalidCredentials)) {
			t.Fatalf("case %d: %s", i, r.body)
		}
	}
	// Over-long passwords are a normal failure, not a 500.
	if r := login(h, "alice", strings.Repeat("x", 200)); r.code != http.StatusUnauthorized {
		t.Fatalf("long password: %d %s", r.code, r.body)
	}
}

func TestLoginRateLimitTripsAndRecovers(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	for i := 0; i < loginFailuresPerWindow; i++ {
		if r := login(h, "alice", "wrong-password-123"); r.code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.code)
		}
	}
	// Even the right password is refused while limited.
	rr := h.do("POST", "/api/v1/auth/login", "", apitypes.LoginRequest{Username: "alice", Password: testPassword})
	errBody(t, rr, http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	ra, err := strconv.Atoi(rr.Header().Get("Retry-After"))
	if err != nil || ra <= 0 || ra > int(loginWindow.Seconds()) {
		t.Fatalf("Retry-After %q", rr.Header().Get("Retry-After"))
	}
	h.clock.Add(loginWindow)
	mustLogin(t, h, "alice")
}

func TestLoginSuccessResetsUsernameBucket(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	for i := 0; i < loginFailuresPerWindow-1; i++ {
		login(h, "alice", "wrong-password-123")
	}
	mustLogin(t, h, "alice")
	if ok, _ := h.srv.loginUser.Check("alice"); !ok || h.srv.loginUser.Len() != 0 {
		t.Fatal("username bucket not reset after success")
	}
}

func TestLoginRateLimitPerIPAcrossUsernames(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < loginFailuresPerWindow; i++ {
		login(h, "user"+strconv.Itoa(i), "wrong-password-123")
	}
	r := login(h, "fresh-user", "wrong-password-123")
	if r.code != http.StatusTooManyRequests {
		t.Fatalf("IP limit not applied: %d", r.code)
	}
}

func TestLoginValidation(t *testing.T) {
	h := newHarness(t)
	rr := h.do("POST", "/api/v1/auth/login", "", `{"username":"a"}`)
	if e := errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "password" {
		t.Fatalf("field %q", e.Field)
	}
}

// Finding 2: sessions are re-validated against the database on every
// request.
func TestDeletedUserSessionRejectedImmediately(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)
	tok := mustLogin(t, h, "dev1").Token
	if rr := h.do("GET", "/api/v1/status", tok, nil); rr.Code != http.StatusOK {
		t.Fatalf("before delete: %d", rr.Code)
	}
	if err := h.st.DeleteAccount(context.Background(), admin.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	rr := h.do("GET", "/api/v1/status", tok, nil)
	errBody(t, rr, http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
	errBody(t, h.do("POST", "/api/v1/auth/refresh", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

func TestUnapprovedUserSessionRejected(t *testing.T) {
	h := newHarness(t)
	h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)
	tok := h.session(dev)
	// Even with token_version unchanged, approval is checked.
	if _, err := h.st.UpdateAccount(context.Background(), 0, dev.ID, store.AccountUpdate{Approved: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("GET", "/api/v1/status", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	// Re-approval does not resurrect the old session (version was bumped).
	if _, err := h.st.UpdateAccount(context.Background(), 0, dev.ID, store.AccountUpdate{Approved: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("GET", "/api/v1/status", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	if _, err := h.st.UpdateAccount(context.Background(), 0, dev.ID, store.AccountUpdate{Approved: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("GET", "/api/v1/status", h.session(dev), nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

func ptr[T any](v T) *T { return &v }

func TestPasswordChangeInvalidatesOldSessions(t *testing.T) {
	h := newHarness(t)
	h.account("dev1", store.RoleDeveloper, true)
	s1 := mustLogin(t, h, "dev1").Token
	s2 := mustLogin(t, h, "dev1").Token

	rr := h.do("POST", "/api/v1/account/password", s1, apitypes.ChangePasswordRequest{CurrentPassword: "nope-nope-nope", NewPassword: "another-good-password"})
	if e := errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeInvalidCurrentPassword); e.Field != "current_password" {
		t.Fatalf("field %q", e.Field)
	}
	rr = h.do("POST", "/api/v1/account/password", s1, apitypes.ChangePasswordRequest{CurrentPassword: testPassword, NewPassword: "short"})
	errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)

	rr = h.do("POST", "/api/v1/account/password", s1, apitypes.ChangePasswordRequest{CurrentPassword: testPassword, NewPassword: "another-good-password"})
	if rr.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rr.Code, rr.Body.String())
	}
	fresh := decode[apitypes.SessionResponse](t, rr).Token
	for _, old := range []string{s1, s2} {
		errBody(t, h.do("GET", "/api/v1/account", old, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	}
	if rr := h.do("GET", "/api/v1/account", fresh, nil); rr.Code != http.StatusOK {
		t.Fatalf("new session rejected: %d", rr.Code)
	}
	if r := login(h, "dev1", testPassword); r.code != http.StatusUnauthorized {
		t.Fatal("old password still works")
	}
	if r := login(h, "dev1", "another-good-password"); r.code != http.StatusOK {
		t.Fatalf("new password: %d", r.code)
	}
}

func TestLogoutEndsSessions(t *testing.T) {
	h := newHarness(t)
	h.account("dev1", store.RoleDeveloper, true)
	tok := mustLogin(t, h, "dev1").Token
	if rr := h.do("POST", "/api/v1/auth/logout", tok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", rr.Code, rr.Body.String())
	}
	errBody(t, h.do("GET", "/api/v1/account", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

func TestRoleChangeEndsSessions(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	h.account("admin2", store.RoleAdmin, true)
	tok2 := mustLogin(t, h, "admin2").Token
	a2, _ := h.st.GetAccountByUsername(context.Background(), "admin2")
	rr := h.do("PATCH", "/api/v1/accounts/"+strconv.FormatInt(a2.ID, 10), h.session(admin), apitypes.UpdateAccountRequest{Role: ptr(apitypes.RoleDeveloper)})
	if rr.Code != http.StatusOK {
		t.Fatalf("demote: %d %s", rr.Code, rr.Body.String())
	}
	// The demoted admin's existing (admin) session is dead.
	errBody(t, h.do("GET", "/api/v1/accounts", tok2, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

func TestRefreshRevalidatesAndRespectsAbsoluteLifetime(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)
	first := mustLogin(t, h, "dev1")
	tok := first.Token
	for elapsed := time.Duration(0); ; elapsed += 11 * time.Hour {
		h.clock.Add(11 * time.Hour)
		rr := h.do("POST", "/api/v1/auth/refresh", tok, nil)
		if rr.Code != http.StatusOK {
			if h.clock.Now().Before(first.SessionExpiresAt) {
				t.Fatalf("refresh refused at %v: %s", elapsed, rr.Body.String())
			}
			errBody(t, rr, http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
			break
		}
		resp := decode[apitypes.SessionResponse](t, rr)
		if !resp.SessionExpiresAt.Equal(first.SessionExpiresAt) {
			t.Fatalf("absolute lifetime moved: %v -> %v", first.SessionExpiresAt, resp.SessionExpiresAt)
		}
		if resp.ExpiresAt.After(first.SessionExpiresAt) {
			t.Fatal("token expiry beyond absolute lifetime")
		}
		tok = resp.Token
	}
	// A fresh login works again.
	tok = mustLogin(t, h, "dev1").Token
	// Refresh re-checks the database.
	if err := h.st.DeleteAccount(context.Background(), admin.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("POST", "/api/v1/auth/refresh", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

func TestForgedSessionTokensRejected(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	now := h.clock.Now()
	claims := auth.SessionClaims{
		Ver: admin.TokenVersion, SidIat: now.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: auth.SessionIssuer, Subject: strconv.FormatInt(admin.ID, 10), Audience: jwt.ClaimStrings{auth.SessionAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	key := bytes.Repeat([]byte{9}, auth.JWTKeySize)
	sign := func(m jwt.SigningMethod, k any) string {
		s, err := jwt.NewWithClaims(m, claims).SignedString(k)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if rr := h.do("GET", "/api/v1/accounts", sign(jwt.SigningMethodHS256, key), nil); rr.Code != http.StatusOK {
		t.Fatalf("baseline HS256 token rejected: %d", rr.Code)
	}
	for name, tok := range map[string]string{
		"HS384":     sign(jwt.SigningMethodHS384, key),
		"none":      sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType),
		"wrong key": sign(jwt.SigningMethodHS256, bytes.Repeat([]byte{1}, auth.JWTKeySize)),
	} {
		rr := h.do("GET", "/api/v1/accounts", tok, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, rr.Code)
		}
	}
	for _, hdr := range []string{"Basic abc", "Bearer", "Bearer ", "Token " + sign(jwt.SigningMethodHS256, key)} {
		errBody(t, rawAuth(h, "GET", "/api/v1/status", hdr), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	}
}

func TestAdminAPITokenCannotReachAdminOrSessionRoutes(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	tok := h.apiToken(admin, nil)
	if rr := h.do("GET", "/api/v1/status", tok, nil); rr.Code != http.StatusOK {
		t.Fatalf("developer route: %d", rr.Code)
	}
	errBody(t, h.do("GET", "/api/v1/accounts", tok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	errBody(t, h.do("PATCH", "/api/v1/settings", tok, `{"registration_enabled":true}`), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	for _, p := range []struct{ m, path string }{
		{"GET", "/api/v1/account/tokens"},
		{"POST", "/api/v1/account/tokens"},
		{"POST", "/api/v1/account/password"},
		{"POST", "/api/v1/auth/refresh"},
		{"POST", "/api/v1/auth/logout"},
	} {
		errBody(t, h.do(p.m, p.path, tok, "{}"), http.StatusForbidden, apitypes.ErrorCodeSessionRequired)
	}
}

func TestAPITokenStates(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)

	exp := h.clock.Now().Add(time.Hour)
	tok := h.apiToken(dev, &exp)
	if rr := h.do("GET", "/api/v1/status", tok, nil); rr.Code != http.StatusOK {
		t.Fatalf("valid token: %d", rr.Code)
	}
	toks, _ := h.st.ListAPITokens(context.Background(), dev.ID)
	if toks[0].LastUsedAt == nil {
		t.Fatal("last_used_at not recorded")
	}
	h.clock.Add(time.Hour)
	errBody(t, h.do("GET", "/api/v1/status", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)

	tok2 := h.apiToken(dev, nil)
	if _, err := h.st.UpdateAccount(context.Background(), 0, dev.ID, store.AccountUpdate{Approved: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("GET", "/api/v1/status", tok2, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	if err := h.st.DeleteAccount(context.Background(), admin.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("GET", "/api/v1/status", tok2, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	// Malformed and unknown tokens.
	errBody(t, h.do("GET", "/api/v1/status", "gdp_short", nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	errBody(t, h.do("GET", "/api/v1/status", "gdp_"+strings.Repeat("ab", 32), nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

func TestTokenEndpoints(t *testing.T) {
	h := newHarness(t)
	h.account("dev1", store.RoleDeveloper, true)
	sess := mustLogin(t, h, "dev1").Token

	errBody(t, h.do("POST", "/api/v1/account/tokens", sess, `{"name":""}`), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	past := h.clock.Now().Add(-time.Minute)
	errBody(t, h.do("POST", "/api/v1/account/tokens", sess, apitypes.CreateAPITokenRequest{Name: "x", ExpiresAt: &past}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)

	rr := h.do("POST", "/api/v1/account/tokens", sess, apitypes.CreateAPITokenRequest{Name: " ci "})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	created := decode[apitypes.CreateAPITokenResponse](t, rr)
	if !strings.HasPrefix(created.Secret, "gdp_") || created.Token.Name != "ci" || !strings.HasPrefix(created.Secret, created.Token.Prefix) {
		t.Fatalf("created %+v", created)
	}
	rr = h.do("GET", "/api/v1/account/tokens", sess, nil)
	if strings.Contains(rr.Body.String(), created.Secret) || strings.Contains(rr.Body.String(), auth.HashAPIToken(created.Secret)) {
		t.Fatal("list leaks the secret or hash")
	}
	list := decode[apitypes.APITokenList](t, rr)
	if len(list.Items) != 1 || list.Items[0].ID != created.Token.ID {
		t.Fatalf("list %+v", list)
	}
	if rr := h.do("GET", "/api/v1/status", created.Secret, nil); rr.Code != http.StatusOK {
		t.Fatalf("token auth: %d", rr.Code)
	}
	path := "/api/v1/account/tokens/" + strconv.FormatInt(created.Token.ID, 10)
	if rr := h.do("DELETE", path, sess, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	errBody(t, h.do("DELETE", path, sess, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, h.do("GET", "/api/v1/status", created.Secret, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	errBody(t, h.do("DELETE", "/api/v1/account/tokens/abc", sess, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
}

func TestRegistration(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	req := apitypes.RegisterRequest{Username: "NewUser", Password: testPassword}
	errBody(t, h.do("POST", "/api/v1/auth/register", "", req), http.StatusForbidden, apitypes.ErrorCodeRegistrationDisabled)

	rr := h.do("PATCH", "/api/v1/settings", h.session(admin), apitypes.UpdateSettingsRequest{RegistrationEnabled: ptr(true)})
	if rr.Code != http.StatusOK || !decode[apitypes.Settings](t, rr).RegistrationEnabled {
		t.Fatalf("enable registration: %d", rr.Code)
	}
	// Clients cannot smuggle in role or approval.
	errBody(t, h.do("POST", "/api/v1/auth/register", "", `{"username":"x1y","password":"`+testPassword+`","role":"admin"}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	if e := errBody(t, h.do("POST", "/api/v1/auth/register", "", apitypes.RegisterRequest{Username: "a!", Password: testPassword}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "username" {
		t.Fatalf("field %q", e.Field)
	}
	if e := errBody(t, h.do("POST", "/api/v1/auth/register", "", apitypes.RegisterRequest{Username: "abc", Password: strings.Repeat("p", 73)}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "password" {
		t.Fatalf("field %q", e.Field)
	}

	h.srv.registerIP.Reset(clientIPForTests)
	rr = h.do("POST", "/api/v1/auth/register", "", req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	acct := decode[apitypes.Account](t, rr)
	if acct.Username != "newuser" || acct.Role != apitypes.RoleDeveloper || acct.Approved {
		t.Fatalf("registered %+v", acct)
	}
	errBody(t, h.do("POST", "/api/v1/auth/register", "", req), http.StatusConflict, apitypes.ErrorCodeUsernameTaken)
	if r := login(h, "newuser", testPassword); r.code != http.StatusUnauthorized {
		t.Fatal("unapproved account could log in")
	}
	// Per-IP registration limit.
	for i := 0; i < registrationsPerWindow; i++ {
		h.do("POST", "/api/v1/auth/register", "", apitypes.RegisterRequest{Username: "spam" + strconv.Itoa(i), Password: testPassword})
	}
	errBody(t, h.do("POST", "/api/v1/auth/register", "", apitypes.RegisterRequest{Username: "spam-x", Password: testPassword}), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
}

const clientIPForTests = "192.0.2.10"

// rawAuth sends a request with an arbitrary Authorization header.
func rawAuth(h *harness, method, path, authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = clientIPForTests + ":1"
	req.Header.Set("Authorization", authz)
	rr := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rr, req)
	return rr
}
