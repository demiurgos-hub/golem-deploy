package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func testOptions(dir string) Options {
	return Options{
		DataDir:    dir,
		Listen:     "127.0.0.1:0",
		Logger:     slog.New(slog.DiscardHandler),
		bcryptCost: bcrypt.MinCost,
		docker:     noDocker{},
		dockerCLI:  dockertest.New(),
	}
}

// TestG4_SecondServeRefusedBeforeSweeps: a second serve on the data
// directory of a running one fails with an error naming the directory,
// before reconciliation and the startup sweeps touch the first daemon's
// jobs, staging directories and orphan checkouts. The admin CLI keeps
// working while serve holds the lock.
func TestG4_SecondServeRefusedBeforeSweeps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	first := testOptions(dir)
	first.Ready = func(a net.Addr) { ready <- a }
	go func() { done <- Run(ctx, first) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("first serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("first serve not ready")
	}

	// Live state of the first daemon that the sweeps would delete or mark.
	layout, err := datadir.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(layout.Tmp(), "git-live")
	orphan := filepath.Join(layout.Repositories(), "77")
	for _, d := range []string{staging, orphan} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	job, _ := st.CreateJob(context.Background(), store.NewJob{Kind: "project.pull", SubjectType: "project", SubjectID: "1"})
	_ = st.MarkJobRunning(context.Background(), job.ID)

	err = Run(context.Background(), testOptions(dir))
	if !errors.Is(err, datadir.ErrLocked) || !strings.Contains(err.Error(), layout.Root) {
		t.Fatalf("second serve: %v", err)
	}
	for _, d := range []string{staging, orphan} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("second serve swept %s", d)
		}
	}
	if j, _ := st.GetJob(context.Background(), job.ID); j.Status != store.JobRunning {
		t.Errorf("second serve reconciled the first one's job: %+v", j)
	}
	_ = st.Close()

	// The admin CLI does not take the lock.
	if _, err := createAdmin(context.Background(), dir, "root", "a-very-good-password", bcrypt.MinCost); err != nil {
		t.Fatalf("admin create while serving: %v", err)
	}
	if _, err := resetPassword(context.Background(), dir, "root", "another-good-password", bcrypt.MinCost); err != nil {
		t.Fatalf("admin reset-password while serving: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("first serve did not stop")
	}
	// Released on exit: a new serve starts.
	ctx2, cancel2 := context.WithCancel(context.Background())
	again := testOptions(dir)
	again.Ready = func(net.Addr) { cancel2() }
	if err := Run(ctx2, again); err != nil {
		t.Fatalf("serve after the first stopped: %v", err)
	}
}

// TestG8_ServeRequiresDedicatedDataDir: serve refuses a non-empty
// directory it did not initialise, and a path that cannot be written into
// the SSH configuration, without writing anything.
func TestG8_ServeRequiresDedicatedDataDir(t *testing.T) {
	unrelated := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(unrelated, "tmp", "git-precious"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(unrelated, "notes.txt"), []byte("mine"), 0o600)
	err := Run(context.Background(), testOptions(unrelated))
	if err == nil || !strings.Contains(err.Error(), "must be empty or previously initialised by Golem Deploy") {
		t.Fatalf("unrelated dir: %v", err)
	}
	entries, _ := os.ReadDir(unrelated)
	if len(entries) != 2 {
		t.Fatalf("refused directory was written to: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(unrelated, "tmp", "git-precious")); err != nil {
		t.Fatal("refused directory was swept")
	}

	bad := filepath.Join(t.TempDir(), "a$HOME")
	err = Run(context.Background(), testOptions(bad))
	if err == nil || !strings.Contains(err.Error(), "SSH configuration") {
		t.Fatalf("unsafe path: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("unsafe data dir created")
	}
}

// TestG8_AdminDataDirRules: admin create may initialise a new or empty
// data directory (the first admin is created before the first serve);
// admin reset-password requires an initialised one; neither touches an
// unrelated directory.
func TestG8_AdminDataDirRules(t *testing.T) {
	ctx := context.Background()
	fresh := filepath.Join(t.TempDir(), "data")
	if _, err := resetPassword(ctx, fresh, "root", "a-very-good-password", bcrypt.MinCost); err == nil {
		t.Fatal("reset-password on a missing data dir")
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("reset-password created the data dir")
	}
	if _, err := createAdmin(ctx, fresh, "root", "a-very-good-password", bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh, datadir.MarkerName)); err != nil {
		t.Fatal("admin create did not mark the data dir")
	}
	unrelated := t.TempDir()
	_ = os.WriteFile(filepath.Join(unrelated, "notes.txt"), nil, 0o600)
	if _, err := createAdmin(ctx, unrelated, "root", "a-very-good-password", bcrypt.MinCost); err == nil {
		t.Fatal("admin create used an unrelated directory")
	}
	if entries, _ := os.ReadDir(unrelated); len(entries) != 1 {
		t.Fatal("admin create wrote to an unrelated directory")
	}
}
