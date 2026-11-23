package git

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is the transport of a validated remote URL.
type Kind int

// Remote URL kinds.
const (
	KindHTTPS Kind = iota + 1
	KindSSH
)

func (k Kind) String() string {
	switch k {
	case KindHTTPS:
		return "https"
	case KindSSH:
		return "ssh"
	}
	return "invalid"
}

// Limits of validated input.
const (
	MaxURLLength    = 2048
	MaxBranchLength = 255
	maxHostLength   = 253
	maxUserLength   = 64
)

var (
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	sshUserRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	// transportRE matches git's "<transport>::<address>" remote-helper
	// syntax (ext::, fd::, ...).
	transportRE = regexp.MustCompile(`^[A-Za-z0-9+.-]*::`)
)

// ValidateRemoteURL checks that raw is a remote URL the daemon may contact
// and returns its kind. Accepted forms:
//
//	https://host[:port]/path            (no user info, query or fragment)
//	ssh://[user@]host[:port]/path       (no password)
//	user@host:path                      (scp-like SSH)
//
// Everything else is rejected: other schemes (http, git, file, ext::...,
// git+ssh), local paths, user info in https URLs (credentials belong in the
// project's auth fields), a leading '-', whitespace, control characters,
// backslashes and non-UTF-8 bytes. Plain http:// is never allowed: it would
// send credentials and code in clear text.
//
// Error messages are safe to show to users and never echo the input.
func ValidateRemoteURL(raw string) (Kind, error) {
	switch {
	case raw == "":
		return 0, errors.New("repository URL is required")
	case len(raw) > MaxURLLength:
		return 0, errors.New("repository URL is too long")
	case !utf8.ValidString(raw):
		return 0, errors.New("repository URL must be valid UTF-8")
	case strings.HasPrefix(raw, "-"):
		return 0, errors.New("repository URL must not start with '-'")
	case strings.ContainsRune(raw, '\\'):
		return 0, errors.New("repository URL must not contain backslashes")
	}
	for _, c := range raw {
		if unicode.IsSpace(c) || unicode.IsControl(c) || !unicode.IsPrint(c) {
			return 0, errors.New("repository URL must not contain whitespace or control characters")
		}
	}
	if scheme, _, ok := strings.Cut(raw, "://"); ok && !strings.ContainsAny(scheme, "/@:") {
		return validateSchemeURL(strings.ToLower(scheme), raw)
	}
	if transportRE.MatchString(raw) {
		return 0, errors.New("repository URL must use https:// or ssh")
	}
	return validateSCPLike(raw)
}

func validateSchemeURL(scheme, raw string) (Kind, error) {
	var kind Kind
	switch scheme {
	case "https":
		kind = KindHTTPS
	case "ssh":
		kind = KindSSH
	default:
		return 0, errors.New("repository URL must use https:// or ssh (" + schemeHint(scheme) + ")")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return 0, errors.New("repository URL is malformed")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "?#") {
		return 0, errors.New("repository URL must not contain a query or fragment")
	}
	if u.User != nil {
		if kind == KindHTTPS {
			return 0, errors.New("https repository URLs must not contain a user name or password; use the auth fields")
		}
		if _, has := u.User.Password(); has {
			return 0, errors.New("repository URL must not contain a password")
		}
		if err := validateSSHUser(u.User.Username()); err != nil {
			return 0, err
		}
	}
	if err := validateHost(u.Hostname(), strings.HasPrefix(u.Host, "[")); err != nil {
		return 0, err
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return 0, errors.New("repository URL has an invalid port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return 0, errors.New("repository URL has an invalid port")
	}
	if strings.Trim(u.Path, "/") == "" {
		return 0, errors.New("repository URL must include a repository path")
	}
	return kind, nil
}

func schemeHint(scheme string) string {
	if scheme == "http" {
		return "plain http is not allowed"
	}
	return "scheme not allowed"
}

func validateSCPLike(raw string) (Kind, error) {
	errForm := errors.New("repository URL must be https://host/path, ssh://[user@]host/path or user@host:path; local paths are not allowed")
	at := strings.IndexByte(raw, '@')
	if at <= 0 {
		return 0, errForm
	}
	user, rest := raw[:at], raw[at+1:]
	var host, path string
	bracketed := strings.HasPrefix(rest, "[")
	if bracketed {
		end := strings.Index(rest, "]:")
		if end < 0 {
			return 0, errForm
		}
		host, path = rest[1:end], rest[end+2:]
	} else {
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			return 0, errForm
		}
		host, path = rest[:colon], rest[colon+1:]
	}
	if strings.ContainsAny(user, "/:") || strings.Contains(host, "/") {
		return 0, errForm
	}
	if err := validateSSHUser(user); err != nil {
		return 0, err
	}
	if err := validateHost(host, bracketed); err != nil {
		return 0, err
	}
	if path == "" || strings.Trim(path, "/") == "" {
		return 0, errors.New("repository URL must include a repository path")
	}
	if strings.HasPrefix(path, "-") {
		return 0, errors.New("repository path must not start with '-'")
	}
	return KindSSH, nil
}

func validateSSHUser(user string) error {
	if user == "" || len(user) > maxUserLength || !sshUserRE.MatchString(user) {
		return errors.New("repository URL has an invalid SSH user name")
	}
	return nil
}

func validateHost(host string, bracketed bool) error {
	if host == "" {
		return errors.New("repository URL must include a host")
	}
	if bracketed {
		// IPv6 literal: only hex digits, colons and dots (no zone).
		for _, c := range host {
			if !(c == ':' || c == '.' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')) {
				return errors.New("repository URL has an invalid host")
			}
		}
		return nil
	}
	if len(host) > maxHostLength || !hostnameRE.MatchString(host) || strings.Contains(host, "..") {
		return errors.New("repository URL has an invalid host")
	}
	return nil
}

// URLHost returns the host[:port] of a scheme URL (https:// or ssh://)
// accepted by ValidateRemoteURL, and "" for scp-like URLs.
func URLHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && strings.Contains(raw, "://") {
		return u.Host
	}
	return ""
}

// ValidateBranch checks a branch name with the rules of
// "git check-ref-format --branch" plus a length limit, a ban on a leading
// '-' (option injection) and printable characters only. Error messages are
// safe to show to users.
func ValidateBranch(b string) error {
	switch {
	case b == "":
		return errors.New("branch is required")
	case len(b) > MaxBranchLength:
		return errors.New("branch name is too long")
	case !utf8.ValidString(b):
		return errors.New("branch name must be valid UTF-8")
	case strings.HasPrefix(b, "-"):
		return errors.New("branch name must not start with '-'")
	case b == "HEAD" || b == "@":
		return errors.New("branch name is reserved")
	case strings.Contains(b, ".."):
		return errors.New("branch name must not contain '..'")
	case strings.Contains(b, "@{"):
		return errors.New("branch name must not contain '@{'")
	case strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.Contains(b, "//"):
		return errors.New("branch name must not start or end with '/' or contain '//'")
	case strings.HasSuffix(b, "."):
		return errors.New("branch name must not end with '.'")
	}
	for _, c := range b {
		if c < 0x20 || c == 0x7f || unicode.IsSpace(c) || unicode.IsControl(c) || !unicode.IsPrint(c) {
			return errors.New("branch name must not contain whitespace or control characters")
		}
		if strings.ContainsRune("~^:?*[\\", c) {
			return errors.New("branch name must not contain any of ~ ^ : ? * [ \\")
		}
	}
	for _, comp := range strings.Split(b, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return errors.New("branch name components must not start with '.' or end with '.lock'")
		}
	}
	return nil
}
