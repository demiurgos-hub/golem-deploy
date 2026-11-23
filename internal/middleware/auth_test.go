package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	if err := database.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
}

func identityProbe() (http.HandlerFunc, *auth.Identity, *int) {
	var got auth.Identity
	var status int
	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFromRequest(r)
		if !ok {
			http.Error(w, "missing identity", http.StatusInternalServerError)
			return
		}
		got = id
		status = http.StatusOK
		w.WriteHeader(http.StatusOK)
	})
	return h, &got, &status
}

func TestAuthMiddlewareSessionIdentity(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")
	token, err := auth.GenerateToken("alice", 0, "test-secret")
	if err != nil {
		t.Fatal(err)
	}

	h, got, _ := identityProbe()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got.Username != "alice" || got.AccessLevel != 0 || got.Kind != auth.KindSession {
		t.Fatalf("identity: %+v", *got)
	}
}

func TestAuthMiddlewareAPITokenIsAlwaysDeveloper(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")
	if err := database.AddAccount("admin", "password123", 0, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("admin", "ci")
	if err != nil {
		t.Fatal(err)
	}

	h, got, _ := identityProbe()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got.Username != "admin" || got.AccessLevel != 1 || got.Kind != auth.KindAPI {
		t.Fatalf("identity: %+v", *got)
	}
}

func TestAuthMiddlewareRejectsRevokedAPIToken(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")
	if err := database.AddAccount("bob", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, rec, err := database.CreateAPIToken("bob", "ci")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeAPIToken(rec.ID, "bob"); err != nil {
		t.Fatal(err)
	}

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestRequireSessionRejectsAPIToken(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")
	if err := database.AddAccount("bob", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("bob", "ci")
	if err != nil {
		t.Fatal(err)
	}

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := RequireSession(w, r); !ok {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestRequireAdminRejectsDeveloperAndAPIToken(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")

	devJWT, err := auth.GenerateToken("dev", 1, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	adminJWT, err := auth.GenerateToken("admin", 0, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddAccount("admin", "password123", 0, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("admin", "ci")
	if err != nil {
		t.Fatal(err)
	}

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := RequireAdmin(w, r); !ok {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"admin session", adminJWT, http.StatusOK},
		{"developer session", devJWT, http.StatusForbidden},
		{"api token", plaintext, http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s: expected %d, got %d (%s)", tc.name, tc.want, rr.Code, rr.Body.String())
		}
	}
}

func TestAuthMiddlewareRejectsAPITokenForUnapprovedAccount(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")
	if err := database.AddAccount("bob", "password123", 1, true); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := database.CreateAPIToken("bob", "ci")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB.Exec("UPDATE accounts SET approved = 0 WHERE username = ?", "bob"); err != nil {
		t.Fatal(err)
	}

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestRequireSessionAcceptsJWT(t *testing.T) {
	setupTestDB(t)
	SetJWTSecret("test-secret")
	token, err := auth.GenerateToken("alice", 1, "test-secret")
	if err != nil {
		t.Fatal(err)
	}

	h := AuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := RequireSession(w, r); !ok {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}
