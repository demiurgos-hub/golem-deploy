package servers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// ctxRunner behaves like ExecRunner for a done context: the command is
// killed and Run returns the process error. A set cancelOnRun is called
// first, as if the caller went away while the command ran.
type ctxRunner struct {
	*dockertest.Fake
	cancelOnRun *context.CancelFunc
}

func (r ctxRunner) Run(ctx context.Context, path string, cmd docker.Cmd) error {
	if c := *r.cancelOnRun; c != nil {
		c()
	}
	if ctx.Err() != nil {
		return errors.New("signal: killed")
	}
	return r.Fake.Run(ctx, path, cmd)
}

func newLoggedService(t *testing.T, logs *bytes.Buffer, cancelOnRun *context.CancelFunc) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	f := dockertest.New()
	dc := docker.New(docker.Options{Runner: ctxRunner{f, cancelOnRun}, InstanceID: dockertest.InstanceID, TempDir: t.TempDir()})
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := Config{Ports: ports.Range{Lo: 7000, Hi: 7009}, ProbePort: func(string, int) error { return nil }}
	return New(st, box, dc, cfg, logger), st
}

func TestListCancelledByCallerIsNotAWarning(t *testing.T) {
	var logs bytes.Buffer
	var cancel context.CancelFunc
	svc, _ := newLoggedService(t, &logs, &cancel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := svc.List(ctx) // the store read succeeds, docker ps is cancelled
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("List with a cancelled context: %v, want context.Canceled", err)
	}
	if !strings.Contains(logs.String(), "docker query cancelled with its caller") {
		t.Fatalf("the docker query was not the one cancelled:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("cancellation logged as a problem:\n%s", logs.String())
	}
}

func TestDockerTimeoutStillWarns(t *testing.T) {
	var logs bytes.Buffer
	var none context.CancelFunc
	svc, _ := newLoggedService(t, &logs, &none)
	// A deadline is the docker layer's own timeout, not a caller leaving.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, derr := svc.docker.ManagedContainers(ctx)
	if !errors.Is(derr, context.DeadlineExceeded) {
		t.Fatalf("docker error %v, want context.DeadlineExceeded", derr)
	}
	if err := svc.dockerFailed(ctx, "list servers", derr); err != nil {
		t.Fatalf("a timeout must be reported as unknown state, not end the request: %v", err)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("timeout not reported:\n%s", logs.String())
	}
}
