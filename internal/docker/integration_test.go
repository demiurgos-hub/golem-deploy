package docker_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
)

// Integration tests run against a real Docker daemon with
// GOLEM_DEPLOY_DOCKER_TESTS=1. Everything they create is named gdm2h-* (the
// server containers golem-deploy-gdm2h-*) and removed at cleanup; the shared
// "golem-deploy" network is removed too when it did not exist before. Run
// packages one at a time (go test -p 1) because they share that network.

// BusyboxImage is the pinned base of the integration test images.
const BusyboxImage = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
}

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dockerCLI(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// dockerTry runs docker and returns its combined output and error.
func dockerTry(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func dockerQuiet(args ...string) { _, _ = dockerTry(args...) }

func freePort(t *testing.T) int {
	t.Helper()
	// Docker publishes the same number on both transports. Start with the
	// UDP allocator so Windows UDP exclusions never become test candidates.
	const loopback = "127.0.0.1"
	for range 32 {
		conn, err := net.ListenPacket("udp4", loopback+":0")
		if err != nil {
			t.Fatal(err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		_ = conn.Close()
		if ports.ProbeFree(loopback, port) == nil {
			return port
		}
	}
	t.Fatal("could not find a host port available on both TCP and UDP")
	return 0
}

// buildImage builds an image from dockerfile with extra build args and
// removes it at cleanup.
func buildImage(t *testing.T, tag, dockerfile string, managed bool) string {
	t.Helper()
	args := []string{"build", "--quiet", "--tag", tag}
	if managed {
		args = append(args, "--label", docker.LabelManaged+"=true", "--label", docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
			"--label", docker.LabelCommit+"=itest")
	}
	args = append(args, "-")
	dockerCLI(t, dockerfile, args...)
	t.Cleanup(func() { dockerQuiet("image", "rm", "--force", tag) })
	return tag
}

// buildTestImage builds a tiny image that prints a line and serves TCP on
// the game port.
func buildTestImage(t *testing.T, tag string, managed bool) string {
	return buildImage(t, tag, "FROM "+BusyboxImage+"\n"+
		`CMD ["sh", "-c", "echo started; echo to-stderr >&2; exec nc -lk -p 7777 -e echo pong"]`+"\n", managed)
}

// recordingRunner wraps ExecRunner and records the environment of every
// docker CLI child it starts (the runner hook).
type recordingRunner struct {
	mu   sync.Mutex
	envs [][]string
	args [][]string
}

func (r *recordingRunner) Run(ctx context.Context, path string, c docker.Cmd) error {
	r.mu.Lock()
	r.envs = append(r.envs, slices.Clone(c.Env))
	r.args = append(r.args, slices.Clone(c.Args))
	r.mu.Unlock()
	return docker.ExecRunner{}.Run(ctx, path, c)
}

// itClient returns a client on the real docker CLI with the integration
// instance id, and removes the golem-deploy network at cleanup when the
// test created it.
func itClient(t *testing.T) (*docker.Client, *recordingRunner) {
	t.Helper()
	requireDocker(t)
	rr := &recordingRunner{}
	c := docker.New(docker.Options{Runner: rr, InstanceID: dockertest.IntegrationInstanceID, TempDir: t.TempDir()})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("docker unreachable: %v", err)
	}
	if _, err := dockerTry("network", "inspect", docker.NetworkName); err != nil {
		t.Cleanup(func() { dockerQuiet("network", "rm", docker.NetworkName) })
	}
	return c, rr
}

func serverID() int64 { return 900000000 + time.Now().UnixNano()%1000000 }

type inspected struct {
	ID    string `json:"Id"`
	State struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Config       struct {
		User        string            `json:"User"`
		Env         []string          `json:"Env"`
		Labels      map[string]string `json:"Labels"`
		StopSignal  string            `json:"StopSignal"`
		Healthcheck *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	HostConfig struct {
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		CapDrop        []string          `json:"CapDrop"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		PidsLimit      int64             `json:"PidsLimit"`
		Memory         int64             `json:"Memory"`
		MemorySwap     int64             `json:"MemorySwap"`
		NanoCPUs       int64             `json:"NanoCpus"`
		Init           *bool             `json:"Init"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		RestartPolicy  struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		LogConfig struct {
			Type   string            `json:"Type"`
			Config map[string]string `json:"Config"`
		} `json:"LogConfig"`
		NetworkMode  string `json:"NetworkMode"`
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type string `json:"Type"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func inspect(t *testing.T, id string) inspected {
	t.Helper()
	var insp []inspected
	if err := json.Unmarshal([]byte(dockerCLI(t, "", "container", "inspect", id)), &insp); err != nil || len(insp) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	return insp[0]
}

func TestIntegrationDocker(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	tag := buildTestImage(t, "gdm2h-itest:"+sfx, true)
	plainTag := buildTestImage(t, "gdm2h-itest-plain:"+sfx, false)
	name := "gdm2h-it-" + sfx
	sid := serverID()
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", docker.ContainerName(name))
		dockerQuiet("rm", "--force", "gdm2h-foreign-"+sfx)
		dockerQuiet("volume", "rm", "--force", "gdm2h-managed-"+sfx, "gdm2h-foreign-"+sfx)
	})

	if _, err := c.ManagedImage(ctx, plainTag); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unlabelled image accepted: %v", err)
	}
	img, err := c.ManagedImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if img.Commit != "itest" {
		t.Fatalf("labels %v", img.Labels)
	}
	if list, err := c.ListImages(ctx); err != nil || !containsImage(list, img.ID) || containsTag(list, plainTag) {
		t.Fatalf("list images: %v", err)
	}
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	s := docker.RunSpec{
		ServerID: sid, ServerName: name, Image: img.ID, HostPort: port, BindAddress: "127.0.0.1",
		Env: map[string]string{"GAME_SECRET": secretMarker}, ConfigHash: "itest", MemoryMB: 256, CPUs: 1,
	}
	id, err := createAndStart(ctx, c, s)
	if err != nil {
		t.Fatal(err)
	}
	ref := docker.ServerRef{ServerID: sid, ContainerID: id}
	waitRunning(t, c, ref)
	if err := c.ApplyRestartPolicy(ctx, ref); err != nil {
		t.Fatal(err)
	}

	// Hardening applied.
	insp := inspect(t, id)
	hc := insp.HostConfig
	if !hc.ReadonlyRootfs || len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || hc.PidsLimit != 512 ||
		!strings.Contains(strings.Join(hc.SecurityOpt, ","), "no-new-privileges") || insp.Config.User != "65532:65532" ||
		hc.RestartPolicy.Name != "on-failure" || hc.RestartPolicy.MaximumRetryCount != 5 || hc.LogConfig.Config["max-size"] != "10m" ||
		hc.NetworkMode != docker.NetworkName || !strings.Contains(hc.Tmpfs["/tmp"], "noexec") ||
		hc.Memory != 256<<20 || hc.MemorySwap != 256<<20 || hc.NanoCPUs != 1e9 || hc.Init == nil || !*hc.Init ||
		insp.Config.StopSignal != "SIGTERM" || insp.Config.Healthcheck == nil || !slices.Equal(insp.Config.Healthcheck.Test, []string{"NONE"}) {
		t.Fatalf("hardening missing: %+v config=%+v", hc, insp.Config)
	}
	for _, proto := range []string{"7777/udp", "7777/tcp"} {
		b := hc.PortBindings[proto]
		if len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != itoa(port) {
			t.Fatalf("port binding %s: %+v", proto, b)
		}
	}
	if !slices.Contains(insp.Config.Env, "GAME_SECRET="+secretMarker) {
		t.Fatal("env value did not reach the container")
	}
	if insp.Config.Labels[docker.LabelInstance] != dockertest.IntegrationInstanceID {
		t.Fatalf("labels %v", insp.Config.Labels)
	}

	// Logs (stdout + stderr) and stats.
	var logs bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		logs.Reset()
		if err := c.ServerLogs(ctx, ref, docker.LogOptions{Tail: 10}, &logs); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "started") && strings.Contains(logs.String(), "to-stderr") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "started") || !strings.Contains(logs.String(), "to-stderr") {
		t.Fatalf("logs %q", logs.String())
	}
	if st, err := c.ServerStats(ctx, ref); err != nil || st.PIDs < 1 || st.MemoryLimitBytes != 256<<20 {
		t.Fatalf("stats %+v %v", st, err)
	}

	// A container started with plain "docker run" that impersonates the
	// server without the managed label is invisible.
	foreign := "gdm2h-foreign-" + sfx
	dockerCLI(t, "", "run", "--detach", "--name", foreign, "--label", docker.LabelServerID+"="+itoa64(sid),
		"--label", docker.LabelServer+"="+name, BusyboxImage, "sleep", "300")
	mc, err := c.ManagedContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range mc {
		if ct.Name == foreign {
			t.Fatal("foreign container listed as managed")
		}
	}

	// Volumes: unmanaged refused, ours listed and removable.
	dockerCLI(t, "", "volume", "create", "gdm2h-foreign-"+sfx)
	dockerCLI(t, "", "volume", "create", "--label", docker.LabelManaged+"=true", "--label", docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
		"--label", docker.LabelServerID+"="+itoa64(sid), "gdm2h-managed-"+sfx)
	if err := c.RemoveVolume(ctx, "gdm2h-foreign-"+sfx); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged volume: %v", err)
	}
	vols, err := c.ListVolumes(ctx)
	if err != nil || !containsVolume(vols, "gdm2h-managed-"+sfx) || containsVolume(vols, "gdm2h-foreign-"+sfx) {
		t.Fatalf("volumes %+v %v", vols, err)
	}
	if err := c.RemoveVolume(ctx, "gdm2h-managed-"+sfx); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImage(ctx, img.ID); !errors.Is(err, docker.ErrInUse) {
		t.Fatalf("in-use image removal: %v", err)
	}
	if err := c.RemoveImage(ctx, mustImageID(t, plainTag)); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged image removal: %v", err)
	}

	// Stop and remove through the client; the foreign container survives.
	if stopped, err := c.StopServer(ctx, ref, 5*time.Second); err != nil || !stopped {
		t.Fatalf("stop %v %v", stopped, err)
	}
	if ct, err := c.ServerContainer(ctx, ref); err != nil || ct.Running {
		t.Fatalf("after stop %+v %v", ct, err)
	}
	if removed, err := c.RemoveServerContainer(ctx, ref, 5*time.Second); err != nil || !removed {
		t.Fatalf("rm %v %v", removed, err)
	}
	if state := dockerCLI(t, "", "container", "inspect", "--format", "{{.State.Status}}", foreign); state != "running" {
		t.Fatalf("foreign container state %q", state)
	}
	if err := c.RemoveImage(ctx, img.ID); err != nil {
		t.Fatalf("remove image: %v", err)
	}
}

// D1 with real Docker: env values arrive byte-identical in the container,
// and a sentinel variable set only in the container env never appears in
// the environment of any docker CLI process.
func TestD1_IntegrationEnvFile(t *testing.T) {
	c, rr := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	img, err := c.ManagedImage(ctx, buildTestImage(t, "gdm2h-d1:"+sfx, true))
	if err != nil {
		t.Fatal(err)
	}
	name := "gdm2h-d1-" + sfx
	t.Cleanup(func() { dockerQuiet("rm", "--force", docker.ContainerName(name)) })
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	const sentinel = "GDM2H_SENTINEL_" + secretMarker
	env := map[string]string{
		"SENTINEL_ONLY_IN_CONTAINER": sentinel,
		"LD_PRELOAD":                 "/nonexistent.so",
		"GODEBUG":                    "panicnil=1",
		"SPACES":                     "  leading and trailing  ",
		"QUOTES":                     `"double" 'single'`,
		"HASH":                       "#not a comment",
		"EQUALS":                     "a=b==c=",
		"DOLLAR":                     "$HOME ${PATH} $(id)",
		"BACKSLASH":                  `C:\path\n\t\\`,
		"UNICODE":                    "ünïcödé ✓ 日本",
		"EMPTY":                      "",
		"TAB":                        "\tx\t",
	}
	sid := serverID()
	id, err := createAndStart(ctx, c, docker.RunSpec{ServerID: sid, ServerName: name, Image: img.ID, HostPort: freePort(t), BindAddress: "127.0.0.1", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range inspect(t, id).Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, v := range env {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("%s: container has %q (present %v), want %q", k, gv, ok, v)
		}
	}
	// And inside the running process (not just the container config).
	waitRunning(t, c, docker.ServerRef{ServerID: sid, ContainerID: id})
	// (busybox is dynamically linked: the container's LD_PRELOAD would make
	// ld.so complain in this exec, which shows it arrived; clear it here.)
	out := dockerCLI(t, "", "exec", "--env", "LD_PRELOAD=", id, "sh", "-c", `printf '[%s]' "$SPACES|$QUOTES|$BACKSLASH|$UNICODE"`)
	if want := "[" + env["SPACES"] + "|" + env["QUOTES"] + "|" + env["BACKSLASH"] + "|" + env["UNICODE"] + "]"; out != want {
		t.Errorf("process env %q, want %q", out, want)
	}
	if out, _ := dockerTry("exec", id, "true"); !strings.Contains(out, "/nonexistent.so") {
		t.Errorf("LD_PRELOAD did not reach the container: %q", out)
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.envs) == 0 {
		t.Fatal("no CLI calls recorded")
	}
	// The env file mechanism of this platform was used.
	wantPath := "/dev/fd/3"
	for _, a := range rr.args {
		if len(a) > 0 && a[0] == "create" {
			i := slices.Index(a, "--env-file")
			if i < 0 {
				t.Fatalf("run without --env-file: %q", a)
			}
			if got := a[i+1]; (runtime.GOOS == "linux") != (got == wantPath) {
				t.Fatalf("env file %q on %s", got, runtime.GOOS)
			}
			t.Logf("%s: docker run --env-file %s", runtime.GOOS, a[i+1])
		}
	}
	for i, e := range rr.envs {
		for _, kv := range e {
			for k, v := range env {
				if v != "" && strings.HasPrefix(kv, k+"=") && strings.Contains(kv, v) {
					t.Fatalf("call %q: container variable %s in the CLI environment", rr.args[i], k)
				}
			}
			if strings.Contains(kv, sentinel) {
				t.Fatalf("call %q: sentinel in the CLI environment", rr.args[i])
			}
		}
		if strings.Contains(strings.Join(rr.args[i], "\x00"), sentinel) {
			t.Fatalf("sentinel in argv %q", rr.args[i])
		}
	}
}

// D1 with real Docker at the size limit: 128 variables and 64 KiB make an
// env file larger than a Linux pipe buffer (64 KiB), so the CLI must read
// it while it is being written; every variable must arrive complete.
func TestD1_IntegrationMaximumEnv(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	img, err := c.ManagedImage(ctx, buildTestImage(t, "gdm2h-d1max:"+sfx, true))
	if err != nil {
		t.Fatal(err)
	}
	name := "gdm2h-d1max-" + sfx
	t.Cleanup(func() { dockerQuiet("rm", "--force", docker.ContainerName(name)) })
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	per := docker.MaxEnvBytes / docker.MaxEnvVars
	for i := 0; i < docker.MaxEnvVars; i++ {
		k := "V" + strconv.Itoa(1000+i)
		env[k] = strings.Repeat(string(rune('a'+i%26)), per-len(k))
	}
	content, err := docker.EncodeEnvFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) <= 64<<10 {
		t.Fatalf("env file of %d bytes does not exceed a pipe buffer", len(content))
	}
	id, err := createAndStart(ctx, c, docker.RunSpec{ServerID: serverID(), ServerName: name, Image: img.ID, HostPort: freePort(t), BindAddress: "127.0.0.1", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range inspect(t, id).Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, v := range env {
		if got[k] != v {
			t.Fatalf("%s: got %d bytes, want %d", k, len(got[k]), len(v))
		}
	}
	t.Logf("%s: %d variables, env file %d bytes, all arrived", runtime.GOOS, len(env), len(content))
}

func volumeNames(t *testing.T) []string {
	t.Helper()
	out := dockerCLI(t, "", "volume", "ls", "--quiet")
	names := strings.Fields(out)
	slices.Sort(names)
	return names
}

// D2 with real Docker: an image VOLUME gets a size-limited noexec tmpfs, no
// anonymous volume is created, and nothing leaks across restart and delete.
func TestD2_IntegrationImageVolumes(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	tag := buildImage(t, "gdm2h-d2:"+sfx, "FROM "+BusyboxImage+"\nVOLUME /data\n"+
		`CMD ["sh", "-c", "echo started; exec sleep 600"]`+"\n", true)
	img, err := c.ManagedImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(img.Volumes, []string{"/data"}) {
		t.Fatalf("image volumes %v", img.Volumes)
	}
	name := "gdm2h-d2-" + sfx
	t.Cleanup(func() { dockerQuiet("rm", "--force", "--volumes", docker.ContainerName(name)) })
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	before := volumeNames(t)
	sid := serverID()
	s := docker.RunSpec{ServerID: sid, ServerName: name, Image: img.ID, ImageVolumes: img.Volumes, HostPort: freePort(t), BindAddress: "127.0.0.1"}
	for round := 0; round < 2; round++ { // start, then "restart" (remove + run)
		id, err := createAndStart(ctx, c, s)
		if err != nil {
			t.Fatal(err)
		}
		ref := docker.ServerRef{ServerID: sid, ContainerID: id}
		waitRunning(t, c, ref)
		insp := inspect(t, id)
		if !strings.Contains(insp.HostConfig.Tmpfs["/data"], "noexec") {
			t.Fatalf("tmpfs %v", insp.HostConfig.Tmpfs)
		}
		for _, m := range insp.Mounts {
			if m.Type == "volume" {
				t.Fatalf("anonymous volume mounted: %+v", insp.Mounts)
			}
		}
		mounts := dockerCLI(t, "", "exec", id, "cat", "/proc/mounts")
		var line string
		for _, l := range strings.Split(mounts, "\n") {
			if f := strings.Fields(l); len(f) > 3 && f[1] == "/data" {
				line = l
			}
		}
		if !strings.HasPrefix(line, "tmpfs /data tmpfs") || !strings.Contains(line, "noexec") || !strings.Contains(line, "nosuid") || !strings.Contains(line, "size=65536k") {
			t.Fatalf("/data mount %q", line)
		}
		if out, err := dockerTry("exec", id, "sh", "-c", "cp /bin/busybox /data/bb && /data/bb true"); err == nil {
			t.Fatalf("executed a binary from /data: %s", out)
		}
		if got := volumeNames(t); !slices.Equal(got, before) {
			t.Fatalf("volumes changed: before %v after %v", before, got)
		}
		if removed, err := c.RemoveServerContainer(ctx, ref, 5*time.Second); err != nil || !removed {
			t.Fatalf("rm %v %v", removed, err)
		}
	}
	if got := volumeNames(t); !slices.Equal(got, before) {
		t.Fatalf("volumes leaked: before %v after %v", before, got)
	}
}

// D3 with real Docker: a hand-started container that carries every one of
// our labels (as an image could make it) is not the server's container.
func TestD3_IntegrationImpostor(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	img, err := c.ManagedImage(ctx, buildTestImage(t, "gdm2h-d3:"+sfx, true))
	if err != nil {
		t.Fatal(err)
	}
	name := "gdm2h-d3-" + sfx
	impostor := "gdm2h-impostor-" + sfx
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", docker.ContainerName(name))
		dockerQuiet("rm", "--force", impostor)
	})
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	sid := serverID()
	id, err := createAndStart(ctx, c, docker.RunSpec{ServerID: sid, ServerName: name, Image: img.ID, HostPort: freePort(t), BindAddress: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ref := docker.ServerRef{ServerID: sid, ContainerID: id}
	impID := dockerCLI(t, "", "run", "--detach", "--name", impostor,
		"--label", docker.LabelManaged+"=true", "--label", docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
		"--label", docker.LabelServerID+"="+itoa64(sid), "--label", docker.LabelServer+"="+name, img.ID, "sleep", "300")
	list, err := c.ManagedContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range list {
		if ct.ID == impID && c.IsServerContainer(ct, ref) {
			t.Fatal("impostor accepted as the server's container")
		}
	}
	if _, err := c.ServerContainer(ctx, docker.ServerRef{ServerID: sid + 1, ContainerID: impID}); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("impostor with another server id: %v", err)
	}
	if ct, err := c.ServerContainer(ctx, ref); err != nil || ct.ID != id {
		t.Fatalf("%+v %v", ct, err)
	}
	if removed, err := c.RemoveServerContainer(ctx, ref, 5*time.Second); err != nil || !removed {
		t.Fatalf("rm %v %v", removed, err)
	}
	if state := dockerCLI(t, "", "container", "inspect", "--format", "{{.State.Status}}", impostor); state != "running" {
		t.Fatalf("impostor state %q", state)
	}
}

// D5 with real Docker: servers cannot reach each other's game port by name
// or IP; their published ports still work from the host.
func TestD5_IntegrationIsolation(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	img, err := c.ManagedImage(ctx, buildTestImage(t, "gdm2h-d5:"+sfx, true))
	if err != nil {
		t.Fatal(err)
	}
	a, b := "gdm2h-d5a-"+sfx, "gdm2h-d5b-"+sfx
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", docker.ContainerName(a))
		dockerQuiet("rm", "--force", docker.ContainerName(b))
	})
	if isolated, err := c.EnsureNetwork(ctx); err != nil || !isolated {
		t.Fatalf("network: %v %v", isolated, err)
	}
	if opt := dockerCLI(t, "", "network", "inspect", "--format", `{{index .Options "com.docker.network.bridge.enable_icc"}}`, docker.NetworkName); opt != "false" {
		t.Fatalf("enable_icc=%q", opt)
	}
	sid := serverID()
	portB := freePort(t)
	idA, err := createAndStart(ctx, c, docker.RunSpec{ServerID: sid, ServerName: a, Image: img.ID, HostPort: freePort(t), BindAddress: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	idB, err := createAndStart(ctx, c, docker.RunSpec{ServerID: sid + 1, ServerName: b, Image: img.ID, HostPort: portB, BindAddress: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, c, docker.ServerRef{ServerID: sid, ContainerID: idA})
	waitRunning(t, c, docker.ServerRef{ServerID: sid + 1, ContainerID: idB})
	ipB := inspect(t, idB).NetworkSettings.Networks[docker.NetworkName].IPAddress
	if ipB == "" {
		t.Fatal("no IP for B")
	}
	// B answers itself (the probe works), A cannot reach B.
	if out, _ := dockerTry("exec", idB, "sh", "-c", "echo x | nc -w 3 127.0.0.1 7777"); !strings.Contains(out, "pong") {
		t.Fatalf("self probe %q", out)
	}
	for _, target := range []string{docker.ContainerName(b), ipB} {
		out, _ := dockerTry("exec", idA, "sh", "-c", "echo x | nc -w 3 "+target+" 7777")
		if strings.Contains(out, "pong") {
			t.Fatalf("A reached B at %s: %q", target, out)
		}
	}
	// The host still reaches B's published port.
	var answer string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(answer, "pong") {
		if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(portB), 3*time.Second); err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 16)
			n, _ := conn.Read(buf)
			conn.Close()
			answer = string(buf[:n])
		}
		if !strings.Contains(answer, "pong") {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !strings.Contains(answer, "pong") {
		t.Fatalf("published port answered %q", answer)
	}
}

// D14 with real Docker: a container that exits during the settle window
// keeps its real exit code (no restart policy yet).
func TestD14_IntegrationExitCode(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	tag := buildImage(t, "gdm2h-d14:"+sfx, "FROM "+BusyboxImage+"\n"+`CMD ["sh", "-c", "echo boom; exit 127"]`+"\n", true)
	img, err := c.ManagedImage(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	name := "gdm2h-d14-" + sfx
	t.Cleanup(func() { dockerQuiet("rm", "--force", docker.ContainerName(name)) })
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	sid := serverID()
	id, err := createAndStart(ctx, c, docker.RunSpec{ServerID: sid, ServerName: name, Image: img.ID, HostPort: freePort(t), BindAddress: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ref := docker.ServerRef{ServerID: sid, ContainerID: id}
	var ct docker.Container
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ct, err = c.ServerContainer(ctx, ref)
		if err == nil && !ct.Running && ct.State == "exited" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // a restart policy would have restarted it by now
	ct, err = c.ServerContainer(ctx, ref)
	if err != nil || ct.Running || ct.ExitCode != 127 || ct.RestartCount != 0 {
		t.Fatalf("%+v %v", ct, err)
	}
}

func waitRunning(t *testing.T, c *docker.Client, ref docker.ServerRef) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ct, err := c.ServerContainer(context.Background(), ref)
		if err == nil && ct.Running {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("container did not reach running")
}

func mustImageID(t *testing.T, ref string) string {
	return dockerCLI(t, "", "image", "inspect", "--format", "{{.Id}}", ref)
}

func containsImage(list []docker.Image, id string) bool {
	for _, i := range list {
		if i.ID == id {
			return true
		}
	}
	return false
}

func containsTag(list []docker.Image, tag string) bool {
	for _, i := range list {
		for _, t := range i.Tags {
			if t == tag {
				return true
			}
		}
	}
	return false
}

func containsVolume(list []docker.Volume, name string) bool {
	for _, v := range list {
		if v.Name == name {
			return true
		}
	}
	return false
}

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func createAndStart(ctx context.Context, c *docker.Client, spec docker.RunSpec) (string, error) {
	id, err := c.CreateServer(ctx, spec)
	if err != nil {
		return "", err
	}
	ref := docker.ServerRef{ServerID: spec.ServerID, ContainerID: id}
	if err := c.StartServer(ctx, ref); err != nil {
		_, _ = c.RemoveServerContainer(context.WithoutCancel(ctx), ref, docker.DefaultStopTimeout)
		return "", err
	}
	return id, nil
}
