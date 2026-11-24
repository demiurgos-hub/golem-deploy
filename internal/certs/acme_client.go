package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"

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

	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	Directory         string
	Email             string
	DirectoryOverride bool
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
	dir          string
	newTransport func() (http.RoundTripper, func())
	logger       *slog.Logger
	listeners    ACMEListeners
	active       atomic.Pointer[activeChallenge]
}

type activeChallenge struct {
	acme.Challenge
	ctx  context.Context
	log  io.Writer
	once sync.Once
}

func NewACMEClient(dir string, roots *x509.CertPool, logger *slog.Logger, listeners ACMEListeners) ACMEClient {
	newTransport := func() (http.RoundTripper, func()) {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
		transport.ResponseHeaderTimeout = 30 * time.Second
		return transport, transport.CloseIdleConnections
	}
	return &acmeClient{dir: dir, newTransport: newTransport, logger: logger, listeners: listeners}
}

const (
	letsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"
	letsEncryptStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

func (m *acmeClient) client(account ACMEAccount) (*acme.Client, *acmeTransport, func()) {
	transport, stop := m.newTransport()
	bounded := &acmeTransport{base: transport}
	// Poll once a second when the CA gives no Retry-After: the library's
	// default of four polls a second would use up the order's request
	// budget long before its deadline.
	return &acme.Client{Directory: account.Directory, UserAgent: "golem-deploy", PollInterval: time.Second,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: bounded},
	}, bounded, stop
}

func (m *acmeClient) Load(ctx context.Context, account ACMEAccount, host string) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	pair, err := m.storage(account).loadPair(host)
	return pair.Chain, pair.Key, err
}

func orderProfile(account ACMEAccount, dir acme.Directory, host string) (string, error) {
	const shortlived = "shortlived"
	if _, err := netip.ParseAddr(host); err != nil {
		return "", nil
	}
	if dir.Meta != nil {
		if _, advertised := dir.Meta.Profiles[shortlived]; advertised {
			return shortlived, nil
		}
	}
	if !account.DirectoryOverride && (account.Directory == letsEncryptProduction || account.Directory == letsEncryptStaging) {
		return "", &acmeFailure{message: "The Let's Encrypt directory does not advertise the shortlived profile required for IP certificates."}
	}
	return "", nil
}

func (m *acmeClient) Issue(ctx context.Context, account ACMEAccount, host string, force bool, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()
	storage := m.storage(account)
	saved, err := storage.loadPair(host)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrNoCertificate) {
		return err
	}
	if !force {
		if _, err := validateACMEPair(saved.Chain, saved.Key, host, time.Now()); err == nil {
			return nil
		}
	}
	client, traffic, stop := m.client(account)
	defer stop()
	traffic.orderDeadline, _ = ctx.Deadline()
	err = m.order(ctx, client, traffic, account, host, storage, saved, log)
	if err != nil && !traffic.retryAfter.IsZero() {
		failure := summarizeACME(err)
		failure.retry = traffic.retryAfter
		if latest := time.Now().Add(24 * time.Hour); failure.retry.After(latest) {
			failure.retry = latest
		}
		failure.message += " Retry after " + failure.retry.UTC().Format(time.RFC3339) + "."
		return failure
	}
	return err
}

func (m *acmeClient) order(ctx context.Context, client *acme.Client, traffic *acmeTransport, account ACMEAccount,
	host string, storage acmeStorage, saved storedACMEPair, log io.Writer) error {
	dir, err := client.GetDirectory(ctx)
	if err != nil {
		return err
	}
	profile, err := orderProfile(account, dir, host)
	if err != nil {
		return err
	}
	traffic.newOrderURL = dir.NewOrder
	traffic.pendingAuthorizations = make(map[string]bool)
	acct, err := m.loadAccount(ctx, client, storage, account.Email)
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
	params.Profile = profile
	if dir.RenewalInfo != "" && saved.AccountURL == acct.Location {
		if prior, err := parsePair(saved.Chain, saved.Key); err == nil && prior.info.Covers(host) {
			if _, err := acme.ARIUniqueIdentifier(prior.info.leaf); err == nil {
				params.Replaces = prior.info.leaf
			}
		}
	}
	solver := &challengeSolver{client: m, host: host, log: log}
	solvers := make(map[string]acmez.Solver, 2)
	if m.listeners.HTTP != "" {
		solvers[acme.ChallengeTypeHTTP01] = solver
	}
	if m.listeners.ALPN != "" {
		solvers[acme.ChallengeTypeTLSALPN01] = solver
	}
	orders := &acmez.Client{Client: client, ChallengeSolvers: solvers}
	obtain := func() ([]acme.Certificate, error) {
		defer solver.clear()
		return orders.ObtainCertificate(ctx, params)
	}
	chains, err := obtain()
	var problem acme.Problem
	if ctx.Err() == nil && errors.As(err, &problem) {
		switch {
		case problem.Type == acme.ProblemTypeAccountDoesNotExist:
			// A private CA can lose its registrations on restart. Reuse
			// the signing key and register again with the configured contact.
			_, regName := accountStorageNames(account.Email)
			if err := storage.removeRegistration(regName); err != nil {
				return err
			}
			params.Account, err = m.loadAccount(ctx, client, storage, account.Email)
			if err != nil {
				return err
			}
			clear(traffic.pendingAuthorizations)
			params.Replaces = nil
			chains, err = obtain()
		case traffic.rejectedReplacement:
			// Retry only a rejection of a newOrder that actually carried
			// the hint. Acmez may already have retried an alreadyReplaced.
			params.Replaces = nil
			chains, err = obtain()
		}
	}
	if err != nil && ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		for auth := range traffic.pendingAuthorizations {
			_, _ = client.DeactivateAuthorization(cleanupCtx, params.Account, auth)
			if cleanupCtx.Err() != nil {
				break
			}
		}
	}
	if traffic.exhausted {
		return &acmeFailure{message: errACMEOrderBudget.Error() + "."}
	}
	if err != nil {
		return err
	}
	if len(chains) == 0 {
		return &acmeFailure{message: "The CA did not provide a certificate chain."}
	}
	keyPEM, err := privateKeyPEM(key)
	if err != nil {
		return err
	}
	pair, err := validateACMEPair(chains[0].ChainPEM, keyPEM, host, time.Now())
	if err != nil {
		return err
	}
	// Save a valid issued pair even if the job was cancelled. Maintenance
	// can install it from the atomic saved copy.
	return storage.savePair(host, storedACMEPair{AccountURL: params.Account.Location, Chain: pair.chain, Key: pair.key})
}

func (m *acmeClient) loadAccount(ctx context.Context, client *acme.Client, storage acmeStorage, email string) (acme.Account, error) {
	keyName, regName := accountStorageNames(email)
	acct := acme.Account{TermsOfServiceAgreed: true}
	if email != "" {
		acct.Contact = []string{"mailto:" + email}
	}
	key, err := storage.read("accounts", keyName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		acct.PrivateKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err == nil {
			key, err = privateKeyPEM(acct.PrivateKey)
		}
		if err == nil {
			err = storage.write("accounts", keyName, key)
		}
	case err == nil:
		reg, readErr := storage.read("accounts", regName)
		if readErr == nil {
			err = json.Unmarshal(reg, &acct)
		} else if !errors.Is(readErr, fs.ErrNotExist) {
			err = readErr
		}
		if err == nil {
			parsed, keyErr := parseKey(key)
			ec, ok := parsed.(*ecdsa.PrivateKey)
			if keyErr != nil || !ok {
				return acme.Account{}, errors.New("invalid stored ACME account key")
			}
			acct.PrivateKey = ec
		}
	}
	if err != nil || acct.Location != "" {
		return acct, err
	}
	acct, err = client.NewAccount(ctx, acct)
	if err != nil {
		return acct, err
	}
	reg, err := json.Marshal(acct)
	if err == nil {
		err = storage.write("accounts", regName, reg)
	}
	if err == nil {
		m.logger.Info("new ACME account registered")
	}
	return acct, err
}

func (m *acmeClient) RenewalInfo(ctx context.Context, account ACMEAccount, _ string, leaf *x509.Certificate) (time.Time, time.Time, error) {
	client, _, stop := m.client(account)
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
	active *activeChallenge
}

func (s *challengeSolver) Present(ctx context.Context, challenge acme.Challenge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if challenge.Identifier.Value != s.host {
		return errors.New("ACME challenge identifier does not match the order")
	}
	s.active = &activeChallenge{Challenge: challenge, ctx: ctx, log: s.log}
	s.client.active.Store(s.active)
	return nil
}

func (s *challengeSolver) CleanUp(_ context.Context, challenge acme.Challenge) error {
	if active := s.active; active != nil && active.URL == challenge.URL &&
		active.Type == challenge.Type && active.Token == challenge.Token {
		s.clear()
	}
	return nil
}

func (s *challengeSolver) clear() {
	if s.active != nil {
		s.client.active.CompareAndSwap(s.active, nil)
		s.active = nil
	}
}

// HTTPChallenge only accepts the current authorization's exact GET path and
// identifier while the order is live.
func (m *acmeClient) HTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	attempt := m.active.Load()
	if attempt == nil || attempt.ctx.Err() != nil || r.Method != http.MethodGet || m.listeners.HTTP == "" {
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
	if m.active.Load() != attempt || attempt.ctx.Err() != nil {
		return false
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, attempt.KeyAuthorization)
	return true
}

func (m *acmeClient) TLSChallenge(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	attempt := m.active.Load()
	if attempt == nil || attempt.ctx.Err() != nil || m.listeners.ALPN == "" {
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
	cert, err := acmez.TLSALPN01ChallengeCert(attempt.Challenge)
	if m.active.Load() != attempt || attempt.ctx.Err() != nil {
		return nil, nil
	}
	return cert, err
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
