package docker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

const secretMarker = "SECRET-VALUE-7f3a"

func newClient(t *testing.T) (*docker.Client, *dockertest.Fake, *bytes.Buffer) {
	t.Helper()
	f := dockertest.New()
	var logs bytes.Buffer
	env := map[string]string{"PATH": "/usr/bin", "HOME": "/home/gd", "DOCKER_HOST": "unix:///run/docker.sock", "GOLEM_DEPLOY_SECRET": "daemon-secret"}
	c := docker.New(docker.Options{
		Runner:    f,
		Logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	return c, f, &logs
}

func managedImage(f *dockertest.Fake, n int, tags ...string) string {
	id := dockertest.ImageID(n)
	f.AddImage(dockertest.Image{ID: id, Tags: tags, Labels: dockertest.ManagedLabels(), Logs: []string{"hello"}})
	return id
}

func spec(image string) docker.RunSpec {
	pid := int64(4)
	return docker.RunSpec{
		ServerID: 12, ServerName: "arena", ProjectID: &pid, Image: image, HostPort: 7005,
		Env: map[string]string{"MODE": secretMarker, "A_FIRST": "x"}, ConfigHash: "abc",
	}
}

func TestRunSpecArgvExact(t *testing.T) {
	img := dockertest.ImageID(1)
	args, env, err := spec(img).Args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run", "--detach", "--name", "golem-deploy-arena",
		"--label", "dev.golemengine.deploy.managed=true",
		"--label", "dev.golemengine.deploy.server-id=12",
		"--label", "dev.golemengine.deploy.server=arena",
		"--label", "dev.golemengine.deploy.project-id=4",
		"--label", "dev.golemengine.deploy.config-hash=abc",
		"--restart", "on-failure:5", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--pids-limit", "512",
		"--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=3",
		"--stop-timeout", "30", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--user", "65532:65532", "--network", "golem-deploy",
		"--publish", "7005:7777/udp", "--publish", "7005:7777/tcp",
		"--env", "A_FIRST", "--env", "MODE",
		"--", img,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("argv\n got %q\nwant %q", args, want)
	}
	if !slices.Equal(env, []string{"A_FIRST=x", "MODE=" + secretMarker}) {
		t.Fatalf("secret env %q", env)
	}
	if strings.Contains(strings.Join(args, " "), secretMarker) {
		t.Fatal("env value in argv")
	}
}

func TestRunSpecOptionsAndExtensionPoints(t *testing.T) {
	s := spec(dockertest.ImageID(1))
	s.BindAddress = "::1"
	s.MemoryMB, s.CPUs, s.PidsLimit, s.GID = 512, 1.5, 64, 1000
	s.ReadOnlyBinds = []docker.BindMount{{Source: "/srv/certs", Target: "/certs"}}
	s.DataVolume = &docker.VolumeMount{Name: "golem-deploy-arena-data", Target: "/data"}
	s.AdminPort, s.AdminHostPort = 9000, 19000
	s.DaemonEnv = map[string]string{"mode": "daemon"} // overrides MODE ignoring case
	args, env, err := s.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--memory 512m", "--cpus 1.5", "--pids-limit 64", "--user 65532:1000",
		"--publish [::1]:7005:7777/udp", "--publish [::1]:7005:7777/tcp",
		"--publish 127.0.0.1:19000:9000/tcp",
		"--mount type=bind,source=/srv/certs,target=/certs,readonly",
		"--mount type=volume,source=golem-deploy-arena-data,target=/data",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if !slices.Equal(env, []string{"A_FIRST=x", "mode=daemon"}) {
		t.Fatalf("env %q", env)
	}

	bad := []func(*docker.RunSpec){
		func(s *docker.RunSpec) { s.Image = "game:latest" },
		func(s *docker.RunSpec) { s.ServerName = "../x" },
		func(s *docker.RunSpec) { s.HostPort = 0 },
		func(s *docker.RunSpec) { s.BindAddress = "example.com" },
		func(s *docker.RunSpec) { s.Env = map[string]string{"DOCKER_HOST": "tcp://evil"} },
		func(s *docker.RunSpec) { s.Env = map[string]string{"A": "x\x00"} },
		func(s *docker.RunSpec) { s.MemoryMB = 2 },
		func(s *docker.RunSpec) { s.ReadOnlyBinds = []docker.BindMount{{Source: "/a,bind-propagation=shared", Target: "/b"}} },
		func(s *docker.RunSpec) { s.ReadOnlyBinds = []docker.BindMount{{Source: "relative", Target: "/b"}} },
		func(s *docker.RunSpec) { s.DataVolume = &docker.VolumeMount{Name: "-x", Target: "/d"} },
		func(s *docker.RunSpec) { s.ServerID = 0 },
	}
	for i, mut := range bad {
		s := spec(dockertest.ImageID(1))
		mut(&s)
		if _, _, err := s.Args(); err == nil {
			t.Errorf("bad spec %d accepted", i)
		}
	}
}

func TestRunServerKeepsEnvValuesOutOfArgvAndLogs(t *testing.T) {
	c, f, logs := newClient(t)
	img := managedImage(f, 1)
	id, err := c.RunServer(context.Background(), spec(img))
	if err != nil || id == "" {
		t.Fatalf("run: %q %v", id, err)
	}
	calls := f.CallsTo("run")
	if len(calls) != 1 {
		t.Fatalf("calls %v", f.Calls())
	}
	if strings.Contains(strings.Join(calls[0].Args, "\x00"), secretMarker) {
		t.Fatal("secret in argv")
	}
	if !slices.Contains(calls[0].Env, "MODE="+secretMarker) {
		t.Fatalf("secret not in child env: %q", calls[0].Env)
	}
	// The child gets only the allowlisted daemon environment.
	for _, kv := range calls[0].Env {
		if strings.HasPrefix(kv, "GOLEM_DEPLOY_SECRET=") {
			t.Fatal("daemon environment leaked into the docker child")
		}
	}
	if !slices.Contains(calls[0].Env, "DOCKER_HOST=unix:///run/docker.sock") {
		t.Fatalf("DOCKER_HOST not passed: %q", calls[0].Env)
	}
	if strings.Contains(logs.String(), secretMarker) {
		t.Fatalf("secret in logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "env_keys=A_FIRST,MODE") {
		t.Fatalf("keys not logged: %s", logs.String())
	}
	ct, _ := f.Container(id)
	if ct.Env["MODE"] != secretMarker {
		t.Fatalf("fake container env %v", ct.Env)
	}
}

// An unmanaged container that impersonates a server (same name, even the
// server-id label) is invisible and never receives a destructive command.
func TestUnmanagedContainersAreUntouchable(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		c, f, _ := newClient(t)
		f.SetIgnoreFilters(ignore) // true: Docker's filter "fails"; the client must re-check labels
		foreign := f.AddContainer(dockertest.Container{
			Name:   "golem-deploy-arena",
			Image:  dockertest.ImageID(9),
			Labels: map[string]string{docker.LabelServerID: "12", docker.LabelServer: "arena"},
		})
		ctx := context.Background()
		if list, err := c.ManagedContainers(ctx); err != nil || len(list) != 0 {
			t.Fatalf("ignore=%v: managed list %v %v", ignore, list, err)
		}
		if _, err := c.ServerContainer(ctx, 12); !errors.Is(err, docker.ErrNotFound) {
			t.Fatalf("ignore=%v: lookup %v", ignore, err)
		}
		if n, err := c.StopServer(ctx, 12, time.Second); err != nil || n != 0 {
			t.Fatalf("stop %d %v", n, err)
		}
		if n, err := c.RemoveServerContainers(ctx, 12, time.Second); err != nil || n != 0 {
			t.Fatalf("rm %d %v", n, err)
		}
		if err := c.ServerLogs(ctx, 12, docker.LogOptions{}, &bytes.Buffer{}); !errors.Is(err, docker.ErrNotFound) {
			t.Fatalf("logs %v", err)
		}
		if _, err := c.ServerStats(ctx, 12); !errors.Is(err, docker.ErrNotFound) {
			t.Fatalf("stats %v", err)
		}
		// Running a server whose name collides fails; the foreign container survives.
		img := managedImage(f, 1)
		if _, err := c.RunServer(ctx, spec(img)); !errors.Is(err, docker.ErrConflict) {
			t.Fatalf("run over foreign name: %v", err)
		}
		for _, call := range f.Calls() {
			if slices.Contains(call.Args, foreign) {
				switch call.Args[0] {
				case "stop", "rm", "logs", "stats", "start", "kill":
					t.Fatalf("destructive call on unmanaged container: %q", call.Args)
				}
			}
		}
		if ct, ok := f.Container(foreign); !ok || ct.State != "running" {
			t.Fatal("foreign container was changed")
		}
	}
}

func TestServerLookupIsScopedToServerID(t *testing.T) {
	c, f, _ := newClient(t)
	img := managedImage(f, 1)
	ctx := context.Background()
	s := spec(img)
	if _, err := c.RunServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	other := spec(img)
	other.ServerID, other.ServerName, other.HostPort = 13, "other", 7006
	if _, err := c.RunServer(ctx, other); err != nil {
		t.Fatal(err)
	}
	ct, err := c.ServerContainer(ctx, 12)
	if err != nil || ct.ServerID != 12 || ct.Name != "golem-deploy-arena" {
		t.Fatalf("%+v %v", ct, err)
	}
	if n, err := c.RemoveServerContainers(ctx, 12, time.Second); err != nil || n != 1 {
		t.Fatalf("rm %d %v", n, err)
	}
	if _, ok := f.Container("golem-deploy-other"); !ok {
		t.Fatal("other server's container removed")
	}
	// Stop gracefully before rm, with "--" before the id.
	stops := f.CallsTo("stop")
	if len(stops) != 1 || !slices.Equal(stops[0].Args[:4], []string{"stop", "--time", "1", "--"}) {
		t.Fatalf("stop calls %v", stops)
	}
	if rms := f.CallsTo("rm"); len(rms) != 1 || rms[0].Args[2] != "--" {
		t.Fatalf("rm calls %v", rms)
	}
}

func TestImagesAndVolumesManagedOnly(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	managed := managedImage(f, 1, "game:v1")
	unmanaged := dockertest.ImageID(2)
	f.AddImage(dockertest.Image{ID: unmanaged, Tags: []string{"postgres:16"}})

	imgs, err := c.ListImages(ctx)
	if err != nil || len(imgs) != 1 || imgs[0].ID != managed || imgs[0].Tags[0] != "game:v1" {
		t.Fatalf("images %+v %v", imgs, err)
	}
	if _, err := c.ManagedImage(ctx, "postgres:16"); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged resolved: %v", err)
	}
	if _, err := c.ManagedImage(ctx, "nope:1"); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if img, err := c.ManagedImage(ctx, "game:v1"); err != nil || img.ID != managed {
		t.Fatalf("resolve tag: %+v %v", img, err)
	}
	f.SetIgnoreFilters(true)
	if imgs, _ := c.ListImages(ctx); len(imgs) != 1 {
		t.Fatalf("unmanaged image listed when filter bypassed: %+v", imgs)
	}
	f.SetIgnoreFilters(false)
	if err := c.RemoveImage(ctx, unmanaged); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged image removal: %v", err)
	}
	if len(f.CallsTo("image", "rm")) != 0 || !f.HasImage(unmanaged) {
		t.Fatal("rm issued for unmanaged image")
	}
	// In use by any container (even an unmanaged one).
	f.AddContainer(dockertest.Container{Name: "x", Image: managed, State: "exited"})
	if err := c.RemoveImage(ctx, managed); !errors.Is(err, docker.ErrInUse) {
		t.Fatalf("in-use removal: %v", err)
	}
	if len(f.CallsTo("image", "rm")) != 0 {
		t.Fatal("rm issued for in-use image")
	}

	f.AddVolume(dockertest.Volume{Name: "golem-deploy-arena-data", Labels: dockertest.ManagedLabels(docker.LabelServerID, "12", docker.LabelServer, "arena")})
	f.AddVolume(dockertest.Volume{Name: "pixel_pgdata"})
	vols, err := c.ListVolumes(ctx)
	if err != nil || len(vols) != 1 || vols[0].ServerID != 12 || vols[0].InUse {
		t.Fatalf("volumes %+v %v", vols, err)
	}
	if err := c.RemoveVolume(ctx, "pixel_pgdata"); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged volume removal: %v", err)
	}
	if err := c.RemoveVolume(ctx, "--all"); err == nil {
		t.Fatal("flag-like volume name accepted")
	}
	f.AddContainer(dockertest.Container{Name: "y", Mounts: []string{"golem-deploy-arena-data"}})
	if err := c.RemoveVolume(ctx, "golem-deploy-arena-data"); !errors.Is(err, docker.ErrInUse) {
		t.Fatalf("attached volume removal: %v", err)
	}
	if len(f.CallsTo("volume", "rm")) != 0 || !f.HasVolume("pixel_pgdata") {
		t.Fatal("volume rm issued")
	}
}

func TestRemoveManagedImageWithTags(t *testing.T) {
	c, f, _ := newClient(t)
	id := managedImage(f, 1, "game:v1", "game:latest")
	if err := c.RemoveImage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if f.HasImage(id) {
		t.Fatal("image not removed")
	}
}

func TestEnsureNetwork(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	if err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsTo("network", "create")); n != 1 {
		t.Fatalf("created %d times", n)
	}
	c2, f2, _ := newClient(t)
	f2.AddNetwork(docker.NetworkName, nil)
	if err := c2.EnsureNetwork(ctx); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("foreign network accepted: %v", err)
	}
}

func TestUnavailableAndProbe(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	if !c.Reachable(ctx) {
		t.Fatal("not reachable")
	}
	f.SetDown(true)
	_, err := c.ManagedContainers(ctx)
	if !errors.Is(err, docker.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if c.Reachable(ctx) {
		t.Fatal("still reachable after a failed call")
	}
	missing := docker.New(docker.Options{Binary: "golem-deploy-no-such-docker-binary"})
	if err := missing.Ping(ctx); !errors.Is(err, docker.ErrUnavailable) {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestLogsAndStats(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	img := managedImage(f, 1)
	id, err := c.RunServer(ctx, spec(img))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := c.ServerLogs(ctx, 12, docker.LogOptions{Tail: 5}, &buf); err != nil || !strings.Contains(buf.String(), "hello") {
		t.Fatalf("logs %q %v", buf.String(), err)
	}
	if call := f.CallsTo("logs")[0]; !slices.Equal(call.Args, []string{"logs", "--timestamps", "--tail", "5", "--", id}) {
		t.Fatalf("logs argv %q", call.Args)
	}
	for _, bad := range []int{-1, docker.MaxLogTail + 1} {
		if err := c.ServerLogs(ctx, 12, docker.LogOptions{Tail: bad}, &buf); err == nil {
			t.Fatalf("tail %d accepted", bad)
		}
	}
	st, err := c.ServerStats(ctx, 12)
	if err != nil || st.PIDs != 3 || st.MemoryBytes != 12<<20 || st.NetworkTxBytes != 2_000_000 {
		t.Fatalf("stats %+v %v", st, err)
	}

	// Follow ends when the container stops.
	done := make(chan error, 1)
	go func() { done <- c.ServerLogs(ctx, 12, docker.LogOptions{Follow: true}, &bytes.Buffer{}) }()
	time.Sleep(20 * time.Millisecond)
	f.SetContainerState(id, "exited", 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end")
	}
	if _, err := c.ServerStats(ctx, 12); !errors.Is(err, docker.ErrNotRunning) {
		t.Fatalf("stats on stopped: %v", err)
	}
	// Follow ends when ctx is cancelled.
	f.SetContainerState(id, "running", 0)
	cctx, cancel := context.WithCancel(ctx)
	go func() { done <- c.ServerLogs(cctx, 12, docker.LogOptions{Follow: true}, &bytes.Buffer{}) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end on cancel")
	}
}
