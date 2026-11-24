package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
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
		{Listen: "127.0.0.1:8000", StartTimeout: 10 * time.Millisecond},
		{Listen: "127.0.0.1:8000", StartTimeout: time.Hour},
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
	ctx0 := context.Background()
	img := dockertest.ImageID(1)
	proj, err := st.CreateProject(ctx0, store.Project{Name: "game", RepoURL: "https://example.com/g.git"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.CreateBuild(ctx0, store.NewBuild{ProjectID: proj.ID})
	_ = st.MarkBuildRunning(ctx0, b.ID, "main", strings.Repeat("a", 40))
	if err := st.FinishBuild(ctx0, b.ID, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: img, EngineVersion: "v0.4.0-rc.1"}); err != nil {
		t.Fatal(err)
	}
	port := 7401
	sv, err := st.CreateServer(ctx0, store.Server{Name: "arena", BuildID: &b.ID, Port: &port, DesiredState: "running"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := 7402
	if _, err := st.CreateServer(ctx0, store.Server{Name: "idle", BuildID: &b.ID, Port: &stopped}, nil); err != nil {
		t.Fatal(err)
	}
	instance, err := st.InstanceID(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	fake := dockertest.New()
	admin := gameadmintest.New(t)
	admin.Set(func(s *gameadmintest.Server) {
		s.ExpectToken = func() string { return fake.LatestEnv("GOLEM_ADMIN_TOKEN") }
	})
	fake.AddImage(dockertest.Image{ID: img, AssignPort: admin.Port,
		Labels: docker.BuildRef{ImageID: img, BuildID: b.ID, ProjectID: proj.ID, Commit: strings.Repeat("a", 40), EngineVersion: "v0.4.0-rc.1"}.Labels(instance)})
	orphan := fake.AddContainer(dockertest.Container{Name: "golem-deploy-gone", Image: img,
		Labels: dockertest.ManagedLabels(docker.LabelInstance, instance, docker.LabelServerID, "999", docker.LabelServer, "gone")})
	// Claims to be server 1 with every label, but is not its recorded container.
	impostor := fake.AddContainer(dockertest.Container{Name: "impostor", Image: img,
		Labels: dockertest.ManagedLabels(docker.LabelInstance, instance, docker.LabelServerID, "1", docker.LabelServer, "arena")})

	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir: dir, Listen: "127.0.0.1:0", PortRange: "7400-7410", PublicHosts: []string{"play.example.com"},
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
		if c, ok := fake.Container("golem-deploy-arena"); ok && c.State == "running" && c.Labels[docker.LabelServerID] == "1" &&
			c.RestartPolicy == docker.RestartPolicy {
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
	for _, id := range []string{orphan, impostor} {
		if c, ok := fake.Container(id); !ok || c.State != "running" {
			t.Fatal("orphaned or impostor container touched")
		}
	}
	if hasWarning := strings.Contains(logs.String(), "Windows is a development platform only"); hasWarning != (runtime.GOOS == "windows") {
		t.Fatalf("unexpected Windows data directory warning: %t", hasWarning)
	}
	if strings.Count(logs.String(), "is not the container recorded for any server") != 2 {
		t.Fatalf("orphan and impostor not logged once each:\n%s", logs.String())
	}
	// The instance id is generated once and kept.
	st2, err := store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if again, _ := st2.InstanceID(context.Background()); again != instance {
		t.Fatalf("instance id changed: %q -> %q", instance, again)
	}
	if got, _ := st2.GetServer(context.Background(), sv.ID); got.ContainerID == "" || got.ContainerID == impostor {
		t.Fatalf("recorded container %q", got.ContainerID)
	}
	// The game certificate was generated for the --public-host fallback and
	// mounted into the container.
	certDir := filepath.Join(layout.Root, "certs", "servers", fmt.Sprint(sv.ID))
	if _, err := os.Stat(filepath.Join(certDir, "tls.key")); err != nil {
		t.Fatal(err)
	}
	if c, _ := fake.Container("golem-deploy-arena"); len(c.Binds) != 1 || !strings.Contains(c.Binds[0], certDir) ||
		c.Env["GOLEM_PUBLIC_URL"] != "https://play.example.com:7401/wt" {
		t.Fatalf("%+v", c)
	}
}
