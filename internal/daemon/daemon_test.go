package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

type noDocker struct{}

func (noDocker) Reachable(context.Context) bool { return false }

func TestRunSelectsManagedTLSForNonLoopback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, done := make(chan net.Addr, 1), make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir: dir, Listen: "0.0.0.0:0", docker: noDocker{}, Logger: slog.New(slog.DiscardHandler),
			Ready: func(addr net.Addr) { ready <- addr },
		})
	}()
	var port int
	select {
	case addr := <-ready:
		port = addr.(*net.TCPAddr).Port
	case err := <-done:
		t.Fatalf("daemon stopped during startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("managed TLS listener did not start")
	}
	cert, err := os.ReadFile(filepath.Join((datadir.Layout{Root: dir}).PanelCerts(), "fallback.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		t.Fatal("fallback certificate could not be loaded")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS status returned %s", resp.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	if err := Run(context.Background(), Options{DataDir: dir, Listen: "127.0.0.1:0", TLSCert: "c.pem"}); err == nil {
		t.Fatal("cert without key accepted")
	}
	if err := Run(t.Context(), Options{DataDir: dir, Listen: "127.0.0.1:0", HTTPListen: "127.0.0.1:0"}); err == nil {
		t.Fatal("HTTP redirect listener without HTTPS accepted")
	}
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	// A fresh path: t.TempDir() itself may be group/world-readable under the
	// umask, which Prepare refuses on Unix.
	dir := filepath.Join(t.TempDir(), "data")
	// A job left running by a crashed daemon.
	layout, err := datadir.Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := st.CreateJob(context.Background(), store.NewJob{Kind: "project.pull", SubjectType: "project", SubjectID: "1"})
	_ = st.MarkJobRunning(context.Background(), stale.ID)
	_ = st.Close()

	var logs strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	closed := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir:     dir,
			Listen:      "127.0.0.1:0",
			Version:     "v-test",
			Logger:      slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)),
			Ready:       func(a net.Addr) { addrCh <- a },
			bcryptCost:  bcrypt.MinCost,
			docker:      noDocker{},
			dockerCLI:   dockertest.New(),
			storeClosed: func(err error) { closed <- err },
		})
	}()
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("Run exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	resp, err := http.Get("http://" + addr.String() + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"unauthenticated"`) {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	for _, f := range []string{layout.JWTKey(), layout.SecretsKey()} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("key not created: %v", err)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("store close: %v", err)
		}
	default:
		t.Fatal("store was not closed")
	}

	out := logs.String()
	for _, want := range []string{"no approved admin account exists", "interrupted_jobs=1", "golem-deploy stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	st, err = store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	j, _ := st.GetJob(context.Background(), stale.ID)
	if j.Status != store.JobError || j.Detail != store.InterruptedDetail {
		t.Fatalf("stale job %+v", j)
	}
}

func TestAdminCreateAndReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx := context.Background()
	acct, err := createAdmin(ctx, dir, "Root-Admin", "a-very-good-password", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Username != "root-admin" || acct.Role != store.RoleAdmin || !acct.Approved {
		t.Fatalf("account %+v", acct)
	}
	if _, err := createAdmin(ctx, dir, "root-admin", "a-very-good-password", bcrypt.MinCost); !errors.Is(err, ErrUsernameExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := createAdmin(ctx, dir, "x", "a-very-good-password", bcrypt.MinCost); err == nil {
		t.Fatal("bad username accepted")
	}
	if _, err := createAdmin(ctx, dir, "other", "short", bcrypt.MinCost); err == nil {
		t.Fatal("short password accepted")
	}
	if _, err := resetPassword(ctx, dir, "ROOT-ADMIN", "another-good-password", bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	if _, err := resetPassword(ctx, dir, "ghost", "another-good-password", bcrypt.MinCost); !errors.Is(err, ErrNoSuchAccount) {
		t.Fatalf("missing user: %v", err)
	}
	layout, _ := datadir.Prepare(dir)
	st, err := store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, _ := st.GetAccount(ctx, acct.ID)
	if got.TokenVersion != acct.TokenVersion+1 || bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte("another-good-password")) != nil {
		t.Fatalf("reset not applied: %+v", got)
	}
}

// TestF5_CLIResetPasswordDeletesTokens: the local recovery action ends
// sessions and deletes every API token of the account.
func TestF5_CLIResetPasswordDeletesTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx := context.Background()
	acct, err := createAdmin(ctx, dir, "root", "a-very-good-password", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := datadir.Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"h1", "h2"} {
		if _, err := st.CreateAPIToken(ctx, store.NewAPIToken{AccountID: acct.ID, Name: "ci", TokenHash: h, Prefix: "gdp_x"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	n, err := resetPassword(ctx, dir, "root", "another-good-password", bcrypt.MinCost)
	if err != nil || n != 2 {
		t.Fatalf("reset: %d %v", n, err)
	}
	st, err = store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if toks, _ := st.ListAPITokens(ctx, acct.ID); len(toks) != 0 {
		t.Fatalf("tokens survived the reset: %d", len(toks))
	}
}
