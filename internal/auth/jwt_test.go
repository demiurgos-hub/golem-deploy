package auth

import "testing"

func TestGenerateAndValidateToken(t *testing.T) {
	token, err := GenerateToken("alice", 0, "test-secret")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	claims, err := ValidateToken(token, "test-secret")
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if claims.Username != "alice" {
		t.Fatalf("username: got %q", claims.Username)
	}
	if claims.AccessLevel != 0 {
		t.Fatalf("access_level: got %d", claims.AccessLevel)
	}
}

func TestValidateTokenRejectsWrongSecret(t *testing.T) {
	token, err := GenerateToken("bob", 1, "test-secret")
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := ValidateToken(token, "other-secret"); err == nil {
		t.Fatal("expected validation to fail")
	}
}

func TestValidateTokenRejectsGarbage(t *testing.T) {
	if _, err := ValidateToken("not-a-jwt", "test-secret"); err == nil {
		t.Fatal("expected validation to fail")
	}
}

func TestIdentityKinds(t *testing.T) {
	session := Identity{Username: "alice", AccessLevel: 0, Kind: KindSession}
	apiTok := Identity{Username: "alice", AccessLevel: 1, Kind: KindAPI}
	if session.Kind != "session" || apiTok.Kind != "api" {
		t.Fatalf("unexpected kinds: %q %q", session.Kind, apiTok.Kind)
	}
	if apiTok.AccessLevel != 1 {
		t.Fatalf("api identity must be developer, got %d", apiTok.AccessLevel)
	}
}
