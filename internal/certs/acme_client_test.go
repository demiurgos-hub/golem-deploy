package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
	"github.com/mholt/acmez/v3/acme"
)

func testACMEPair(t *testing.T, host string, now time.Time) *certificatePair {
	t.Helper()
	key, caKey := newKey(t, ecP256), newKey(t, ecP256)
	opts := certOpts{host: host, notBefore: now.Add(-time.Hour), notAfter: now.Add(SelfSignedValidity - time.Hour)}
	caOpts := opts
	caOpts.isCA = true
	ca, caPEM := makeCert(t, caKey, nil, nil, caOpts)
	_, leafPEM := makeCert(t, key, ca, caKey, opts)
	pair, err := parsePair(slices.Concat(leafPEM, caPEM), keyPEM(t, key, "PRIVATE KEY"))
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func mockACMEDirectory(t *testing.T, ari bool, handle func(http.ResponseWriter, *http.Request, string)) string {
	t.Helper()
	var nonce atomic.Int64
	var accounts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.Header().Set("Replay-Nonce", fmt.Sprint(nonce.Add(1)))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/dir":
			dir := acme.Directory{NewNonce: base + "/nonce", NewAccount: base + "/account", NewOrder: base + "/order"}
			if ari {
				dir.RenewalInfo = base + "/renewal"
			}
			_ = json.NewEncoder(w).Encode(dir)
		case "/nonce":
			w.WriteHeader(http.StatusNoContent)
		case "/account":
			w.Header().Set("Location", fmt.Sprintf("%s/account/%d", base, accounts.Add(1)))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"status":"valid"}`)
		default:
			handle(w, r, base)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL + "/dir"
}

func testACMEClient(t *testing.T) *acmeClient {
	t.Helper()
	return NewACMEClient(t.TempDir(), nil, slog.New(slog.DiscardHandler), ACMEListeners{HTTP: ":80", ALPN: ":443"}).(*acmeClient)
}

func TestACMEReplacesOnlyIssuingAccountAndRetriesRejectedOrder(t *testing.T) {
	var mu sync.Mutex
	var orders []acme.Order
	url := mockACMEDirectory(t, true, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path != "/order" {
			http.NotFound(w, r)
			return
		}
		var order acme.Order
		if err := decodeACMEPayload(r.Body, &order); err != nil {
			t.Error(err)
		}
		mu.Lock()
		orders = append(orders, order)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"urn:ietf:params:acme:error:malformed","detail":"order rejected"}`)
	})
	client := testACMEClient(t)
	account := ACMEAccount{Directory: url, Email: "original@example.com"}
	host := "game.example.com"
	pair := testACMEPair(t, host, time.Now())
	issuingURL := strings.TrimSuffix(url, "/dir") + "/account/1"
	storage := client.storage(account)
	if err := storage.savePair(host, storedACMEPair{AccountURL: issuingURL, Chain: pair.chain, Key: pair.key}); err != nil {
		t.Fatal(err)
	}
	if err := client.Issue(t.Context(), account, host, true, io.Discard); err == nil {
		t.Fatal("expected the mock CA to reject both orders")
	}
	account.Email = "changed@example.com"
	if err := client.Issue(t.Context(), account, host, true, io.Discard); err == nil {
		t.Fatal("expected the mock CA to reject the order")
	}
	mu.Lock()
	defer mu.Unlock()
	want, err := acme.ARIUniqueIdentifier(pair.info.leaf)
	if err != nil || len(orders) != 3 || orders[0].Replaces != want || orders[1].Replaces != "" || orders[2].Replaces != "" {
		t.Fatalf("unexpected replacement hints: %v, ARI ID %q (%v)", orders, want, err)
	}
}

func TestACMERateLimitedOrderIsNotRetriedWithoutReplaces(t *testing.T) {
	var orders atomic.Int32
	url := mockACMEDirectory(t, true, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path != "/order" {
			http.NotFound(w, r)
			return
		}
		orders.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"urn:ietf:params:acme:error:rateLimited","detail":"too many orders"}`)
	})
	client := testACMEClient(t)
	account := ACMEAccount{Directory: url, Email: "original@example.com"}
	host := "game.example.com"
	pair := testACMEPair(t, host, time.Now())
	issuingURL := strings.TrimSuffix(url, "/dir") + "/account/1"
	if err := client.storage(account).savePair(host, storedACMEPair{AccountURL: issuingURL, Chain: pair.chain, Key: pair.key}); err != nil {
		t.Fatal(err)
	}
	err := client.Issue(t.Context(), account, host, true, io.Discard)
	failure := summarizeACME(err)
	if err == nil || orders.Load() != 1 {
		t.Fatalf("a rate-limited order was sent %d times (err %v), want once", orders.Load(), err)
	}
	if left := time.Until(failure.retry); left < 50*time.Minute || left > time.Hour+time.Minute {
		t.Fatalf("the CA's Retry-After was not kept: retry in %v", left)
	}
}

func TestACMEStorageSeparatesAccountsAndDirectories(t *testing.T) {
	url := mockACMEDirectory(t, false, httpNotFoundACME)
	client := testACMEClient(t)
	a := ACMEAccount{Directory: url, Email: "a+b@example.com"}
	b := ACMEAccount{Directory: url, Email: "a_plus_b@example.com"}
	ac, _, stop := client.client(a)
	defer stop()
	storage := client.storage(a)
	load := func(email string) acme.Account {
		t.Helper()
		account, err := client.loadAccount(t.Context(), ac, storage, email)
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	first, second, again := load(a.Email), load(b.Email), load(strings.ToUpper(a.Email))
	if first.Location == second.Location || first.PrivateKey.Public().(*ecdsa.PublicKey).Equal(second.PrivateKey.Public()) {
		t.Fatal("distinct contact addresses reused an ACME registration or key")
	}
	if first.Location != again.Location || !first.PrivateKey.Public().(*ecdsa.PublicKey).Equal(again.PrivateKey.Public()) {
		t.Fatal("normalized address did not reuse the account")
	}
	host := "game.example.com"
	pair := testACMEPair(t, host, time.Now())
	if err := storage.savePair(host, storedACMEPair{AccountURL: first.Location, Chain: pair.chain, Key: pair.key}); err != nil {
		t.Fatal(err)
	}
	if chain, key, err := client.Load(t.Context(), b, host); err != nil || !bytes.Equal(chain, pair.chain) || !bytes.Equal(key, pair.key) {
		t.Fatalf("the pair should be shared within the directory: %v", err)
	}
	b.Directory += "/separate"
	if _, _, err := client.Load(t.Context(), b, host); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pair crossed directory boundaries: %v", err)
	}
	if runtime.GOOS != "windows" {
		err := filepath.WalkDir(client.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			want := fs.FileMode(0o600)
			if d.IsDir() {
				want = 0o700
			}
			if fi.Mode().Perm() != want {
				t.Errorf("permission of %s = %o, want %o", path, fi.Mode().Perm(), want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func httpNotFoundACME(w http.ResponseWriter, r *http.Request, _ string) { http.NotFound(w, r) }

func TestACMERejectsUnrelatedChainBeforeSaveOrInstall(t *testing.T) {
	game, clock, st := newManager(t)
	clock.add(time.Since(clock.now()))
	host := "game.example.com"
	now := time.Now()
	unrelated := testACMEPair(t, "unrelated.example.com", now).chain
	signer := newKey(t, ecP256)
	issuer, _ := makeCert(t, signer, nil, nil, certOpts{isCA: true, notBefore: now.Add(-time.Hour), notAfter: now.Add(time.Hour)})
	var issued []byte
	var issuedMu sync.Mutex
	url := mockACMEDirectory(t, false, func(w http.ResponseWriter, r *http.Request, base string) {
		order := acme.Order{Identifiers: []acme.Identifier{{Type: "dns", Value: host}}, Finalize: base + "/finalize"}
		switch r.URL.Path {
		case "/order":
			order.Status = acme.StatusReady
			w.Header().Set("Location", base+"/order/1")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(order)
		case "/finalize":
			var payload struct{ CSR string }
			if err := decodeACMEPayload(r.Body, &payload); err != nil {
				t.Error(err)
				return
			}
			der, _ := base64.RawURLEncoding.DecodeString(payload.CSR)
			csr, err := x509.ParseCertificateRequest(der)
			if err != nil {
				t.Error(err)
				return
			}
			template := *issuer
			template.AuthorityKeyId, template.SubjectKeyId, template.IsCA = nil, nil, false
			template.DNSNames = csr.DNSNames
			der, err = x509.CreateCertificate(rand.Reader, &template, issuer, csr.PublicKey, signer)
			if err != nil {
				t.Error(err)
				return
			}
			issuedMu.Lock()
			issued = slices.Concat(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), unrelated)
			issuedMu.Unlock()
			order.Status, order.Certificate = acme.StatusValid, base+"/cert"
			_ = json.NewEncoder(w).Encode(order)
		case "/cert":
			w.Header().Set("Content-Type", "application/pem-certificate-chain")
			issuedMu.Lock()
			defer issuedMu.Unlock()
			_, _ = w.Write(issued)
		default:
			http.NotFound(w, r)
		}
	})
	client := testACMEClient(t)
	a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(host), Client: client, Directory: url,
		PanelMode: apitypes.PanelCertificateModeNone, HTTP01: true})
	account := (accountSettings{}).account(url)
	old := testACMEPair(t, host, now)
	if _, err := game.useShared(t.Context(), ModeACME, old.chain, old.key); err != nil {
		t.Fatal(err)
	}
	st.m[store.SettingACME] = `{"TermsAgreed":true}`
	if err := client.storage(account).savePair(host, storedACMEPair{
		AccountURL: strings.TrimSuffix(url, "/dir") + "/account/1", Chain: old.chain, Key: old.key,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.run(t.Context(), RenewAll, nil, io.Discard); err == nil || !strings.Contains(err.Error(), "usable certificate") {
		t.Fatalf("invalid issuance was not rejected with a bounded error: %v", err)
	}
	if chain, _, err := client.Load(t.Context(), account, host); err != nil || !bytes.Equal(chain, old.chain) {
		t.Fatalf("invalid renewal replaced CA storage: %v", err)
	}
	if view, err := game.Current(t.Context(), 0); err != nil || view.Fingerprint != old.info.Fingerprint {
		t.Fatalf("invalid renewal replaced the installed certificate: %+v, %v", view, err)
	}
}

func TestACMERequiresExactlyItsRequestedIdentifier(t *testing.T) {
	game, clock, _ := newManager(t)
	const host = "game.example.com"
	now := clock.now()
	wildcard := testACMEPair(t, "*.example.com", now)
	chain, key, _, err := game.generate(host) // Includes localhost and loopback SANs.
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ chain, key []byte }{{wildcard.chain, wildcard.key}, {chain, key}} {
		if candidate, err := parsePair(pair.chain, pair.key); err != nil || !candidate.info.Covers(host) {
			t.Fatalf("bad test fixture: %v", err)
		}
		if _, err := validateACMEPair(pair.chain, pair.key, host, now); err == nil {
			t.Fatal("accepted a leaf whose identifiers exceed the order")
		}
	}
}

func TestACMEIPProfileDependsOnDirectory(t *testing.T) {
	// Fake the built-in directory metadata without contacting Let's Encrypt.
	for _, dir := range []string{letsEncryptProduction, letsEncryptStaging} {
		if _, err := orderProfile(ACMEAccount{Directory: dir}, acme.Directory{}, "8.8.8.8"); err == nil ||
			!strings.Contains(operationError(err), "shortlived") {
			t.Fatalf("missing profile did not explain the built-in CA failure: %v", err)
		}
	}
	called := make(chan acme.Order, 1)
	url := mockACMEDirectory(t, false, func(w http.ResponseWriter, r *http.Request, _ string) {
		var order acme.Order
		if err := decodeACMEPayload(r.Body, &order); err != nil {
			t.Error(err)
		}
		called <- order
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"urn:ietf:params:acme:error:malformed"}`)
	})
	_ = testACMEClient(t).Issue(t.Context(), ACMEAccount{Directory: url, DirectoryOverride: true}, "127.0.0.1", false, io.Discard)
	select {
	case order := <-called:
		if order.Profile != "" || len(order.Identifiers) != 1 || order.Identifiers[0].Type != "ip" {
			t.Fatalf("unexpected private CA order: %+v", order)
		}
	default:
		t.Fatal("no order reached the private CA without shortlived")
	}
}

func TestACMECancelledOrderDeactivatesAuthorization(t *testing.T) {
	host := "game.example.com"
	started := make(chan struct{})
	deactivated := make(chan struct{}, 1)
	url := mockACMEDirectory(t, false, func(w http.ResponseWriter, r *http.Request, base string) {
		auth := acme.Authorization{Status: acme.StatusPending, Identifier: acme.Identifier{Type: "dns", Value: host},
			Challenges: []acme.Challenge{{Type: acme.ChallengeTypeHTTP01, URL: base + "/challenge", Token: "token"}}}
		switch r.URL.Path {
		case "/order":
			w.Header().Set("Location", base+"/order/1")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(acme.Order{Status: acme.StatusPending, Authorizations: []string{base + "/auth"}})
		case "/auth":
			var payload struct{ Status string }
			if decodeACMEPayload(r.Body, &payload) == nil && payload.Status == acme.StatusDeactivated {
				auth.Status = acme.StatusDeactivated
				deactivated <- struct{}{}
			}
			_ = json.NewEncoder(w).Encode(auth)
		case "/challenge":
			_, _ = io.Copy(io.Discard, r.Body)
			close(started)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	})
	client := testACMEClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- client.Issue(ctx, ACMEAccount{Directory: url}, host, false, io.Discard)
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("ended before starting challenge: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("challenge did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || client.active.Load() != nil {
			t.Fatalf("cancelled order did not clear its challenge: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("order did not exit within cleanup deadline")
	}
	select {
	case <-deactivated:
	default:
		t.Fatal("the cancelled authorization was never deactivated")
	}
}

func TestACMEChallengeRequiresLiveOrderAndMatchingCleanup(t *testing.T) {
	client := testACMEClient(t)
	const host = "game.example.com"
	solver := &challengeSolver{client: client, host: host, log: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	challenge := acme.Challenge{Type: acme.ChallengeTypeHTTP01, Token: "first", KeyAuthorization: "first.key",
		Identifier: acme.Identifier{Type: "dns", Value: host}}
	request := httptest.NewRequest(http.MethodGet, "http://"+host+challenge.HTTP01ResourcePath(), nil)
	if err := solver.Present(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if !client.HTTPChallenge(httptest.NewRecorder(), request) {
		t.Fatal("live challenge not served")
	}
	cancel()
	if client.HTTPChallenge(httptest.NewRecorder(), request) {
		t.Fatal("cancelled challenge served before cleanup")
	}
	newSolver := &challengeSolver{client: client, host: host, log: io.Discard}
	alpn, cancelALPN := context.WithCancel(t.Context())
	defer cancelALPN()
	next := challenge
	next.Type, next.Token = acme.ChallengeTypeTLSALPN01, "second"
	if err := newSolver.Present(alpn, next); err != nil {
		t.Fatal(err)
	}
	_ = solver.CleanUp(ctx, challenge)
	hello := &tls.ClientHelloInfo{ServerName: host}
	if got, err := client.TLSChallenge(hello); got == nil || err != nil {
		t.Fatalf("cleaning up the old challenge removed the live one: %v", err)
	}
	cancelALPN()
	if got, err := client.TLSChallenge(hello); got != nil || err != nil {
		t.Fatalf("cancelled TLS-ALPN challenge served before cleanup: %v", err)
	}
}
