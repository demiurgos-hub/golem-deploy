package docker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

const (
	secretMarker = "SECRET-VALUE-7f3a"
	instance     = dockertest.InstanceID
)

// daemonEnv is the daemon's own (fake) environment.
var daemonEnv = map[string]string{"PATH": "/usr/bin", "HOME": "/home/gd", "DOCKER_HOST": "unix:///run/docker.sock", "GOLEM_DEPLOY_SECRET": "daemon-secret"}

func newClient(t *testing.T) (*docker.Client, *dockertest.Fake, *bytes.Buffer) {
	t.Helper()
	f := dockertest.New()
	var logs bytes.Buffer
	c := docker.New(docker.Options{
		Runner:     f,
		Logger:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		LookupEnv:  func(k string) (string, bool) { v, ok := daemonEnv[k]; return v, ok },
		InstanceID: instance,
		TempDir:    t.TempDir(),
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

func runtimeGOOS() string { return runtime.GOOS }

// run starts spec and returns the ref the service would record.
func run(t *testing.T, c *docker.Client, s docker.RunSpec) docker.ServerRef {
	t.Helper()
	if _, err := c.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, err := c.RunServer(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return docker.ServerRef{ServerID: s.ServerID, ContainerID: id}
}

func TestRunSpecArgvExact(t *testing.T) {
	img := dockertest.ImageID(1)
	args, err := spec(img).Args(instance, "/dev/fd/3")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run", "--detach", "--pull", "never", "--name", "golem-deploy-arena",
		"--label", "dev.golemengine.deploy.managed=true",
		"--label", "dev.golemengine.deploy.instance=" + instance,
		"--label", "dev.golemengine.deploy.server-id=12",
		"--label", "dev.golemengine.deploy.server=arena",
		"--label", "dev.golemengine.deploy.project-id=4",
		"--label", "dev.golemengine.deploy.commit=",
		"--label", "dev.golemengine.deploy.config-hash=abc",
		"--restart", "no", "--init", "--no-healthcheck", "--stop-signal", "SIGTERM", "--stop-timeout", "30",
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512",
		"--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=3",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--user", "65532:65532", "--network", "golem-deploy",
		"--publish", "7005:7777/udp", "--publish", "7005:7777/tcp",
		"--env-file", "/dev/fd/3",
		"--", img,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("argv\n got %q\nwant %q", args, want)
	}
	envFile, err := spec(img).EnvFile()
	if err != nil || string(envFile) != "A_FIRST=x\nMODE="+secretMarker+"\n" {
		t.Fatalf("env file %q %v", envFile, err)
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
	s.DaemonEnv = map[string]string{"mode": "daemon", "GOLEM_ENGINE_PORT": "7777"} // overrides MODE ignoring case; may use GOLEM_
	args, err := s.Args(instance, "/tmp/env")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--memory 512m --memory-swap 512m", "--cpus 1.5", "--pids-limit 64", "--user 65532:1000",
		"--publish [::1]:7005:7777/udp", "--publish [::1]:7005:7777/tcp",
		"--publish 127.0.0.1:19000:9000/tcp",
		"--mount type=bind,source=/srv/certs,target=/certs,readonly",
		"--mount type=volume,source=golem-deploy-arena-data,target=/data",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	envFile, _ := s.EnvFile()
	if string(envFile) != "A_FIRST=x\nGOLEM_ENGINE_PORT=7777\nmode=daemon\n" {
		t.Fatalf("env %q", envFile)
	}

	bad := []func(*docker.RunSpec){
		func(s *docker.RunSpec) { s.Image = "game:latest" },
		func(s *docker.RunSpec) { s.ServerName = "../x" },
		func(s *docker.RunSpec) { s.HostPort = 0 },
		func(s *docker.RunSpec) { s.BindAddress = "example.com" },
		func(s *docker.RunSpec) { s.Env = map[string]string{"GOLEM_X": "1"} },
		func(s *docker.RunSpec) { s.Env = map[string]string{"A": "x\x00"} },
		func(s *docker.RunSpec) { s.Env = map[string]string{"A": "x\ny"} },
		func(s *docker.RunSpec) { s.MemoryMB = 2 },
		func(s *docker.RunSpec) {
			s.ReadOnlyBinds = []docker.BindMount{{Source: "/a,bind-propagation=shared", Target: "/b"}}
		},
		func(s *docker.RunSpec) { s.ReadOnlyBinds = []docker.BindMount{{Source: "relative", Target: "/b"}} },
		func(s *docker.RunSpec) { s.DataVolume = &docker.VolumeMount{Name: "-x", Target: "/d"} },
		func(s *docker.RunSpec) { s.ServerID = 0 },
		func(s *docker.RunSpec) { s.Commit = "a\nb" },
	}
	for i, mut := range bad {
		s := spec(dockertest.ImageID(1))
		mut(&s)
		if _, err := s.Args(instance, "/tmp/env"); err == nil {
			t.Errorf("bad spec %d accepted", i)
		}
	}
	if _, err := spec(dockertest.ImageID(1)).Args("not-an-instance", "/tmp/env"); err == nil {
		t.Error("bad instance id accepted")
	}
	if _, err := spec(dockertest.ImageID(1)).Args(instance, ""); err == nil {
		t.Error("env without an env file accepted")
	}
}

// D1: container env values never reach the docker CLI's own process
// environment or its argv, whatever their names.
func TestD1_EnvValuesNeverReachCLIEnvOrArgv(t *testing.T) {
	c, f, logs := newClient(t)
	img := managedImage(f, 1)
	s := spec(img)
	s.Env = map[string]string{
		"LD_PRELOAD":  "/srv/static/" + secretMarker + ".so",
		"GODEBUG":     "x509ignoreCN=0," + secretMarker,
		"DOCKER_HOST": "tcp://evil.example:2375/" + secretMarker,
		"PATH":        "/evil/" + secretMarker,
		"MODE":        secretMarker,
	}
	r := run(t, c, s)
	calls := f.CallsTo("run")
	if len(calls) != 1 {
		t.Fatalf("calls %v", f.Calls())
	}
	call := calls[0]
	if strings.Contains(strings.Join(call.Args, "\x00"), secretMarker) {
		t.Fatalf("value in argv: %q", call.Args)
	}
	// The child environment is exactly the allowlist from the daemon's own
	// environment.
	want := docker.BaseEnv(func(k string) (string, bool) { v, ok := daemonEnv[k]; return v, ok }, runtimeGOOS())
	if !slices.Equal(call.Env, want) {
		t.Fatalf("child env %q, want %q", call.Env, want)
	}
	for _, kv := range call.Env {
		if strings.Contains(kv, secretMarker) || strings.HasPrefix(kv, "LD_PRELOAD=") || strings.HasPrefix(kv, "GODEBUG=") {
			t.Fatalf("container variable in the CLI environment: %q", kv)
		}
	}
	if !slices.Contains(call.Env, "DOCKER_HOST=unix:///run/docker.sock") || !slices.Contains(call.Env, "PATH=/usr/bin") {
		t.Fatalf("daemon's DOCKER_HOST/PATH not kept: %q", call.Env)
	}
	if !strings.Contains(call.EnvFile, "LD_PRELOAD=/srv/static/"+secretMarker+".so\n") {
		t.Fatalf("env file %q", call.EnvFile)
	}
	ct, _ := f.Container(r.ContainerID)
	for k, v := range s.Env {
		if ct.Env[k] != v {
			t.Fatalf("container env %s=%q, want %q", k, ct.Env[k], v)
		}
	}
	if strings.Contains(logs.String(), secretMarker) {
		t.Fatalf("value in logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "env_keys=DOCKER_HOST,GODEBUG,LD_PRELOAD,MODE,PATH") {
		t.Fatalf("keys not logged: %s", logs.String())
	}
	// No call ever carries --env.
	for _, call := range f.Calls() {
		if slices.Contains(call.Args, "--env") || slices.Contains(call.Args, "-e") {
			t.Fatalf("--env used: %q", call.Args)
		}
	}
}

// D1: the generated env file means exactly the map it was made from, as
// the docker CLI parses it.
func TestD1_EnvFileIsUnambiguous(t *testing.T) {
	values := map[string]string{
		"SPACES":    "  leading and trailing  ",
		"QUOTES":    `"double" 'single' ` + "`back`",
		"HASH":      "#not a comment",
		"EQUALS":    "a=b==c=",
		"DOLLAR":    "$HOME ${PATH} $(id)",
		"BACKSLASH": `C:\path\n\t\\`,
		"UNICODE":   "ünïcödé ✓ 日本",
		"BOM":       "\uFEFFstarts with a BOM",
		"EMPTY":     "",
		"TAB":       "\tx\t",
		"CTRL":      "esc\x1b[31m",
		"ONLY_WS":   "   ",
		"LONG":      strings.Repeat("y", docker.MaxEnvVarBytes-len("LONG")),
	}
	content, err := docker.EncodeEnvFile(values)
	if err != nil {
		t.Fatal(err)
	}
	// Every line is KEY=value: never a bare key (which would copy the
	// variable from the CLI's environment), a comment or a blank line.
	for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		k, _, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t#\uFEFF") {
			t.Fatalf("ambiguous line %q", line)
		}
	}
	cliEnv := map[string]string{"EMPTY": "from-cli", "SPACES": "from-cli"}
	lines, err := dockertest.ParseEnvFile(content, func(k string) (string, bool) { v, ok := cliEnv[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range lines {
		k, v, _ := strings.Cut(l, "=")
		got[k] = v
	}
	if len(got) != len(values) {
		t.Fatalf("parsed %d variables, want %d", len(got), len(values))
	}
	for k, v := range values {
		if got[k] != v {
			t.Errorf("%s: parsed %q, want %q", k, got[k], v)
		}
	}
	// Values that could not be represented are rejected before any file is
	// written, naming the key but never the value.
	for _, bad := range []string{"a\nINJECTED=1", "a\r", "a\x00b", "\xff\xfe"} {
		_, err := docker.EncodeEnvFile(map[string]string{"K": bad})
		var ve *docker.ValidationError
		if !errors.As(err, &ve) || ve.Key != "K" || strings.Contains(err.Error(), bad) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// A bare key line in a hand-written file would inherit from the CLI:
	// that is exactly what the encoder must never produce.
	inherited, _ := dockertest.ParseEnvFile([]byte("EMPTY\n"), func(k string) (string, bool) { v, ok := cliEnv[k]; return v, ok })
	if !slices.Equal(inherited, []string{"EMPTY=from-cli"}) {
		t.Fatalf("parser model: %q", inherited)
	}
}

// D1: GOLEM_* is reserved for the daemon on input.
func TestD1_ReservedPrefix(t *testing.T) {
	for _, k := range []string{"GOLEM_ENGINE", "golem_secret", "Golem_X"} {
		err := docker.ValidateEnvKey(k)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: %v", k, err)
		}
	}
}

// D2: every volume the image declares gets a size-limited noexec tmpfs, a
// daemon mount at the same path wins, and too many or odd paths are refused.
func TestD2_ImageVolumesGetTmpfs(t *testing.T) {
	s := spec(dockertest.ImageID(1))
	s.ImageVolumes = []string{"/data", "/var/lib/game/", "/tmp", "/certs", "/data"}
	s.DataVolume = &docker.VolumeMount{Name: "v1", Target: "/var/lib/game"}
	s.ReadOnlyBinds = []docker.BindMount{{Source: "/srv/certs", Target: "/certs"}}
	args, err := s.Args(instance, "/e")
	if err != nil {
		t.Fatal(err)
	}
	var tmpfs []string
	for i, a := range args {
		if a == "--tmpfs" {
			tmpfs = append(tmpfs, args[i+1])
		}
	}
	if !slices.Equal(tmpfs, []string{"/tmp:rw,noexec,nosuid,size=64m", "/data:rw,noexec,nosuid,size=64m"}) {
		t.Fatalf("tmpfs %q", tmpfs)
	}
	for _, vols := range [][]string{
		{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h", "/i"},
		{"/data:rw"}, {"/a,b"}, {"/"}, {"relative"},
	} {
		s := spec(dockertest.ImageID(1))
		s.ImageVolumes = vols
		if _, err := s.Args(instance, "/e"); err == nil {
			t.Errorf("image volumes %q accepted", vols)
		}
	}
}

// D3: a container that carries every one of our labels but is not the
// recorded container is never resolved, stopped, removed or read.
func TestD3_ImpostorIsIgnored(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	img := managedImage(f, 1)
	impostor := f.AddContainer(dockertest.Container{Name: "evil", Image: img,
		Labels: dockertest.OurLabels(docker.LabelServerID, "12", docker.LabelServer, "arena")})
	// No container recorded: nothing to act on.
	none := docker.ServerRef{ServerID: 12}
	if _, err := c.ServerContainer(ctx, none); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("lookup without record: %v", err)
	}
	if stopped, err := c.StopServer(ctx, none, time.Second); err != nil || stopped {
		t.Fatalf("stop %v %v", stopped, err)
	}
	if removed, err := c.RemoveServerContainer(ctx, none, time.Second); err != nil || removed {
		t.Fatalf("rm %v %v", removed, err)
	}
	if err := c.ServerLogs(ctx, none, docker.LogOptions{}, &bytes.Buffer{}); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("logs %v", err)
	}
	if _, err := c.ServerStats(ctx, none); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("stats %v", err)
	}
	// The real container is resolved by its recorded id only.
	r := run(t, c, spec(img))
	if ct, err := c.ServerContainer(ctx, r); err != nil || ct.ID != r.ContainerID {
		t.Fatalf("%+v %v", ct, err)
	}
	// Recording another server's (or a foreign) id is refused on inspect.
	if _, err := c.ServerContainer(ctx, docker.ServerRef{ServerID: 13, ContainerID: r.ContainerID}); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("wrong server id: %v", err)
	}
	if removed, err := c.RemoveServerContainer(ctx, r, time.Second); err != nil || !removed {
		t.Fatalf("rm %v %v", removed, err)
	}
	if ct, ok := f.Container(impostor); !ok || ct.State != "running" {
		t.Fatal("impostor touched")
	}
	for _, call := range f.Calls() {
		if slices.Contains(call.Args, impostor) && call.Args[0] != "container" {
			t.Fatalf("call on impostor: %q", call.Args)
		}
	}
	// Labels an image sets are overridden by "docker run".
	f.AddImage(dockertest.Image{ID: dockertest.ImageID(2), Labels: dockertest.ManagedLabels(
		docker.LabelInstance, "ffffffffffffffffffffffffffffffff", docker.LabelServerID, "99",
		docker.LabelProjectID, "77", docker.LabelCommit, "evil", docker.LabelConfigHash, "stale")})
	s := spec(dockertest.ImageID(2))
	s.ProjectID = nil
	r2 := run(t, c, s)
	ct, _ := f.Container(r2.ContainerID)
	for k, want := range map[string]string{docker.LabelInstance: instance, docker.LabelServerID: "12",
		docker.LabelProjectID: "", docker.LabelCommit: "", docker.LabelConfigHash: "abc"} {
		if ct.Labels[k] != want {
			t.Errorf("label %s=%q, want %q", k, ct.Labels[k], want)
		}
	}
}

// D3: objects of another installation (or none) are invisible and refused.
func TestD3_InstanceScoping(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	other := "ffffffffffffffffffffffffffffffff"
	f.AddContainer(dockertest.Container{Name: "other", Labels: dockertest.ManagedLabels(docker.LabelInstance, other, docker.LabelServerID, "12")})
	f.AddContainer(dockertest.Container{Name: "legacy", Labels: dockertest.ManagedLabels(docker.LabelServerID, "12")})
	ours := f.AddContainer(dockertest.Container{Name: "ours", Labels: dockertest.OurLabels(docker.LabelServerID, "12")})
	for _, ignore := range []bool{false, true} {
		f.SetIgnoreFilters(ignore)
		list, err := c.ManagedContainers(ctx)
		if err != nil || len(list) != 1 || list[0].ID != ours {
			t.Fatalf("ignore=%v: %+v %v", ignore, list, err)
		}
	}
	f.SetIgnoreFilters(false)
	if ps := f.CallsTo("ps"); !strings.Contains(strings.Join(ps[0].Args, " "), "--filter label="+docker.LabelInstance+"="+instance) {
		t.Fatalf("instance filter missing: %q", ps[0].Args)
	}
	f.AddVolume(dockertest.Volume{Name: "other-data", Labels: dockertest.ManagedLabels(docker.LabelInstance, other)})
	f.AddVolume(dockertest.Volume{Name: "ours-data", Labels: dockertest.OurLabels()})
	vols, err := c.ListVolumes(ctx)
	if err != nil || len(vols) != 1 || vols[0].Name != "ours-data" {
		t.Fatalf("volumes %+v %v", vols, err)
	}
	if err := c.RemoveVolume(ctx, "other-data"); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("other installation's volume: %v", err)
	}
	if len(f.CallsTo("volume", "rm")) != 0 {
		t.Fatal("volume rm issued")
	}
	// Without an instance id the client refuses to work.
	noInst := docker.New(docker.Options{Runner: f})
	if _, err := noInst.ManagedContainers(ctx); err == nil {
		t.Fatal("client without instance id listed containers")
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

	f.AddVolume(dockertest.Volume{Name: "golem-deploy-arena-data", Labels: dockertest.OurLabels(docker.LabelServerID, "12", docker.LabelServer, "arena")})
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

// D14: images are removed by their immutable id, never by a tag.
func TestD14_RemoveImageByID(t *testing.T) {
	c, f, _ := newClient(t)
	id := managedImage(f, 1, "game:v1", "game:latest")
	if err := c.RemoveImage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if f.HasImage(id) {
		t.Fatal("image not removed")
	}
	rms := f.CallsTo("image", "rm")
	if len(rms) != 1 || !slices.Equal(rms[0].Args, []string{"image", "rm", "--force", "--", id}) {
		t.Fatalf("rm calls %v", rms)
	}
	single := managedImage(f, 2, "solo:1")
	f.ResetCalls()
	if err := c.RemoveImage(context.Background(), single); err != nil {
		t.Fatal(err)
	}
	if rms := f.CallsTo("image", "rm"); len(rms) != 1 || !slices.Equal(rms[0].Args, []string{"image", "rm", "--", single}) {
		t.Fatalf("rm calls %v", rms)
	}
}

// D5: the network is created without inter-container traffic; an older
// network of ours is recreated when nothing is attached and kept (with a
// warning) otherwise; other installations' networks are never touched.
func TestD5_NetworkIsolation(t *testing.T) {
	ctx := context.Background()
	c, f, _ := newClient(t)
	if isolated, err := c.EnsureNetwork(ctx); err != nil || !isolated {
		t.Fatalf("%v %v", isolated, err)
	}
	if isolated, err := c.EnsureNetwork(ctx); err != nil || !isolated {
		t.Fatalf("%v %v", isolated, err)
	}
	creates := f.CallsTo("network", "create")
	if len(creates) != 1 || !strings.Contains(strings.Join(creates[0].Args, " "), "--opt com.docker.network.bridge.enable_icc=false") {
		t.Fatalf("creates %v", creates)
	}
	n, _ := f.Network(docker.NetworkName)
	if n.Labels[docker.LabelInstance] != instance || n.Options[docker.NetworkICCOption] != "false" {
		t.Fatalf("network %+v", n)
	}

	// Ours with ICC enabled, nothing attached: recreated.
	c2, f2, _ := newClient(t)
	f2.AddNetwork(docker.NetworkName, dockertest.OurLabels(), nil)
	if isolated, err := c2.EnsureNetwork(ctx); err != nil || !isolated {
		t.Fatalf("%v %v", isolated, err)
	}
	if len(f2.CallsTo("network", "rm")) != 1 {
		t.Fatal("not recreated")
	}
	// Ours with ICC enabled and a running container attached: kept.
	c3, f3, logs := newClient(t)
	f3.AddNetwork(docker.NetworkName, dockertest.OurLabels(), nil)
	f3.AddContainer(dockertest.Container{Name: "attached", Network: docker.NetworkName})
	if isolated, err := c3.EnsureNetwork(ctx); err != nil || isolated {
		t.Fatalf("%v %v", isolated, err)
	}
	if len(f3.CallsTo("network", "rm")) != 0 || !strings.Contains(logs.String(), "isolation applies") {
		t.Fatal("attached network removed or no warning")
	}
	// Legacy (managed, no instance): recreated when empty, refused when used.
	c4, f4, _ := newClient(t)
	f4.AddNetwork(docker.NetworkName, dockertest.ManagedLabels(), nil)
	if _, err := c4.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := f4.Network(docker.NetworkName); n.Labels[docker.LabelInstance] != instance {
		t.Fatal("legacy network not recreated")
	}
	c5, f5, _ := newClient(t)
	f5.AddNetwork(docker.NetworkName, dockertest.ManagedLabels(), nil)
	f5.AddContainer(dockertest.Container{Name: "old", Network: docker.NetworkName})
	if _, err := c5.EnsureNetwork(ctx); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("legacy attached: %v", err)
	}
	// Another installation's network, even empty, and an unmanaged one.
	c6, f6, _ := newClient(t)
	f6.AddNetwork(docker.NetworkName, dockertest.ManagedLabels(docker.LabelInstance, "ffffffffffffffffffffffffffffffff"), nil)
	if _, err := c6.EnsureNetwork(ctx); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("other installation: %v", err)
	}
	c7, f7, _ := newClient(t)
	f7.AddNetwork(docker.NetworkName, nil, nil)
	if _, err := c7.EnsureNetwork(ctx); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("foreign network accepted: %v", err)
	}
	for _, fk := range []*dockertest.Fake{f5, f6, f7} {
		if len(fk.CallsTo("network", "rm")) != 0 || len(fk.CallsTo("network", "create")) != 0 {
			t.Fatal("network of someone else changed")
		}
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
	missing := docker.New(docker.Options{Binary: "golem-deploy-no-such-docker-binary", InstanceID: instance})
	if err := missing.Ping(ctx); !errors.Is(err, docker.ErrUnavailable) {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestLogsStatsAndRestartPolicy(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	img := managedImage(f, 1)
	r := run(t, c, spec(img))
	if ct, _ := f.Container(r.ContainerID); ct.RestartPolicy != "no" {
		t.Fatalf("restart policy at run %q", ct.RestartPolicy)
	}
	if err := c.ApplyRestartPolicy(ctx, r); err != nil {
		t.Fatal(err)
	}
	if ct, _ := f.Container(r.ContainerID); ct.RestartPolicy != docker.RestartPolicy {
		t.Fatalf("restart policy %q", ct.RestartPolicy)
	}
	var buf bytes.Buffer
	if err := c.ServerLogs(ctx, r, docker.LogOptions{Tail: 5}, &buf); err != nil || !strings.Contains(buf.String(), "hello") {
		t.Fatalf("logs %q %v", buf.String(), err)
	}
	if call := f.CallsTo("logs")[0]; !slices.Equal(call.Args, []string{"logs", "--timestamps", "--tail", "5", "--", r.ContainerID}) {
		t.Fatalf("logs argv %q", call.Args)
	}
	for _, bad := range []int{-1, docker.MaxLogTail + 1} {
		if err := c.ServerLogs(ctx, r, docker.LogOptions{Tail: bad}, &buf); err == nil {
			t.Fatalf("tail %d accepted", bad)
		}
	}
	st, err := c.ServerStats(ctx, r)
	if err != nil || st.PIDs != 3 || st.MemoryBytes != 12<<20 || st.NetworkTxBytes != 2_000_000 {
		t.Fatalf("stats %+v %v", st, err)
	}

	// Follow ends when the container stops.
	done := make(chan error, 1)
	go func() { done <- c.ServerLogs(ctx, r, docker.LogOptions{Follow: true}, &bytes.Buffer{}) }()
	time.Sleep(20 * time.Millisecond)
	f.SetContainerState(r.ContainerID, "exited", 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end")
	}
	if _, err := c.ServerStats(ctx, r); !errors.Is(err, docker.ErrNotRunning) {
		t.Fatalf("stats on stopped: %v", err)
	}
	// Follow ends when ctx is cancelled.
	f.SetContainerState(r.ContainerID, "running", 0)
	cctx, cancel := context.WithCancel(ctx)
	go func() { done <- c.ServerLogs(cctx, r, docker.LogOptions{Follow: true}, &bytes.Buffer{}) }()
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
	// Stop gracefully, then remove with its anonymous volumes.
	if _, err := c.RemoveServerContainer(ctx, r, time.Second); err != nil {
		t.Fatal(err)
	}
	stops := f.CallsTo("stop")
	if len(stops) != 1 || !slices.Equal(stops[0].Args, []string{"stop", "--time", "1", "--", r.ContainerID}) {
		t.Fatalf("stop calls %v", stops)
	}
	if rms := f.CallsTo("rm"); len(rms) != 1 || !slices.Equal(rms[0].Args, []string{"rm", "--force", "--volumes", "--", r.ContainerID}) {
		t.Fatalf("rm calls %v", rms)
	}
}

// A failed run never puts Docker's stderr, with values, into the error the
// daemon logs, and a name taken by another container is ErrConflict.
func TestRunErrorsAreClassifiedAndRedacted(t *testing.T) {
	c, f, _ := newClient(t)
	img := managedImage(f, 1)
	f.AddContainer(dockertest.Container{Name: "golem-deploy-arena"})
	_, err := c.RunServer(context.Background(), spec(img))
	if !errors.Is(err, docker.ErrConflict) || strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("got %v", err)
	}
	s := spec(img)
	s.Image = dockertest.ImageID(77)
	s.ServerName = "other"
	if _, err := c.RunServer(context.Background(), s); !errors.Is(err, docker.ErrImageMissing) {
		t.Fatalf("missing image: %v", err)
	}
}
