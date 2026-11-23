package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := testBox(t)
	for _, pt := range [][]byte{nil, []byte(""), []byte("hunter2"), bytes.Repeat([]byte("x"), 10000)} {
		ct, err := b.Seal(ProjectAuthPurpose(1), pt)
		if err != nil {
			t.Fatal(err)
		}
		if len(pt) > 0 && bytes.Contains(ct, pt) {
			t.Fatal("ciphertext contains plaintext")
		}
		got, err := b.Open(ProjectAuthPurpose(1), ct)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("round trip mismatch")
		}
	}
	// Nonces are random: sealing twice differs.
	c1, _ := b.Seal("p", []byte("same"))
	c2, _ := b.Seal("p", []byte("same"))
	if bytes.Equal(c1, c2) {
		t.Fatal("identical ciphertexts for identical input")
	}
	if c1[0] != version1 {
		t.Fatalf("version byte %d", c1[0])
	}
}

func TestOpenRejects(t *testing.T) {
	b := testBox(t)
	ct, err := b.Seal(ProjectAuthPurpose(1), []byte("secret password"))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := New(bytes.Repeat([]byte{8}, KeySize))

	flip := func(i int) []byte {
		c := bytes.Clone(ct)
		c[i] ^= 0x01
		return c
	}
	cases := map[string]struct {
		box     *Box
		purpose string
		ct      []byte
	}{
		"wrong purpose (other row)":    {b, ProjectAuthPurpose(2), ct},
		"wrong purpose (other column)": {b, ServerEnvPurpose(1), ct},
		"empty purpose":                {b, "", ct},
		"wrong key":                    {other, ProjectAuthPurpose(1), ct},
		"tampered version":             {b, ProjectAuthPurpose(1), flip(0)},
		"tampered nonce":               {b, ProjectAuthPurpose(1), flip(3)},
		"tampered body":                {b, ProjectAuthPurpose(1), flip(15)},
		"tampered tag":                 {b, ProjectAuthPurpose(1), flip(len(ct) - 1)},
		"truncated":                    {b, ProjectAuthPurpose(1), ct[:len(ct)-1]},
		"too short":                    {b, ProjectAuthPurpose(1), ct[:5]},
		"empty":                        {b, ProjectAuthPurpose(1), nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			pt, err := c.box.Open(c.purpose, c.ct)
			if !errors.Is(err, ErrDecrypt) || pt != nil {
				t.Fatalf("got %q, %v", pt, err)
			}
			if bytes.Contains([]byte(err.Error()), []byte("secret password")) {
				t.Fatal("error leaks plaintext")
			}
		})
	}
	if _, err := b.Seal("", []byte("x")); err == nil {
		t.Fatal("empty purpose accepted for Seal")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestLoadOrCreateKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "secrets.key")
	b1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != KeySize {
		t.Fatalf("key size %d", info.Size())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v, want 0600", info.Mode().Perm())
	}
	ct, _ := b1.Seal("p", []byte("v"))
	b2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := b2.Open("p", ct); err != nil || string(pt) != "v" {
		t.Fatalf("reloaded key differs: %v", err)
	}
}
