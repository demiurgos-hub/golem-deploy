package inspector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func itDocker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func itDockerQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

// buildStandinImage compiles the stand-in hub for the Docker daemon's
// platform and builds an image of it (removed at cleanup).
func buildStandinImage(t *testing.T, tag string) {
	t.Helper()
	arch := itDocker(t, "version", "--format", "{{.Server.Arch}}")
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(dir, "standinhub"), "./inspectortest/standinhub")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dockerfile := "FROM scratch\nCOPY standinhub /standinhub\nVOLUME /var/lib/standin\nENTRYPOINT [\"/standinhub\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	itDocker(t, "build", "--quiet", "--tag", tag, dir)
	t.Cleanup(func() { itDockerQuiet("image", "rm", "--force", tag) })
}

// TestIntegrationInspector applies the Inspector for real with a stand-in
// hub image: the hardened, labelled hub container, the static UI with the
// boot script, a ticket exchanged for a session key, a proxied request and
// a browser WebSocket with that key, an agent relayed from a container on
// the managed network, the target listing, and turning it off again.
func TestIntegrationInspector(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
	if itDocker(t, "version", "--format", "{{.Server.Os}}") != "linux" {
		t.Skip("needs a Linux Docker engine")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b)
	tag := "gdinsp-hub-" + suffix + ":test"
	buildStandinImage(t, tag)
	networkExisted := exec.Command("docker", "network", "inspect", docker.NetworkName).Run() == nil
	ctx := context.Background()
	st, box := openStore(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dc := docker.New(docker.Options{InstanceID: dockertest.IntegrationInstanceID, TempDir: t.TempDir(), Logger: logger,
		BuildConfigDir: filepath.Join(t.TempDir(), "docker-build")})
	runner := jobs.New(st, t.TempDir(), jobs.Options{Logger: logger})
	t.Cleanup(func() { <-runner.Stop() })
	listen := "127.0.0.1:0"
	if runtime.GOOS == "linux" {
		listen = "0.0.0.0:0" // containers reach it at the network's gateway
	}
	s, err := New(ctx, Config{Store: st, Secrets: box, Docker: dc, Jobs: runner, Policy: e2ePolicy(), Logger: logger,
		Image: tag, Listen: listen, GOOS: runtime.GOOS, ReadyTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		if hubs, err := dc.InspectorHubs(context.Background()); err == nil {
			for _, h := range hubs {
				_ = dc.RemoveInspectorHub(context.Background(), h.ID)
			}
		}
		if !networkExisted {
			itDockerQuiet("network", "rm", docker.NetworkName)
		}
	})
	job, err := s.Update(ctx, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.Done(job.ID)
	j, _ := st.GetJob(ctx, job.ID)
	jobLog, _ := os.ReadFile(runner.LogPath(job.ID))
	if j.Status != store.JobSuccess {
		t.Fatalf("apply: %+v\n%s\n%s", j, jobLog, logs.String())
	}
	sec := s.sec
	for _, secret := range []string{sec.Password, sec.AgentToken, sec.TokenKey} {
		if strings.Contains(string(jobLog), secret) || strings.Contains(logs.String(), secret) {
			t.Fatal("a secret reached a log")
		}
	}

	// The container's hardening and labels.
	hubs, err := dc.InspectorHubs(ctx)
	if err != nil || len(hubs) != 1 {
		t.Fatalf("%v %v", hubs, err)
	}
	var insp []struct {
		Args   []string
		Config struct {
			User   string
			Labels map[string]string
			Env    []string
		}
		HostConfig struct {
			ReadonlyRootfs bool
			CapDrop        []string
			SecurityOpt    []string
			Memory         int64
			PidsLimit      int64
			NetworkMode    string
			Tmpfs          map[string]string
			Binds          []string
			RestartPolicy  struct{ Name string }
			PortBindings   map[string][]struct{ HostIp, HostPort string }
		}
		Mounts []any
	}
	if err := json.Unmarshal([]byte(itDocker(t, "container", "inspect", hubs[0].ID)), &insp); err != nil || len(insp) != 1 {
		t.Fatal(err)
	}
	c := insp[0]
	hc := c.HostConfig
	if !hc.ReadonlyRootfs || len(hc.CapDrop) != 1 || !strings.EqualFold(hc.CapDrop[0], "ALL") || !strings.Contains(strings.Join(hc.SecurityOpt, ","), "no-new-privileges") ||
		hc.Memory != 512<<20 || hc.PidsLimit != 256 || hc.NetworkMode != docker.NetworkName || c.Config.User != "65532:65532" ||
		hc.Tmpfs["/tmp"] == "" || hc.Tmpfs["/var/lib/standin"] == "" || len(hc.Binds) != 0 || len(c.Mounts) != 0 || hc.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("hardening: %+v %+v", hc, c.Config.User)
	}
	pb := hc.PortBindings["8787/tcp"]
	if len(pb) != 1 || pb[0].HostIp != "127.0.0.1" || pb[0].HostPort != "" {
		t.Fatalf("port bindings %+v", hc.PortBindings)
	}
	if c.Config.Labels[docker.LabelInspectorHub] != "true" || c.Config.Labels[docker.LabelInstance] != dockertest.IntegrationInstanceID ||
		c.Config.Labels[docker.LabelInspector] != "" || c.Config.Labels[docker.LabelConfigHash] == "" {
		t.Fatalf("labels %v", c.Config.Labels)
	}
	for _, a := range c.Args {
		if strings.Contains(a, sec.AgentToken) || strings.Contains(a, sec.PasswordHash) {
			t.Fatal("secret in the container's arguments")
		}
	}
	if out := itDocker(t, "logs", hubs[0].ID); !strings.Contains(out, "uid=65532 root=read-only") {
		t.Fatalf("hub output %q", out)
	}

	// Ticket, entry, proxied request.
	acct, _ := st.CreateAccount(ctx, store.NewAccount{Username: "dev", PasswordHash: "x", Role: store.RoleDeveloper, Approved: true})
	p := 7101
	sv, err := st.CreateServer(ctx, store.Server{Name: "itest-arena-" + suffix, Port: &p, Inspector: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ses, err := s.CreateSession(ctx, acct, &sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(s.port())
	// The static UI comes without a session and carries the boot script.
	resp, err := plainClient().Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(page, "<head>"+bootTag) || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("static UI: %d %q", resp.StatusCode, page)
	}
	entry := entryOf(t, postTicket(t, plainClient(), base, ticketOf(t, ses.Path), base, nil))
	if entry.TargetID != sv.Name {
		t.Fatalf("target %q", entry.TargetID)
	}
	req, _ := testRequest("GET", base+"/api/targets", entry.Key)
	resp, err = plainClient().Do(req)
	if err != nil || resp.StatusCode != 200 || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("proxied: %v %v", resp, err)
	}
	resp.Body.Close()
	req, _ = testRequest("GET", base+"/api/targets", "")
	req.Header.Set("Cookie", "golem_deploy_inspector="+entry.Key)
	if resp, err = plainClient().Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie: %v %v", resp, err)
	}
	resp.Body.Close()
	conn, br, wsResp := dialWSResponse(t, "127.0.0.1:"+strconv.Itoa(s.port()), "/ws/client", http.Header{
		"Sec-WebSocket-Protocol": {WebSocketProtocol + ", " + WebSocketKeyPrefix + entry.Key}, "Origin": {base}})
	if wsResp.StatusCode != http.StatusSwitchingProtocols || wsResp.Header.Get("Sec-WebSocket-Protocol") != WebSocketProtocol {
		t.Fatalf("browser WebSocket: %d %v", wsResp.StatusCode, wsResp.Header)
	}
	echoOnce(t, conn, br, "ping")
	conn.Close()
	if strings.Contains(logs.String(), entry.Key) {
		t.Fatal("the session key reached the log")
	}

	// An agent in a container on the managed network, relayed through the
	// listener.
	agent, ok := s.Agent(ctx)
	if !ok {
		t.Fatalf("no agent address (listen %s on %s)", listen, runtime.GOOS)
	}
	tok, _ := s.AgentToken(sv.ID)
	name := "gdinsp-agent-" + suffix
	envFile := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(envFile, []byte("GOLEM_INSPECTOR_HUB_URL="+agent.HubURL+"\nGOLEM_INSPECTOR_TOKEN="+tok+"\nGOLEM_INSPECTOR_TARGET_ID="+sv.Name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { itDockerQuiet("rm", "--force", name) })
	itDocker(t, "run", "--detach", "--name", name, "--network", docker.NetworkName, "--read-only", "--cap-drop", "ALL", "--user", "65532:65532",
		"--env-file", envFile, "--entrypoint", "/standinhub", tag, "agent")
	deadline := time.Now().Add(30 * time.Second)
	var status apitypes.Inspector
	for time.Now().Before(deadline) {
		status, err = s.Status(ctx)
		if err == nil && len(status.Targets) == 1 && status.Targets[0].Connected {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(status.Targets) != 1 || status.Targets[0].ServerID != sv.ID || !status.Targets[0].Connected || status.Targets[0].Entities != 3 ||
		status.State != apitypes.InspectorStateRunning {
		t.Fatalf("status %+v\nagent output:\n%s\ndaemon log:\n%s", status, itDocker(t, "logs", name), logs.String())
	}
	if !status.AgentsReachable || status.AgentAddress == "" {
		t.Fatalf("%+v", status)
	}
	// Off: the listener closes and the container goes.
	job, err = s.Update(ctx, apitypes.UpdateInspectorRequest{Enabled: boolPtr(false)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-runner.Done(job.ID)
	if j, _ := st.GetJob(ctx, job.ID); j.Status != store.JobSuccess {
		t.Fatalf("off: %+v", j)
	}
	if hubs, _ := dc.InspectorHubs(ctx); len(hubs) != 0 {
		t.Fatal("hub kept")
	}
}

// testRequest builds a request with the session key as a bearer header
// (when not empty).
func testRequest(method, url, key string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)
	if err == nil && key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, err
}
