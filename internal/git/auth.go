package git

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// AuthMethod selects how git authenticates to a remote.
type AuthMethod int

// Authentication methods.
const (
	AuthNone AuthMethod = iota
	// AuthHTTPS answers git's username/password prompts through the askpass
	// helper (see Askpass). Only valid with https URLs.
	AuthHTTPS
	// AuthSSH uses PrivateKey as the only SSH identity. Only valid with SSH
	// URLs.
	AuthSSH
)

func (m AuthMethod) String() string {
	switch m {
	case AuthNone:
		return "none"
	case AuthHTTPS:
		return "https"
	case AuthSSH:
		return "ssh"
	}
	return "invalid"
}

// Auth holds the credentials of one remote. Its fmt and slog renderings
// never include Password or PrivateKey.
type Auth struct {
	Method   AuthMethod
	Username string
	// Password is the HTTPS password or access token.
	Password string
	// PrivateKey is an unencrypted SSH private key as returned by
	// PrepareSSHKey.
	PrivateKey []byte
}

// String implements fmt.Stringer without secrets.
func (a Auth) String() string {
	return fmt.Sprintf("git.Auth{method=%s username=%q secret=%s}", a.Method, a.Username, a.secretState())
}

// GoString implements fmt.GoStringer without secrets.
func (a Auth) GoString() string { return a.String() }

// Format implements fmt.Formatter so every verb, including %+v and %#v,
// renders without secrets.
func (a Auth) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(a.String())) }

// LogValue implements slog.LogValuer without secrets.
func (a Auth) LogValue() slog.Value {
	return slog.GroupValue(slog.String("method", a.Method.String()), slog.String("username", a.Username), slog.String("secret", a.secretState()))
}

func (a Auth) secretState() string {
	if a.Password != "" || len(a.PrivateKey) > 0 {
		return "[redacted]"
	}
	return "none"
}

// secrets lists the values that must be scrubbed from any output.
func (a Auth) secrets() []string {
	var out []string
	if a.Password != "" {
		out = append(out, a.Password)
	}
	return out
}

// Remote is what an operation talks to.
type Remote struct {
	URL string
	// Branch is the branch to check out. Clone accepts an empty Branch and
	// then uses the remote's default branch.
	Branch string
	Auth   Auth
}

// Validate checks the URL, the branch (when set) and that the auth method
// matches the URL kind.
func (r Remote) Validate() (Kind, error) {
	kind, err := ValidateRemoteURL(r.URL)
	if err != nil {
		return 0, err
	}
	if r.Branch != "" {
		if err := ValidateBranch(r.Branch); err != nil {
			return 0, err
		}
	}
	if err := CheckAuthKind(r.Auth.Method, kind); err != nil {
		return 0, err
	}
	switch r.Auth.Method {
	case AuthHTTPS:
		if err := ValidateUsername(r.Auth.Username); err != nil {
			return 0, err
		}
		if err := ValidatePassword(r.Auth.Password); err != nil {
			return 0, err
		}
	case AuthSSH:
		if len(r.Auth.PrivateKey) == 0 {
			return 0, errors.New("an SSH private key is required")
		}
	}
	return kind, nil
}

// CheckAuthKind reports whether method can be used with a URL of kind.
func CheckAuthKind(method AuthMethod, kind Kind) error {
	switch {
	case method == AuthHTTPS && kind != KindHTTPS:
		return errors.New("https credentials require an https:// repository URL")
	case method == AuthSSH && kind != KindSSH:
		return errors.New("an SSH key requires an ssh:// or user@host:path repository URL")
	}
	return nil
}

// Credential limits.
const (
	MaxUsernameLength = 255
	MaxPasswordLength = 4096
	MaxKeyLength      = 16 << 10
)

// ValidateUsername checks an HTTPS user name.
func ValidateUsername(u string) error {
	if u == "" {
		return errors.New("username is required for https authentication")
	}
	if len(u) > MaxUsernameLength {
		return errors.New("username is too long")
	}
	if strings.IndexFunc(u, func(c rune) bool { return unicode.IsControl(c) || unicode.IsSpace(c) }) >= 0 {
		return errors.New("username must not contain whitespace or control characters")
	}
	return nil
}

// ValidatePassword checks an HTTPS password or token. It must fit on one
// line because the askpass helper prints it as a line.
func ValidatePassword(p string) error {
	if p == "" {
		return errors.New("password is required for https authentication")
	}
	if len(p) > MaxPasswordLength {
		return errors.New("password is too long")
	}
	if strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return errors.New("password must not contain control characters or line breaks")
	}
	return nil
}

// SSH key errors. Their messages are safe to show to users.
var (
	ErrKeyEncrypted = errors.New("passphrase-protected SSH keys are not supported; provide an unencrypted deploy key")
	ErrKeyInvalid   = errors.New("the SSH private key could not be parsed; provide an unencrypted OpenSSH or PEM private key")
)

// PrepareSSHKey normalises an SSH private key (CRLF to LF, surrounding
// whitespace trimmed, exactly one trailing newline), checks that it parses
// as an unencrypted private key and returns the normalised bytes.
func PrepareSSHKey(raw string) ([]byte, error) {
	if len(raw) > MaxKeyLength {
		return nil, errors.New("the SSH private key is too large")
	}
	norm := strings.ReplaceAll(raw, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	norm = strings.TrimSpace(norm)
	if norm == "" {
		return nil, errors.New("an SSH private key is required")
	}
	key := []byte(norm + "\n")
	if _, err := ssh.ParseRawPrivateKey(key); err != nil {
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) || bytes.Contains(key, []byte("ENCRYPTED")) {
			return nil, ErrKeyEncrypted
		}
		return nil, ErrKeyInvalid
	}
	return key, nil
}
