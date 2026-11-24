package certs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/v3"
	"github.com/mholt/acmez/v3/acme"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ACMEClient separates ACME traffic and storage from the mode and install logic.
// Load is a local-only operation. All operations except challenge handlers are
// serialized by Automation.
type ACMEClient interface {
	Load(context.Context, ACMEAccount, string) (chain, key []byte, err error)
	Issue(context.Context, ACMEAccount, string, bool, io.Writer) error
	RenewalInfo(context.Context, ACMEAccount, string, *x509.Certificate) (when, refresh time.Time, err error)
	HTTPChallenge(http.ResponseWriter, *http.Request) bool
	TLSChallenge(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// ACMEAccount is an operation's immutable CA and account configuration.
type ACMEAccount struct {
	Directory string
	Email     string
}

// ACMEListeners describes the bound challenge listeners. ALPN is empty unless
// the panel serves TLS on port 443.
type ACMEListeners struct {
	HTTP string
	ALPN string
}

func ValidateDirectory(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err == nil && u.Hostname() != "" && u.User == nil && u.Fragment == "" {
		ip, _ := netip.ParseAddr(u.Hostname())
		if u.Scheme == "https" || (u.Scheme == "http" && (ip.IsLoopback() || strings.EqualFold(u.Hostname(), "localhost"))) {
			return nil
		}
	}
	return errors.New("--acme-directory must be an HTTPS URL (HTTP is permitted for loopback), without credentials or a fragment")
}

func LoadACMERoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --acme-root-ca: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("--acme-root-ca contains no valid PEM certificates")
	}
	return roots, nil
}

type magicClient struct {
	dir       string
	roots     *x509.CertPool
	logger    *slog.Logger
	listeners ACMEListeners
	active    atomic.Pointer[activeChallenge]
}

type activeChallenge struct {
	host string
	log  io.Writer
	once sync.Once
}

func NewACMEClient(dir string, roots *x509.CertPool, logger *slog.Logger, listeners ACMEListeners) ACMEClient {
	return &magicClient{dir: dir, roots: roots, logger: logger, listeners: listeners}
}

// The data-directory process lock and Automation's operation lock already
// serialize every storage writer. Avoid disk lease files that can delay crash
// recovery and that FileStorage creates with mode 0644.
type localACMEStorage struct{ *certmagic.FileStorage }

func (localACMEStorage) Lock(ctx context.Context, _ string) error { return ctx.Err() }
func (localACMEStorage) Unlock(context.Context, string) error     { return nil }
func (s localACMEStorage) Store(ctx context.Context, key string, value []byte) error {
	// CertMagic still supplies its embedded solvers, but a single daemon
	// needs only the library's active in-memory challenges. Leave its
	// distributed mode enabled to disable blind fallback, without retaining
	// challenge state that could outlive the current process.
	if strings.Contains(key, "/challenge_tokens/") {
		return ctx.Err()
	}
	return s.FileStorage.Store(ctx, key, value)
}

// Using an empty contact must not prompt on stdin or discover a previously
// configured email from another account. The issuer starts with empty email.
type contactlessIssuer struct{ *certmagic.ACMEIssuer }

func (contactlessIssuer) PreCheck(context.Context, []string, bool) error { return nil }

func (m *magicClient) config(account ACMEAccount, host string, retryAfter *atomic.Pointer[time.Time]) (*certmagic.Config, *certmagic.ACMEIssuer, func()) {
	var profile string
	if _, err := netip.ParseAddr(host); err == nil {
		profile = "shortlived"
	}
	log := zap.New(acmeLogCore{logger: m.logger, retryAfter: retryAfter})
	// The cache stays empty: Automation drives renewal and publishes pairs
	// itself. CertMagic's independent maintenance never contacts the CA.
	cache := certmagic.NewCache(certmagic.CacheOptions{
		Logger: log,
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return nil, errors.New("ACME cache is not used to serve certificates")
		},
	})
	cfg := certmagic.New(cache, certmagic.Config{
		Storage: m.storage(account),
		Issuers: []certmagic.Issuer{}, Logger: log, RenewalWindowRatio: 0.5,
	})
	httpHost, httpPort := listenerParts(m.listeners.HTTP)
	alpnHost, alpnPort := listenerParts(m.listeners.ALPN)
	listenHost := httpHost
	if httpPort == 0 {
		listenHost = alpnHost
	} else if alpnPort != 0 && alpnHost != httpHost {
		listenHost = ""
	}
	issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA: account.Directory, TestCA: account.Directory, Profile: profile,
		Email: account.Email, Agreed: true, TrustedRoots: m.roots, Logger: log,
		DisableHTTPChallenge: httpPort == 0, DisableTLSALPNChallenge: alpnPort == 0,
		ListenHost: listenHost, AltHTTPPort: httpPort, AltTLSALPNPort: alpnPort,
	})
	if account.Email == "" {
		cfg.Issuers = []certmagic.Issuer{contactlessIssuer{issuer}}
	} else {
		cfg.Issuers = []certmagic.Issuer{issuer}
	}
	return cfg, issuer, cache.Stop
}

func listenerParts(address string) (string, int) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

func (m *magicClient) storage(account ACMEAccount) localACMEStorage {
	dir := filepath.Join(m.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(account.Directory))))
	return localACMEStorage{&certmagic.FileStorage{Path: dir}}
}

func storedPair(ctx context.Context, storage certmagic.Storage, issuerKey, host string) ([]byte, []byte, error) {
	chain, err := storage.Load(ctx, certmagic.StorageKeys.SiteCert(issuerKey, host))
	if err != nil {
		return nil, nil, err
	}
	key, err := storage.Load(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, host))
	return chain, key, err
}

func (m *magicClient) Load(ctx context.Context, account ACMEAccount, host string) ([]byte, []byte, error) {
	issuer := certmagic.ACMEIssuer{CA: account.Directory}
	return storedPair(ctx, m.storage(account), issuer.IssuerKey(), host)
}

func (m *magicClient) Issue(ctx context.Context, account ACMEAccount, host string, force bool, log io.Writer) (issueErr error) {
	var retryAfter atomic.Pointer[time.Time]
	cfg, issuer, stop := m.config(account, host, &retryAfter)
	defer stop()
	defer func() {
		retry := retryAfter.Load()
		if issueErr == nil || retry == nil {
			return
		}
		failure := summarizeACME(issueErr)
		if failure.retry.IsZero() {
			failure.retry = *retry
			failure.message += " The CA rate limited this request. Retry after " + retry.UTC().Format(time.RFC3339) + "."
		}
		issueErr = failure
	}()
	chain, key, err := storedPair(ctx, cfg.Storage, issuer.IssuerKey(), host)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		pair, parseErr := parsePair(chain, key)
		if parseErr == nil && pair.info.Covers(host) {
			if force || !pair.info.NotAfter.After(time.Now()) || pair.info.NotBefore.After(time.Now()) {
				m.active.Store(&activeChallenge{host: host, log: log})
				defer m.active.Store(nil)
				return cfg.RenewCertSync(ctx, host, true)
			}
			return nil
		}
		// A hard stop between CertMagic's resource writes can leave an
		// incomplete pair. Keep serving the installed copy and re-obtain.
		if err := cfg.Storage.Delete(ctx, certmagic.StorageKeys.CertsSitePrefix(issuer.IssuerKey(), host)); err != nil {
			return err
		}
	}
	m.active.Store(&activeChallenge{host: host, log: log})
	defer m.active.Store(nil)
	return cfg.ObtainCertSync(ctx, host)
}

func (m *magicClient) RenewalInfo(ctx context.Context, account ACMEAccount, host string, leaf *x509.Certificate) (time.Time, time.Time, error) {
	_, issuer, stop := m.config(account, host, nil)
	defer stop()
	info, err := issuer.GetRenewalInfo(ctx, certmagic.Certificate{Certificate: tls.Certificate{Leaf: leaf}})
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var refresh time.Time
	if info.RetryAfter != nil {
		refresh = *info.RetryAfter
	}
	return info.SelectedTime, refresh, nil
}

// HTTPChallenge only accepts the current authorization's exact GET path and
// identifier. In particular, never invoke CertMagic's blind HTTP solver.
func (m *magicClient) HTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	attempt := m.active.Load()
	if attempt == nil || r.Method != http.MethodGet || m.listeners.HTTP == "" {
		return false
	}
	host := certificateHost(r.Host)
	if host != attempt.host {
		return false
	}
	challenge, ok := certmagic.GetACMEChallenge(host)
	if !ok || challenge.Type != acme.ChallengeTypeHTTP01 || r.URL.EscapedPath() != challenge.HTTP01ResourcePath() {
		return false
	}
	attempt.once.Do(func() { fmt.Fprintf(attempt.log, "Answering HTTP-01 for %s\n", host) })
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, challenge.KeyAuthorization)
	return true
}

func (m *magicClient) TLSChallenge(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	attempt := m.active.Load()
	if attempt == nil || m.listeners.ALPN == "" {
		return nil, nil
	}
	name := certificateHost(hello.ServerName)
	want := attempt.host
	if ip, err := netip.ParseAddr(want); err == nil {
		reversed, _ := dns.ReverseAddr(ip.String())
		want = strings.TrimSuffix(reversed, ".")
	}
	if name != want {
		return nil, nil
	}
	challenge, ok := certmagic.GetACMEChallenge(name)
	if !ok || challenge.Type != acme.ChallengeTypeTLSALPN01 {
		return nil, nil
	}
	attempt.once.Do(func() { fmt.Fprintf(attempt.log, "Answering TLS-ALPN-01 for %s\n", attempt.host) })
	return acmez.TLSALPN01ChallengeCert(challenge.Challenge)
}

func certificateHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return host
}

// acmeLogCore forwards known lifecycle messages only. Upstream fields can
// contain raw CA responses, request metadata or certificate material.
type acmeLogCore struct {
	logger     *slog.Logger
	retryAfter *atomic.Pointer[time.Time]
}

func (c acmeLogCore) Enabled(level zapcore.Level) bool {
	return c.retryAfter != nil || level >= zapcore.InfoLevel
}
func (c acmeLogCore) With([]zapcore.Field) zapcore.Core { return c }
func (c acmeLogCore) Sync() error                       { return nil }
func (c acmeLogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}
func (c acmeLogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	switch e.Message {
	case "obtaining certificate", "renewing certificate", "certificate obtained successfully", "certificate renewed successfully", "new ACME account registered":
		c.logger.Info(e.Message)
	case "http request":
		// Acmez exposes the header only in its debug HTTP event. Read
		// the retry time without forwarding the request or the headers.
		if c.retryAfter == nil {
			break
		}
		var status int64
		var headers http.Header
		for _, field := range fields {
			switch field.Key {
			case "status_code":
				status = field.Integer
			case "response_headers":
				headers, _ = field.Interface.(http.Header)
			}
		}
		if status == http.StatusTooManyRequests {
			raw := headers.Get("Retry-After")
			if n, err := strconv.ParseUint(raw, 10, 32); err == nil {
				t := time.Now().Add(time.Duration(n) * time.Second)
				c.retryAfter.Store(&t)
			} else if t, err := http.ParseTime(raw); err == nil {
				c.retryAfter.Store(&t)
			}
		}
	}
	// Automation logs failures with a bounded, user-safe summary.
	return nil
}
