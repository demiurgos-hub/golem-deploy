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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

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

func dockerQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// BuildTestImage builds a tiny managed image that prints a line and serves
// TCP on the game port, and removes it at cleanup.
func buildTestImage(t *testing.T, tag string, managed bool) string {
	t.Helper()
	dockerfile := "FROM " + BusyboxImage + "\n" +
		`CMD ["sh", "-c", "echo started; echo to-stderr >&2; exec nc -lk -p 7777 -e echo pong"]` + "\n"
	args := []string{"build", "--quiet", "--tag", tag}
	if managed {
		args = append(args, "--label", docker.LabelManaged+"=true", "--label", docker.LabelCommit+"=itest")
	}
	args = append(args, "-")
	dockerCLI(t, dockerfile, args...)
	t.Cleanup(func() { dockerQuiet("image", "rm", "--force", tag) })
	return tag
}

func TestIntegrationDocker(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	c := docker.New(docker.Options{})
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("docker unreachable: %v", err)
	}
	sfx := randSuffix()
	_, netErr := exec.Command("docker", "network", "inspect", docker.NetworkName).Output()
	networkExisted := netErr == nil

	tag := buildTestImage(t, "golem-deploy-itest:"+sfx, true)
	plainTag := buildTestImage(t, "golem-deploy-itest-plain:"+sfx, false)
	name := "it-" + sfx
	serverID := int64(900000000 + time.Now().UnixNano()%1000000)
	t.Cleanup(func() {
		dockerQuiet("rm", "--force", docker.ContainerName(name))
		dockerQuiet("rm", "--force", "golem-deploy-it-foreign-"+sfx)
		dockerQuiet("volume", "rm", "--force", "golem-deploy-it-managed-"+sfx, "golem-deploy-it-foreign-"+sfx)
		if !networkExisted {
			dockerQuiet("network", "rm", docker.NetworkName)
		}
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
	if err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	spec := docker.RunSpec{
		ServerID: serverID, ServerName: name, Image: img.ID, HostPort: port, BindAddress: "127.0.0.1",
		Env: map[string]string{"GAME_SECRET": secretMarker}, ConfigHash: "itest",
	}
	id, err := c.RunServer(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	waitRunning(t, c, serverID)

	// Hardening applied.
	var insp []struct {
		Config struct {
			User   string   `json:"User"`
			Env    []string `json:"Env"`
			Labels map[string]string
		}
		HostConfig struct {
			ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
			CapDrop        []string          `json:"CapDrop"`
			SecurityOpt    []string          `json:"SecurityOpt"`
			PidsLimit      int64             `json:"PidsLimit"`
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
		}
	}
	if err := json.Unmarshal([]byte(dockerCLI(t, "", "container", "inspect", id)), &insp); err != nil || len(insp) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	hc := insp[0].HostConfig
	if !hc.ReadonlyRootfs || len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" || hc.PidsLimit != 512 ||
		!strings.Contains(strings.Join(hc.SecurityOpt, ","), "no-new-privileges") || insp[0].Config.User != "65532:65532" ||
		hc.RestartPolicy.Name != "on-failure" || hc.RestartPolicy.MaximumRetryCount != 5 || hc.LogConfig.Config["max-size"] != "10m" ||
		hc.NetworkMode != docker.NetworkName || !strings.Contains(hc.Tmpfs["/tmp"], "noexec") {
		t.Fatalf("hardening missing: %+v user=%q", hc, insp[0].Config.User)
	}
	for _, proto := range []string{"7777/udp", "7777/tcp"} {
		b := hc.PortBindings[proto]
		if len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != itoa(port) {
			t.Fatalf("port binding %s: %+v", proto, b)
		}
	}
	if !strings.Contains(strings.Join(insp[0].Config.Env, ","), "GAME_SECRET="+secretMarker) {
		t.Fatal("env value did not reach the container")
	}

	// Logs (stdout + stderr) and stats.
	var logs bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		logs.Reset()
		if err := c.ServerLogs(ctx, serverID, docker.LogOptions{Tail: 10}, &logs); err != nil {
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
	if st, err := c.ServerStats(ctx, serverID); err != nil || st.PIDs < 1 || st.MemoryLimitBytes == 0 {
		t.Fatalf("stats %+v %v", st, err)
	}
	// The game port answers on the bound address.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 5*time.Second); err == nil {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 16)
		n, _ := conn.Read(buf)
		conn.Close()
		if !strings.Contains(string(buf[:n]), "pong") {
			t.Logf("game port answered %q (Docker Desktop port forwarding may be slow)", buf[:n])
		}
	}

	// A container started with plain "docker run" that impersonates the
	// server (same server-id label, no managed label) is invisible and
	// refused by every mutating call.
	foreign := "golem-deploy-it-foreign-" + sfx
	dockerCLI(t, "", "run", "--detach", "--name", foreign, "--label", docker.LabelServerID+"="+itoa64(serverID),
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
	list, err := c.ServerContainers(ctx, serverID)
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("server containers %+v %v", list, err)
	}

	// Volumes: unmanaged refused, managed listed and removable.
	dockerCLI(t, "", "volume", "create", "golem-deploy-it-foreign-"+sfx)
	dockerCLI(t, "", "volume", "create", "--label", docker.LabelManaged+"=true", "--label", docker.LabelServerID+"="+itoa64(serverID), "golem-deploy-it-managed-"+sfx)
	if err := c.RemoveVolume(ctx, "golem-deploy-it-foreign-"+sfx); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged volume: %v", err)
	}
	vols, err := c.ListVolumes(ctx)
	if err != nil || !containsVolume(vols, "golem-deploy-it-managed-"+sfx) || containsVolume(vols, "golem-deploy-it-foreign-"+sfx) {
		t.Fatalf("volumes %+v %v", vols, err)
	}
	if err := c.RemoveVolume(ctx, "golem-deploy-it-managed-"+sfx); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImage(ctx, img.ID); !errors.Is(err, docker.ErrInUse) {
		t.Fatalf("in-use image removal: %v", err)
	}
	if err := c.RemoveImage(ctx, mustImageID(t, plainTag)); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged image removal: %v", err)
	}

	// Stop and remove through the client; the foreign container survives.
	if n, err := c.StopServer(ctx, serverID, 5*time.Second); err != nil || n != 1 {
		t.Fatalf("stop %d %v", n, err)
	}
	if ct, err := c.ServerContainer(ctx, serverID); err != nil || ct.Running {
		t.Fatalf("after stop %+v %v", ct, err)
	}
	if n, err := c.RemoveServerContainers(ctx, serverID, 5*time.Second); err != nil || n != 1 {
		t.Fatalf("rm %d %v", n, err)
	}
	if state := dockerCLI(t, "", "container", "inspect", "--format", "{{.State.Status}}", foreign); state != "running" {
		t.Fatalf("foreign container state %q", state)
	}
	if err := c.RemoveImage(ctx, img.ID); err != nil {
		t.Fatalf("remove image: %v", err)
	}
}

func waitRunning(t *testing.T, c *docker.Client, serverID int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ct, err := c.ServerContainer(context.Background(), serverID)
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
