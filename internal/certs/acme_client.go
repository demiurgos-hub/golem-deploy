package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	"path"
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

type acmeClient struct {
	dir       string
	roots     *x509.CertPool
	logger    *slog.Logger
	listeners ACMEListeners
	active    atomic.Pointer[activeChallenge]
}

type activeChallenge struct {
	acme.Challenge
	log  io.Writer
	once sync.Once
}

func NewACMEClient(dir string, roots *x509.CertPool, logger *slog.Logger, listeners ACMEListeners) ACMEClient {
	return &acmeClient{dir: dir, roots: roots, logger: logger, listeners: listeners}
}

const maxACMEResponseBytes = 2 << 20

// Acmez accepts a custom HTTP client. Bound the decoded body before the
// library reads it, including responses without a Content-Length.
type acmeTransport struct {
	base       http.RoundTripper
	retryAfter *atomic.Pointer[time.Time]
}

func (t acmeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.retryAfter != nil && resp.StatusCode == http.StatusTooManyRequests {
		raw := resp.Header.Get("Retry-After")
		if n, err := strconv.ParseUint(raw, 10, 32); err == nil {
			when := time.Now().Add(time.Duration(n) * time.Second)
			t.retryAfter.Store(&when)
		} else if when, err := http.ParseTime(raw); err == nil {
			t.retryAfter.Store(&when)
		}
	}
	if resp.ContentLength > maxACMEResponseBytes {
		_ = resp.Body.Close()
		return nil, &http.MaxBytesError{Limit: maxACMEResponseBytes}
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxACMEResponseBytes)
	return resp, nil
}

func (m *acmeClient) client(account ACMEAccount, retryAfter *atomic.Pointer[time.Time]) (*acme.Client, func()) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: m.roots}
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &acme.Client{Directory: account.Directory, UserAgent: "golem-deploy",
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: acmeTransport{transport, retryAfter}},
	}, transport.CloseIdleConnections
}

func (m *acmeClient) storage(account ACMEAccount) (*certmagic.FileStorage, string) {
	dir := filepath.Join(m.dir, fmt.Sprintf("%x", sha256.Sum256([]byte(account.Directory))))
	issuer := certmagic.ACMEIssuer{CA: account.Directory}
	return &certmagic.FileStorage{Path: dir}, issuer.IssuerKey()
}

func pairStorageKey(issuer, host string) string {
	return path.Join(certmagic.StorageKeys.CertsSitePrefix(issuer, host), "pair.pem")
}

func (m *acmeClient) Load(ctx context.Context, account ACMEAccount, host string) ([]byte, []byte, error) {
	storage, issuer := m.storage(account)
	pem, err := storage.Load(ctx, pairStorageKey(issuer, host))
	if !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return nil, nil, err
		}
		// The key is last. Split it from the leaf and intermediates.
		i := bytes.LastIndex(pem, []byte("-----BEGIN"))
		if i < 0 {
			return nil, nil, ErrNoCertificate
		}
		return pem[:i], pem[i:], nil
	}
	// Older installations saved the certificate and key separately.
	chain, err := storage.Load(ctx, certmagic.StorageKeys.SiteCert(issuer, host))
	if err != nil {
		return nil, nil, err
	}
	key, err := storage.Load(ctx, certmagic.StorageKeys.SitePrivateKey(issuer, host))
	return chain, key, err
}

func profileFor(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return "shortlived"
	}
	return ""
}

func (m *acmeClient) Issue(ctx context.Context, account ACMEAccount, host string, force bool, log io.Writer) error {
	chain, key, err := m.Load(ctx, account, host)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrNoCertificate) {
		return err
	}
	prior, _ := parsePair(chain, key)
	if prior != nil && prior.info.Covers(host) && !force &&
		time.Now().Before(prior.info.NotAfter) && !time.Now().Before(prior.info.NotBefore) {
		return nil
	}
	var retryAfter atomic.Pointer[time.Time]
	client, stop := m.client(account, &retryAfter)
	defer stop()
	err = m.order(ctx, client, account, host, prior, log)
	if err != nil {
		if retry := retryAfter.Load(); retry != nil {
			failure := summarizeACME(err)
			if failure.retry.IsZero() {
				failure.retry = *retry
				failure.message += " The CA rate limited this request. Retry after " + retry.UTC().Format(time.RFC3339) + "."
			}
			return failure
		}
		return err
	}
	return nil
}

func (m *acmeClient) order(ctx context.Context, client *acme.Client, account ACMEAccount, host string, prior *certificatePair, log io.Writer) error {
	storage, issuer := m.storage(account)
	acct, err := m.loadAccount(ctx, client, storage, issuer, account.Email)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	template := &x509.CertificateRequest{DNSNames: []string{host}}
	if ip := net.ParseIP(host); ip != nil {
		template.DNSNames, template.IPAddresses = nil, []net.IP{ip}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return err
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return err
	}
	params, err := acmez.OrderParametersFromCSR(acct, csr)
	if err != nil {
		return err
	}
	params.Profile = profileFor(host)
	if prior != nil && prior.info.Covers(host) {
		dir, err := client.GetDirectory(ctx)
		if err != nil {
			return err
		}
		if _, err := acme.ARIUniqueIdentifier(prior.info.leaf); dir.RenewalInfo != "" && err == nil {
			params.Replaces = prior.info.leaf
		}
	}
	solver := challengeSolver{client: m, host: host, log: log}
	solvers := make(map[string]acmez.Solver, 2)
	if m.listeners.HTTP != "" {
		solvers[acme.ChallengeTypeHTTP01] = solver
	}
	if m.listeners.ALPN != "" {
		solvers[acme.ChallengeTypeTLSALPN01] = solver
	}
	orders := &acmez.Client{Client: client, ChallengeSolvers: solvers}
	chains, err := orders.ObtainCertificate(ctx, params)
	var problem acme.Problem
	if errors.As(err, &problem) {
		switch {
		case problem.Type == acme.ProblemTypeAccountDoesNotExist:
			// A private CA can lose its registrations on restart. Reuse the
			// local signing key and register again with the configured contact.
			_, regPath := accountStoragePaths(issuer, account.Email)
			if err := storage.Delete(ctx, regPath); err != nil {
				return err
			}
			params.Account, err = m.loadAccount(ctx, client, storage, issuer, account.Email)
			if err != nil {
				return err
			}
			params.Replaces = nil
			chains, err = orders.ObtainCertificate(ctx, params)
		case problem.Type == acme.ProblemTypeAlreadyReplaced && params.Replaces != nil:
			// The CA may have completed the last order before a crash could save it.
			params.Replaces = nil
			chains, err = orders.ObtainCertificate(ctx, params)
		}
	}
	if err != nil {
		return err
	}
	if len(chains) == 0 {
		return ErrNoCertificate
	}
	keyPEM, err := certmagic.PEMEncodePrivateKey(key)
	if err != nil {
		return err
	}
	pair, err := parsePair(chains[0].ChainPEM, keyPEM)
	if err != nil || !pair.info.Covers(host) {
		return ErrNoCertificate
	}
	// Once the CA has issued a valid pair, save it even if the job was
	// cancelled. Maintenance can install it from the atomic saved copy.
	data := append(append(pair.chain, '\n'), pair.key...)
	return storage.Store(context.WithoutCancel(ctx), pairStorageKey(issuer, host), data)
}

func accountStoragePaths(issuer, email string) (key, registration string) {
	// Keep the existing CertMagic account path and encoding so upgrades
	// reuse the same registration and signing key for each directory/email.
	user := strings.ToLower(email)
	if user == "" {
		user = "default"
	}
	name, _, _ := strings.Cut(user, "@")
	dir := path.Join("acme", certmagic.StorageKeys.Safe(issuer), "users", certmagic.StorageKeys.Safe(user))
	name = certmagic.StorageKeys.Safe(name)
	return path.Join(dir, name+".key"), path.Join(dir, name+".json")
}

func (m *acmeClient) loadAccount(ctx context.Context, client *acme.Client, storage *certmagic.FileStorage, issuer, email string) (acme.Account, error) {
	keyPath, regPath := accountStoragePaths(issuer, email)
	acct := acme.Account{TermsOfServiceAgreed: true}
	if email != "" {
		acct.Contact = []string{"mailto:" + email}
	}
	key, err := storage.Load(ctx, keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		acct.PrivateKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err == nil {
			key, err = certmagic.PEMEncodePrivateKey(acct.PrivateKey)
		}
		if err == nil {
			err = storage.Store(ctx, keyPath, key)
		}
	case err == nil:
		reg, readErr := storage.Load(ctx, regPath)
		if readErr == nil {
			err = json.Unmarshal(reg, &acct)
		} else if !errors.Is(readErr, fs.ErrNotExist) {
			err = readErr
		}
		if err == nil {
			acct.PrivateKey, err = certmagic.PEMDecodePrivateKey(key)
		}
	}
	if err != nil || acct.Status != "" {
		return acct, err
	}
	acct, err = client.NewAccount(ctx, acct)
	if err != nil {
		return acct, err
	}
	reg, err := json.Marshal(acct)
	if err == nil {
		err = storage.Store(context.WithoutCancel(ctx), regPath, reg)
	}
	if err == nil {
		m.logger.Info("new ACME account registered")
	}
	return acct, err
}

func (m *acmeClient) RenewalInfo(ctx context.Context, account ACMEAccount, _ string, leaf *x509.Certificate) (time.Time, time.Time, error) {
	client, stop := m.client(account, nil)
	defer stop()
	info, err := client.GetRenewalInfo(ctx, leaf)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var refresh time.Time
	if info.RetryAfter != nil {
		refresh = *info.RetryAfter
	}
	return info.SelectedTime, refresh, nil
}

type challengeSolver struct {
	client *acmeClient
	host   string
	log    io.Writer
}

func (s challengeSolver) Present(ctx context.Context, challenge acme.Challenge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if challenge.Identifier.Value != s.host {
		return errors.New("ACME challenge identifier does not match the order")
	}
	s.client.active.Store(&activeChallenge{Challenge: challenge, log: s.log})
	return nil
}

func (s challengeSolver) CleanUp(context.Context, acme.Challenge) error {
	s.client.active.Store(nil)
	return nil
}

// HTTPChallenge only accepts the current authorization's exact GET path and
// identifier. In particular, never invoke CertMagic's blind HTTP solver.
func (m *acmeClient) HTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	attempt := m.active.Load()
	if attempt == nil || r.Method != http.MethodGet || m.listeners.HTTP == "" {
		return false
	}
	host := certificateHost(r.Host)
	if host != attempt.Identifier.Value {
		return false
	}
	if attempt.Type != acme.ChallengeTypeHTTP01 || r.URL.EscapedPath() != attempt.HTTP01ResourcePath() {
		return false
	}
	attempt.once.Do(func() { fmt.Fprintf(attempt.log, "Answering HTTP-01 for %s\n", host) })
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, attempt.KeyAuthorization)
	return true
}

func (m *acmeClient) TLSChallenge(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	attempt := m.active.Load()
	if attempt == nil || m.listeners.ALPN == "" {
		return nil, nil
	}
	name := certificateHost(hello.ServerName)
	want := attempt.Identifier.Value
	if ip, err := netip.ParseAddr(want); err == nil {
		reversed, _ := dns.ReverseAddr(ip.String())
		want = strings.TrimSuffix(reversed, ".")
	}
	if name != want {
		return nil, nil
	}
	if attempt.Type != acme.ChallengeTypeTLSALPN01 {
		return nil, nil
	}
	attempt.once.Do(func() { fmt.Fprintf(attempt.log, "Answering TLS-ALPN-01 for %s\n", attempt.Identifier.Value) })
	return acmez.TLSALPN01ChallengeCert(attempt.Challenge)
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
