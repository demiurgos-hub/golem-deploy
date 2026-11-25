package auth

import (
	"encoding/base64"
	"testing"
)

func TestNewSetupToken(t *testing.T) {
	tok, hash, err := NewSetupToken()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 || len(tok) != 43 {
		t.Fatalf("token %q: %d bytes, %v", tok, len(raw), err)
	}
	if hash != HashSetupToken(tok) || len(hash) != 64 {
		t.Fatalf("hash %q", hash)
	}
	tok2, _, _ := NewSetupToken()
	if tok2 == tok {
		t.Fatal("tokens repeat")
	}
}
