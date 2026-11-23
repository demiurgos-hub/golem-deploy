package account

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	if err := database.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	middleware.SetJWTSecret("test-secret")
}

func sessionRequest(t *testing.T, method, path, username string, accessLevel int, body []byte) *http.Request {
	t.Helper()
	token, err := auth.GenerateToken(username, accessLevel, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestCreateHandlerIgnoresClientPrivileges(t *testing.T) {
	setupTestDB(t)
	body := []byte(`{"username":"eve","password":"password123","access_level":0,"approved":true}`)
	req := httptest.NewRequest(http.MethodPost, "/account/create", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	CreateHandler(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}

	var accessLevel, approved int
	err := database.DB.QueryRow("SELECT access_level, approved FROM accounts WHERE username = ?", "eve").
		Scan(&accessLevel, &approved)
	if err != nil {
		t.Fatal(err)
	}
	if accessLevel != 1 || approved != 0 {
		t.Fatalf("got access_level=%d approved=%d", accessLevel, approved)
	}
}

func TestTokensCRUDRequiresSession(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("alice", "password123", 1, true); err != nil {
		t.Fatal(err)
	}

	createBody, _ := json.Marshal(api.CreateAPITokenRequest{Name: "ci"})
	req := sessionRequest(t, http.MethodPost, "/account/tokens", "alice", 1, createBody)
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(TokensHandler)(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rr.Code, rr.Body.String())
	}

	var created api.CreateAPITokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || !strings.HasPrefix(created.Token, "gdp_") {
		t.Fatalf("expected gdp_ secret, got %q", created.Token)
	}
	if created.Prefix == "" || created.ID == 0 {
		t.Fatalf("incomplete create response: %+v", created)
	}
	var storedHash string
	if err := database.DB.QueryRow("SELECT token_hash FROM api_tokens WHERE id = ?", created.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == created.Token || strings.Contains(storedHash, created.Token) {
		t.Fatal("plaintext token was persisted")
	}
	if storedHash != database.HashAPIToken(created.Token) {
		t.Fatal("stored hash does not match SHA-256 of plaintext")
	}

	listReq := sessionRequest(t, http.MethodGet, "/account/tokens", "alice", 1, nil)
	listRR := httptest.NewRecorder()
	middleware.AuthMiddleware(TokensHandler)(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", listRR.Code, listRR.Body.String())
	}
	var listed api.ListAPITokensResponse
	if err := json.Unmarshal(listRR.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tokens) != 1 || listed.Tokens[0].Name != "ci" {
		t.Fatalf("list: %+v", listed.Tokens)
	}

	revokeBody, _ := json.Marshal(api.RevokeAPITokenRequest{ID: created.ID})
	revokeReq := sessionRequest(t, http.MethodPost, "/account/tokens/revoke", "alice", 1, revokeBody)
	revokeRR := httptest.NewRecorder()
	middleware.AuthMiddleware(RevokeTokenHandler)(revokeRR, revokeReq)
	if revokeRR.Code != http.StatusOK {
		t.Fatalf("revoke status %d: %s", revokeRR.Code, revokeRR.Body.String())
	}

	probe := middleware.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	authReq := httptest.NewRequest(http.MethodGet, "/", nil)
	authReq.Header.Set("Authorization", "Bearer "+created.Token)
	authRR := httptest.NewRecorder()
	probe(authRR, authReq)
	if authRR.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still accepted: %d", authRR.Code)
	}
}

func TestTokensRejectAPITokenAuth(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("alice", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("alice", "existing")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(api.CreateAPITokenRequest{Name: "second"})
	req := httptest.NewRequest(http.MethodPost, "/account/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(TokensHandler)(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestRefreshRejectsAPIToken(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("alice", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("alice", "ci")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/account/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(RefreshHandler)(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestRefreshAcceptsSession(t *testing.T) {
	setupTestDB(t)
	req := sessionRequest(t, http.MethodPost, "/account/refresh", "alice", 0, nil)
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(RefreshHandler)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp api.RefreshResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Fatal("expected new session token")
	}
	claims, err := auth.ValidateToken(resp.Token, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if claims.Username != "alice" || claims.AccessLevel != 0 {
		t.Fatalf("claims: %+v", claims)
	}
}

func TestApproveRequiresAdminSession(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("admin", "password123", 0, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("admin", "ci")
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"username":"eve","approve":true}`)
	apiReq := httptest.NewRequest(http.MethodPost, "/account/approve", bytes.NewReader(body))
	apiReq.Header.Set("Authorization", "Bearer "+plaintext)
	apiRR := httptest.NewRecorder()
	middleware.AuthMiddleware(ApproveHandler)(apiRR, apiReq)
	if apiRR.Code != http.StatusForbidden {
		t.Fatalf("api token: expected 403, got %d", apiRR.Code)
	}

	devReq := sessionRequest(t, http.MethodPost, "/account/approve", "dev", 1, body)
	devRR := httptest.NewRecorder()
	middleware.AuthMiddleware(ApproveHandler)(devRR, devReq)
	if devRR.Code != http.StatusForbidden {
		t.Fatalf("developer: expected 403, got %d", devRR.Code)
	}
}

func TestPasswordRejectsAPIToken(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("alice", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("alice", "ci")
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"old_password":"password123","new_password":"newpass123"}`)
	req := httptest.NewRequest(http.MethodPost, "/account/password", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(PasswordHandler)(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestDeleteAccountRemovesTokens(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("admin", "password123", 0, true); err != nil {
		t.Fatal(err)
	}
	if err := database.AddAccount("bob", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("bob", "ci")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteAccount("bob"); err != nil {
		t.Fatal(err)
	}

	h := middleware.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("token survived account delete: %d", rr.Code)
	}
}

func TestDeletedAccountSessionCannotMintToken(t *testing.T) {
	setupTestDB(t)
	if err := database.AddAccount("alice", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("alice", "ci")
	if err != nil {
		t.Fatal(err)
	}

	jwt, err := auth.GenerateToken("alice", 1, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteAccount("alice"); err != nil {
		t.Fatal(err)
	}

	probe := middleware.AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	preReq := httptest.NewRequest(http.MethodGet, "/", nil)
	preReq.Header.Set("Authorization", "Bearer "+plaintext)
	preRR := httptest.NewRecorder()
	probe(preRR, preReq)
	if preRR.Code != http.StatusUnauthorized {
		t.Fatalf("pre-delete token survived cascade: %d", preRR.Code)
	}

	body, _ := json.Marshal(api.CreateAPITokenRequest{Name: "after-delete"})
	req := httptest.NewRequest(http.MethodPost, "/account/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(TokensHandler)(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("leftover JWT minted a token: %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestDeleteHandlerRequiresAdmin(t *testing.T) {
	setupTestDB(t)
	body := []byte(`{"username":"bob"}`)
	req := sessionRequest(t, http.MethodPost, "/account/delete", "dev", 1, body)
	rr := httptest.NewRecorder()
	middleware.AuthMiddleware(DeleteHandler)(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}
