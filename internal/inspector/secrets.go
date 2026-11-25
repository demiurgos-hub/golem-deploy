package inspector

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// hubSecrets are the Inspector hub's secrets. They are generated on the
// first enable, stored sealed (secrets.InspectorSecretsPurpose) in the
// settings table and never returned, logged or put in argv. Nobody types
// the hub password: only the daemon logs in at the hub.
type hubSecrets struct {
	// Password is the hub's UI password and PasswordHash its bcrypt hash
	// (the hub's GOLEM_INSPECTOR_PASSWORD_HASH), kept so the hub's
	// environment does not change on every daemon start.
	Password     string `json:"password"`
	PasswordHash string `json:"password_hash"`
	// AgentToken is the hub's own agent token; only the daemon's agent
	// relay presents it.
	AgentToken string `json:"agent_token"`
	// TokenKey is the HMAC key of the per-server agent tokens (hex).
	TokenKey string `json:"token_key"`
	// Generation counts secret replacements; it stands in for the secrets
	// in config hashes.
	Generation int64 `json:"generation"`
}

// HubBcryptCost is the bcrypt cost of the hub password hash (the hub's
// production minimum).
const HubBcryptCost = 12

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newHubSecrets generates a fresh set with the given generation.
func newHubSecrets(generation int64, cost int) (*hubSecrets, error) {
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		return nil, err
	}
	password := base64.RawURLEncoding.EncodeToString(pw)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return nil, err
	}
	token, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	key, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	return &hubSecrets{Password: password, PasswordHash: string(hash), AgentToken: token, TokenKey: key, Generation: generation}, nil
}

func (h *hubSecrets) valid() bool {
	key, err := hex.DecodeString(h.TokenKey)
	return h.Password != "" && strings.HasPrefix(h.PasswordHash, "$2") && len(h.AgentToken) >= 32 && err == nil && len(key) == 32
}

// loadSecrets reads the sealed secrets (nil when none were generated).
func loadSecrets(ctx context.Context, st *store.Store, box *secrets.Box) (*hubSecrets, error) {
	raw, ok, err := st.GetSetting(ctx, store.SettingInspectorSecrets)
	if err != nil || !ok || raw == "" {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("inspector: stored secrets are not base64")
	}
	pt, err := box.Open(secrets.InspectorSecretsPurpose(), ct)
	if err != nil {
		return nil, fmt.Errorf("inspector: open stored secrets: %w", err)
	}
	var h hubSecrets
	if err := json.Unmarshal(pt, &h); err != nil || !h.valid() {
		return nil, errors.New("inspector: stored secrets are malformed")
	}
	return &h, nil
}

// saveSecrets seals and stores h (a durable write).
func saveSecrets(ctx context.Context, st *store.Store, box *secrets.Box, h *hubSecrets) error {
	pt, err := json.Marshal(h)
	if err != nil {
		return err
	}
	ct, err := box.Seal(secrets.InspectorSecretsPurpose(), pt)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, store.SettingInspectorSecrets, base64.StdEncoding.EncodeToString(ct))
}

// AgentTokenPrefix starts every per-server agent token.
const AgentTokenPrefix = "gdi_"

// agentToken is the per-server agent token: "gdi_<server id>_<hex
// HMAC-SHA256(key, server id)>". The games never learn the hub's own agent
// token.
func agentToken(keyHex string, id int64) (string, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return "", errors.New("inspector: invalid agent token key")
	}
	if id <= 0 {
		return "", errors.New("inspector: invalid server id")
	}
	return AgentTokenPrefix + strconv.FormatInt(id, 10) + "_" + hex.EncodeToString(tokenMAC(key, id)), nil
}

func tokenMAC(key []byte, id int64) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("golem-deploy inspector agent\x00" + strconv.FormatInt(id, 10)))
	return m.Sum(nil)
}

// verifyAgentToken returns the server id a token was issued for. The MAC
// is compared in constant time.
func verifyAgentToken(keyHex, tok string) (int64, bool) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 || len(tok) > 128 {
		return 0, false
	}
	rest, ok := strings.CutPrefix(tok, AgentTokenPrefix)
	if !ok {
		return 0, false
	}
	idStr, macHex, ok := strings.Cut(rest, "_")
	if !ok || idStr == "" || len(idStr) > 19 || strings.TrimLeft(idStr, "0123456789") != "" || idStr[0] == '0' {
		return 0, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	got, err := hex.DecodeString(macHex)
	if err != nil || len(got) != sha256.Size || strings.ToLower(macHex) != macHex {
		return 0, false
	}
	if subtle.ConstantTimeCompare(got, tokenMAC(key, id)) != 1 {
		return 0, false
	}
	return id, true
}
