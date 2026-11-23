package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func TestRunRejectsBadServerSettings(t *testing.T) {
	cases := []Options{
		{Listen: "127.0.0.1:8000", PortRange: "80-90"},
		{Listen: "127.0.0.1:8000", PortRange: "40000-40010"},
		{Listen: "127.0.0.1:7050"}, // default range contains the panel port
		{Listen: "127.0.0.1:8000", PortBindAddress: "example.com"},
		{Listen: "127.0.0.1:8000", ServerMemoryMB: 4},
		{Listen: "127.0.0.1:8000", ServerCPUs: -1},
	}
	for i, o := range cases {
		dir := t.TempDir()
		o.DataDir = dir
		o.Logger = slog.New(slog.DiscardHandler)
		if err := Run(context.Background(), o); err == nil {
			t.Errorf("case %d accepted", i)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("case %d: data dir written before validation", i)
		}
	}
}

// Startup reconciliation starts servers that should run and leaves
// orphaned managed containers alone.
func TestRunReconcilesServers(t *testing.T) {
	// A fresh path: t.TempDir() itself may be group/world-readable under the
	// umask, which Prepare refuses on Unix.
	dir := filepath.Join(t.TempDir(), "data")
	layout, err := datadir.Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	img := dockertest.ImageID(1)
	port := 7401
	sv, err := st.CreateServer(context.Background(), store.Server{Name: "arena", Image: img, Port: &port, DesiredState: "running"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := 7402
	if _, err := st.CreateServer(context.Background(), store.Server{Name: "idle", Image: img, Port: &stopped}, nil); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	fake := dockertest.New()
	fake.AddImage(dockertest.Image{ID: img, Labels: dockertest.ManagedLabels()})
	orphan := fake.AddContainer(dockertest.Container{Name: "golem-deploy-gone", Image: img,
		Labels: dockertest.ManagedLabels(docker.LabelServerID, "999", docker.LabelServer, "gone")})

	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir: dir, Listen: "127.0.0.1:0", PortRange: "7400-7410",
			Logger:     slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)),
			Ready:      func(a net.Addr) { ready <- a },
			bcryptCost: bcrypt.MinCost,
			dockerCLI:  fake,
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if c, ok := fake.Container("golem-deploy-arena"); ok && c.State == "running" && c.Labels[docker.LabelServerID] == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server %d not started by reconciliation; calls %v", sv.ID, fake.Calls())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Container("golem-deploy-idle"); ok {
		t.Fatal("stopped server started")
	}
	if c, ok := fake.Container(orphan); !ok || c.State != "running" {
		t.Fatal("orphaned managed container touched")
	}
	if !strings.Contains(logs.String(), "managed container has no server definition") {
		t.Fatalf("orphan not logged:\n%s", logs.String())
	}
}
