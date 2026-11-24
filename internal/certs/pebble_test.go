package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/miekg/dns"
)

const pebbleVersion = "v2.10.3"

type progressLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *progressLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *progressLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func (b *progressLog) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Buffer.Reset()
}

func testListener(t *testing.T) *net.TCPListener {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func startPebble(t *testing.T, httpPort, tlsPort int) (string, *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	build := exec.CommandContext(t.Context(), "go", "install", "github.com/letsencrypt/pebble/v2/cmd/pebble@"+pebbleVersion)
	build.Env = append(os.Environ(), "GOBIN="+dir)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("install Pebble: %v\n%s", err, out)
	}
	pebble := filepath.Join(dir, "pebble")
	if runtime.GOOS == "windows" {
		pebble += ".exe"
	}

	// Local authoritative answers mean the test needs no external DNS or
	// modifications to /etc/hosts on either platform.
	dnsListener := testListener(t)
	dnsServer := &dns.Server{Listener: dnsListener, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(q)
		for _, question := range q.Question {
			if question.Qtype == dns.TypeA {
				r.Answer = append(r.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(127, 0, 0, 1)})
			}
		}
		_ = w.WriteMsg(r)
	})}
	go func() { _ = dnsServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = dnsServer.Shutdown() })

	// The Pebble API serves TLS using this run's private trust anchor.
	generator := &Manager{now: time.Now}
	chain, key, _, err := generator.generate("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	slot := testListener(t)
	addr := slot.Addr().String()
	_ = slot.Close()
	config, err := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress": addr, "certificate": write("server.crt", chain), "privateKey": write("server.key", key),
		"httpPort": httpPort, "tlsPort": tlsPort, "keyAlgorithm": "ecdsa",
		"retryAfter":      map[string]int{"authz": 1, "order": 1},
		"domainBlocklist": []string{"blocked.example.test"},
		"profiles": map[string]any{
			"default":    map[string]any{"description": "Default", "validityPeriod": 90 * 86400},
			"shortlived": map[string]any{"description": "Short", "validityPeriod": 160 * 3600},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), pebble, "-config", write("pebble.json", config), "-dnsserver", dnsListener.Addr().String())
	cmd.Env = append(os.Environ(), "PEBBLE_VA_ALWAYS_VALID=0", "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0")
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(out.String())
		}
	})
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(chain) {
		t.Fatal("could not load Pebble root")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	directory := "https://" + addr + "/dir"
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(directory)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return directory, roots
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Pebble did not serve its directory")
	return "", nil
}

func TestPebbleCertificates(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_ACME_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_ACME_TESTS=1 to run the native Pebble integration test")
	}
	hln, tln := testListener(t), testListener(t)
	directory, roots := startPebble(t, hln.Addr().(*net.TCPAddr).Port, tln.Addr().(*net.TCPAddr).Port)
	logger := slog.New(slog.DiscardHandler)
	st := &memSettings{m: map[string]string{}, ids: []int64{1}}
	game, err := Open(Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: st})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	storage := filepath.Join(t.TempDir(), "acme")
	if err := os.Mkdir(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	hclient := NewACMEClient(storage, roots, logger, ACMEListeners{HTTP: hln.Addr().String()})
	tclient := NewACMEClient(storage, roots, logger, ACMEListeners{ALPN: tln.Addr().String()})
	ip := "127.0.0.1"
	// Pebble randomly picks from its configured profiles when the order
	// omits a profile. Assert the issuer configuration as well as the IP
	// lifetime so the random choice cannot make an incorrect request pass.
	for _, tc := range []struct{ name, profile string }{{ip, "shortlived"}, {"panel.example.test", ""}} {
		_, issuer, stop := hclient.(*magicClient).config(ACMEAccount{Directory: directory}, tc.name, nil)
		if issuer.Profile != tc.profile {
			stop()
			t.Fatalf("profile for %s was %q, want %q", tc.name, issuer.Profile, tc.profile)
		}
		stop()
	}
	a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(ip), PanelMode: apitypes.PanelCertificateModeNone,
		Directory: directory, Client: hclient, HTTP01: true})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	agree := true
	if _, err := a.UpdateSettings(ctx, apitypes.UpdateACMERequest{TermsAgreed: &agree}); err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hclient.HTTPChallenge(w, r) {
			http.NotFound(w, r)
		}
	})}
	go func() { _ = hs.Serve(hln) }()
	t.Cleanup(func() { _ = hs.Close() })

	var progress progressLog
	if err := a.run(ctx, IssueGame, []string{ip}, &progress); err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	first, err := game.Current(ctx, 0)
	if err != nil || !first.Covers(ip) || first.SelfSigned || first.NotAfter.Sub(first.NotBefore).Round(time.Hour) != 160*time.Hour {
		t.Fatalf("IP was not issued under shortlived: %+v, %v", first, err)
	}
	if !strings.Contains(progress.String(), "Answering HTTP-01") {
		t.Fatal(progress.String())
	}
	keyBefore, err := os.ReadFile(filepath.Join(game.dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}

	if err := a.run(ctx, RenewAll, []string{ip}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{game.dir, game.ServerDir(1)} {
		current, err := load(path)
		if err != nil || current.Fingerprint == first.Fingerprint || !current.Covers(ip) {
			t.Fatalf("not renewed at %s: %v", path, err)
		}
		key, err := os.ReadFile(filepath.Join(path, KeyFile))
		if err != nil || bytes.Equal(key, keyBefore) {
			t.Fatalf("key was not replaced at %s: %v", path, err)
		}
	}

	host := "panel.example.test"
	panel := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(ip), PublicHosts: []string{host, ip},
		PanelMode: apitypes.PanelCertificateModeSelfSigned, PanelDir: filepath.Join(filepath.Dir(game.dir), "panel"),
		Directory: directory, Client: tclient, TLSALPN01: true})
	if err := panel.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	ts := &http.Server{ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"acme-tls/1"}, GetCertificate: panel.GetCertificate}}
	go func() { _ = ts.ServeTLS(tln, "", "") }()
	t.Cleanup(func() { _ = ts.Close() })
	progress.Reset()
	if err := panel.run(ctx, IssuePanel, []string{host, ip}, &progress); err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	view, err := panel.Panel(ctx)
	if err != nil || view.Mode != "acme" || view.Certificate == nil || view.Certificate.SelfSigned ||
		!slices.Contains(view.Certificate.DNSNames, host) || !strings.Contains(progress.String(), "Answering TLS-ALPN-01") {
		t.Fatalf("unexpected panel issuance: %+v, %v\n%s", view, err, progress.String())
	}
	current, err := game.Current(ctx, 0)
	if err != nil || panel.panel.Load().issued[ip].info.Fingerprint != current.Fingerprint {
		t.Fatal("game and panel did not share the certificate for the IP")
	}
	if _, err := panel.UsePanelSelfSigned(ctx); err != nil {
		t.Fatal(err)
	}
	panel.PublicHosts = []string{"blocked.example.test"}
	err = panel.run(ctx, IssuePanel, panel.PublicHosts, io.Discard)
	if err == nil || !strings.Contains(operationError(err), "ACME") || strings.Contains(operationError(err), "PRIVATE KEY") {
		t.Fatalf("expected a user-safe rejection, got %v", err)
	}
	view, err = panel.Panel(ctx)
	if err != nil || view.Mode != "self_signed" {
		t.Fatalf("failed issuance changed mode: %+v, %v", view, err)
	}
	if runtime.GOOS != "windows" {
		err := filepath.WalkDir(storage, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err == nil && fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("ACME storage is not private: %s (%o)", path, fi.Mode().Perm())
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Log("Pebble verified IP shortlived with HTTP-01, CA-selected DNS with TLS-ALPN-01, shared game and panel use, renewed canonical and mounted pairs, and unchanged mode after rejection")
}
