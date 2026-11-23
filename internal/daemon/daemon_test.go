package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/httpapi"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

type noDocker struct{}

func (noDocker) Reachable(context.Context) bool { return false }

func TestRunRefusesPlaintextNonLoopback(t *testing.T) {
	dir := t.TempDir()
	err := Run(context.Background(), Options{DataDir: dir, Listen: "0.0.0.0:0", Logger: slog.New(slog.DiscardHandler)})
	if !errors.Is(err, httpapi.ErrPlaintextNonLoopback) {
		t.Fatalf("got %v", err)
	}
	// Refused before touching the data directory.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("data dir written: %v", entries)
	}
	if err := Run(context.Background(), Options{DataDir: dir, Listen: "127.0.0.1:0", TLSCert: "c.pem"}); err == nil {
		t.Fatal("cert without key accepted")
	}
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	dir := t.TempDir()
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
	if err := resetPassword(ctx, dir, "ROOT-ADMIN", "another-good-password", bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	if err := resetPassword(ctx, dir, "ghost", "another-good-password", bcrypt.MinCost); !errors.Is(err, ErrNoSuchAccount) {
		t.Fatalf("missing user: %v", err)
	}
	layout, _ := datadir.Prepare(dir)
	st, err := store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, _ := st.GetAccount(ctx, acct.ID)
	if got.TokenVersion != 1 || bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte("another-good-password")) != nil {
		t.Fatalf("reset not applied: %+v", got)
	}
}
