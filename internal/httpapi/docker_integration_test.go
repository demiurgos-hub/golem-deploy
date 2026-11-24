package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func itestDocker(t *testing.T, stdin string, args ...string) string {
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

func itestQuiet(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", args...).Run()
}

// itestRemoveVolumes removes the data volumes the integration instance
// created for the named servers (and nothing else).
func itestRemoveVolumes(servers ...string) {
	for _, name := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, _ := exec.CommandContext(ctx, "docker", "volume", "ls", "--quiet",
			"--filter", "label="+docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
			"--filter", "label="+docker.LabelServer+"="+name).Output()
		cancel()
		for _, v := range strings.Fields(string(out)) {
			itestQuiet("volume", "rm", "--", v)
		}
	}
}

// served returns the SHA-256 of the leaf certificate served on
// 127.0.0.1:port ("" when the handshake fails).
func served(port int) string {
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // comparing fingerprints
	if err != nil {
		return ""
	}
	defer conn.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// TestIntegrationServersAPI drives the server API against a real Docker
// daemon (GOLEM_DEPLOY_DOCKER_TESTS=1) with a statically compiled fake game
// in a FROM scratch image: health-gated start, the runtime view, the
// certificate bind mount (read by uid 65532) and certificate replacement
// without a restart, a start that never becomes ready, and the isolation
// guarantees of the previous milestone.
func TestIntegrationServersAPI(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	sfx := hex.EncodeToString(b)
	_, netErr := exec.Command("docker", "network", "inspect", docker.NetworkName).Output()
	networkExisted := netErr == nil
	name := "gdm3-itapi-" + sfx
	stuck := "gdm3-itapi-stuck-" + sfx
	impostor := "gdm3-itapi-impostor-" + sfx
	t.Cleanup(func() {
		itestQuiet("rm", "--force", docker.ContainerName(name), docker.ContainerName(stuck), impostor)
		itestRemoveVolumes(name, stuck, "gdm3-wrong-"+sfx)
		if !networkExisted {
			itestQuiet("network", "rm", docker.NetworkName)
		}
	})

	h := newHarness(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	// The builds row comes first: its id goes into the image's labels.
	proj, err := h.st.CreateProject(ctx, store.Project{Name: "itgame", RepoURL: "https://example.com/itgame.git"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	build, _ := h.st.CreateBuild(ctx, store.NewBuild{ProjectID: proj.ID})
	commit := strings.Repeat("c", 40)
	_ = h.st.MarkBuildRunning(ctx, build.ID, "main", commit)
	tag := "golem-deploy/itgame-" + sfx + ":" + commit
	imageID := gameadmintest.BuildFakeGameImage(t, tag,
		docker.LabelManaged+"=true", docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
		docker.LabelProjectID+"="+strconv.FormatInt(proj.ID, 10), docker.LabelCommit+"="+commit,
		docker.LabelBuildID+"="+strconv.FormatInt(build.ID, 10), docker.LabelEngineVersion+"=v0.4.0-rc.1")
	if err := h.st.FinishBuild(ctx, build.ID, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: imageID, ImageTag: tag, EngineVersion: "v0.4.0-rc.1"}); err != nil {
		t.Fatal(err)
	}
	// The same program labelled for another build must not run.
	otherBuild, _ := h.st.CreateBuild(ctx, store.NewBuild{ProjectID: proj.ID})
	_ = h.st.MarkBuildRunning(ctx, otherBuild.ID, "main", commit)
	wrongTag := "golem-deploy/itgame-wrong-" + sfx + ":x"
	wrongID := gameadmintest.BuildFakeGameImage(t, wrongTag,
		docker.LabelManaged+"=true", docker.LabelInstance+"="+dockertest.IntegrationInstanceID, docker.LabelBuildID+"=999999")
	_ = h.st.FinishBuild(ctx, otherBuild.ID, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: wrongID})

	dc := docker.New(docker.Options{Logger: logger, InstanceID: dockertest.IntegrationInstanceID, TempDir: t.TempDir()})
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	cm, err := certs.Open(certs.Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: h.st, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	// A free 10-port window for the range.
	lo := 0
	for p := 7600; p < 7990; p += 10 {
		if ports.ProbeFree("127.0.0.1", p) == nil {
			lo = p
			break
		}
	}
	if lo == 0 {
		t.Fatal("no free port window")
	}
	bs, err := backups.Open(filepath.Join(t.TempDir(), "backups"), backups.Options{CheckFree: func(string, int64) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	svc := servers.New(h.st, box, dc, servers.Config{Ports: ports.Range{Lo: lo, Hi: lo + 9}, BindAddress: "127.0.0.1", StopTimeout: 5 * time.Second,
		StartTimeout: 20 * time.Second, DefaultGameHost: "localhost", Certs: cm, Backups: bs}, logger)
	runner := jobs.New(h.st, t.TempDir(), jobs.Options{Logger: logger})
	t.Cleanup(func() { _ = runner.Shutdown(context.Background()) })
	srv, err := New(Config{Listen: "127.0.0.1:0", PublicHosts: []string{"example.com"}}, Deps{Store: h.st, Sessions: h.sessions, Hasher: h.hasher,
		Secrets: box, Jobs: runner, Logger: logger, Docker: dc, Servers: svc, Certs: cm})
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.runner = srv, runner
	admin := h.account("admin1", store.RoleAdmin, true)
	tok := h.session(admin)
	runJob := func(rr *httptest.ResponseRecorder) store.Job {
		t.Helper()
		if rr.Code != http.StatusAccepted {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		job := decode[apitypes.Job](t, rr)
		select {
		case <-runner.Done(job.ID):
		case <-time.After(2 * time.Minute):
			t.Fatal("job timeout")
		}
		j, _ := h.st.GetJob(ctx, job.ID)
		return j
	}
	wait := func(rr *httptest.ResponseRecorder) store.Job {
		t.Helper()
		j := runJob(rr)
		if j.Status != store.JobSuccess {
			log, _ := os.ReadFile(runner.LogPath(j.ID))
			t.Fatalf("job %s %q\n%s", j.Status, j.Detail, log)
		}
		return j
	}

	rr := h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: name, BuildID: build.ID, Env: map[string]string{
		"GAME_SECRET": envMarker, "LD_PRELOAD": "/nonexistent.so", "MOTD": `say "hi" #1 $x \ ünï`, "FAKEGAME_POLL": "1s"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	sv := decode[apitypes.Server](t, rr)
	base := fmt.Sprintf("/api/v1/servers/%d", sv.ID)

	// An image whose build-id label names another build is refused before
	// anything runs.
	wrong := decode[apitypes.Server](t, h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: "gdm3-wrong-" + sfx, BuildID: otherBuild.ID}))
	if j := runJob(h.do("POST", fmt.Sprintf("/api/v1/servers/%d/start", wrong.ID), tok, nil)); j.Status != store.JobError || !strings.Contains(j.Detail, "refusing to run it") {
		t.Fatalf("wrong build image: %s %q", j.Status, j.Detail)
	}

	// Health-gated start.
	startJob := wait(h.do("POST", base+"/start", tok, nil))
	got := decode[apitypes.Server](t, h.do("GET", base, tok, nil))
	if got.State != apitypes.ServerStateRunning || got.Port != lo || got.PublicURL != fmt.Sprintf("https://localhost:%d/", lo) {
		t.Fatalf("%+v", got)
	}
	ro := itestDocker(t, "", "container", "inspect", "--format",
		"{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}} {{.HostConfig.Init}} {{.HostConfig.RestartPolicy.Name}} {{json .Config.Healthcheck.Test}}", got.ContainerID)
	if ro != `true [ALL] 65532:65532 true on-failure ["NONE"]` {
		t.Fatalf("hardening %q", ro)
	}
	envJSON := itestDocker(t, "", "container", "inspect", "--format", "{{json .Config.Env}}", got.ContainerID)
	if !strings.Contains(envJSON, `"MOTD=say \"hi\" #1 $x \\ ünï"`) || !strings.Contains(envJSON, `"LD_PRELOAD=/nonexistent.so"`) ||
		!strings.Contains(envJSON, fmt.Sprintf(`"GOLEM_PUBLIC_URL=https://localhost:%d/wt"`, lo)) {
		t.Fatalf("container env %s", envJSON)
	}
	adminBind := itestDocker(t, "", "container", "inspect", "--format", `{{json (index .NetworkSettings.Ports "7778/tcp")}}`, got.ContainerID)
	if !strings.Contains(adminBind, `"HostIp":"127.0.0.1"`) || strings.Contains(adminBind, `"0.0.0.0"`) {
		t.Fatalf("admin port binding %s", adminBind)
	}
	// The certificate mount is readable by uid 65532 (the game could not
	// have become ready otherwise) and is read-only.
	startLog, _ := os.ReadFile(runner.LogPath(startJob.ID))
	if !strings.Contains(string(startLog), "game ready: engine v0.4.0-rc.1, transport webtransport, reported schema itest-fp-1") {
		t.Fatalf("start log:\n%s", startLog)
	}
	mounts := itestDocker(t, "", "container", "inspect", "--format", "{{range .Mounts}}{{.Destination}} {{.RW}};{{end}}", got.ContainerID)
	if !strings.Contains(mounts, certs.ContainerDir+" false;") {
		t.Fatalf("mounts %q", mounts)
	}
	if b, _ := h.st.GetBuild(ctx, build.ID); b.SchemaFingerprint != "itest-fp-1" {
		t.Fatalf("fingerprint %q", b.SchemaFingerprint)
	}

	// Runtime view from the admin endpoint.
	rt := decode[apitypes.ServerRuntime](t, h.do("GET", base+"/runtime", tok, nil))
	if !rt.Ready || rt.Entities != 7 || rt.Tick.Rate != 20 || rt.SchemaFingerprint != "itest-fp-1" || rt.Transport != "webtransport" || !rt.CertificateHashesPublished {
		t.Fatalf("runtime %+v", rt)
	}

	// The game serves the server's certificate on the published TCP
	// port, and picks up a replacement without a restart.
	before, err := cm.Current(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fp := served(lo); fp != before.Fingerprint {
		t.Fatalf("served %q, installed %q", fp, before.Fingerprint)
	}
	rr = h.do("PUT", "/api/v1/certificates/game", tok, apitypes.UpdateGameCertificateRequest{Mode: apitypes.CertificateModeSelfSigned})
	mode := decode[apitypes.UpdateGameCertificateResponse](t, rr).Certificate
	if mode.Certificate != nil {
		t.Fatalf("%+v", mode)
	}
	after := decode[apitypes.Server](t, h.do("GET", base, tok, nil))
	if after.Certificate == nil || after.Certificate.FingerprintSHA256 == before.Fingerprint || !after.Certificate.CoversGameHost {
		t.Fatalf("%+v", after)
	}
	waitForCertificate := func(fingerprint string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for served(lo) != fingerprint {
			if time.Now().After(deadline) {
				t.Fatal("replaced certificate not picked up by the running game")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	waitForCertificate(after.Certificate.FingerprintSHA256)
	if again := decode[apitypes.Server](t, h.do("GET", base, tok, nil)); again.RestartRequired || again.ContainerID != got.ContainerID {
		t.Fatalf("certificate replacement restarted or staled the server: %+v", again)
	}
	customChain, customKey := testCertPEM(t, "localhost", time.Now().Add(time.Hour))
	custom := decode[apitypes.UpdateGameCertificateResponse](t, h.do("PUT", "/api/v1/certificates/game", tok, apitypes.UpdateGameCertificateRequest{
		Mode: apitypes.CertificateModeCustom, CertificatePEM: customChain, PrivateKeyPEM: customKey,
	})).Certificate
	if custom.Certificate == nil {
		t.Fatal("missing uploaded certificate")
	}
	waitForCertificate(custom.Certificate.FingerprintSHA256)
	if again := decode[apitypes.Server](t, h.do("GET", base, tok, nil)); !again.RestartRequired || again.ContainerID != got.ContainerID {
		t.Fatalf("mode change did not flag the pinning configuration: %+v", again)
	}
	_ = decode[apitypes.UpdateGameCertificateResponse](t, h.do("PUT", "/api/v1/certificates/game", tok, apitypes.UpdateGameCertificateRequest{Mode: apitypes.CertificateModeSelfSigned}))
	back := decode[apitypes.Server](t, h.do("GET", base, tok, nil))
	if back.Certificate == nil || back.Certificate.FingerprintSHA256 == after.Certificate.FingerprintSHA256 || back.ContainerID != got.ContainerID {
		t.Fatalf("switching back did not regenerate the running server's certificate: %+v", back)
	}
	waitForCertificate(back.Certificate.FingerprintSHA256)

	// D3: an impostor with every one of our labels is not the server's
	// container.
	itestDocker(t, "", "run", "--detach", "--name", impostor,
		"--label", docker.LabelManaged+"=true", "--label", docker.LabelInstance+"="+dockertest.IntegrationInstanceID,
		"--label", docker.LabelServerID+"="+fmt.Sprint(sv.ID), "--label", docker.LabelServer+"="+name, tag)
	if again := decode[apitypes.Server](t, h.do("GET", base, tok, nil)); again.ContainerID != got.ContainerID {
		t.Fatalf("container id changed to %s", again.ContainerID)
	}

	// Follow the log over a real connection and restart the server: the
	// stream ends with the old container.
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+base+"/logs?follow=1&tail=5", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	sawTick := false
	timeout := time.After(15 * time.Second)
	for !sawTick {
		select {
		case l := <-lines:
			sawTick = strings.Contains(l, "tick")
		case <-timeout:
			t.Fatal("no streamed log lines")
		}
	}
	if st := decode[apitypes.ServerStats](t, h.do("GET", base+"/stats", tok, nil)); st.PIDs < 1 {
		t.Fatalf("stats %+v", st)
	}
	patched := decode[apitypes.Server](t, h.do("PATCH", base, tok, apitypes.UpdateServerRequest{AllowedOrigins: &[]string{"https://web.example.com"}}))
	if !patched.RestartRequired {
		t.Fatal("restart_required not set")
	}
	wait(h.do("POST", base+"/restart", tok, nil))
	if got := decode[apitypes.Server](t, h.do("GET", base, tok, nil)); got.RestartRequired || got.State != apitypes.ServerStateRunning {
		t.Fatalf("after restart %+v", got)
	}
	ended := time.After(30 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-lines:
		case <-ended:
			t.Fatal("follow stream did not end when the container went away")
		}
	}

	// A game that never reports ready fails the start, is stopped (not
	// crash-looped) and keeps its logs.
	rr = h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: stuck, BuildID: build.ID, Env: map[string]string{"FAKEGAME_NEVER_READY": "1"}})
	stuckSv := decode[apitypes.Server](t, rr)
	quick := servers.New(h.st, box, dc, servers.Config{Ports: ports.Range{Lo: lo, Hi: lo + 9}, BindAddress: "127.0.0.1", StopTimeout: 5 * time.Second,
		StartTimeout: 4 * time.Second, DefaultGameHost: "localhost", Certs: cm}, logger)
	var stuckLog bytes.Buffer
	err = quick.Start(ctx, stuckSv.ID, &stuckLog)
	if err == nil || !strings.Contains(err.Error(), "did not report ready within 4s") {
		t.Fatalf("got %v\n%s", err, stuckLog.String())
	}
	state := itestDocker(t, "", "container", "inspect", "--format", "{{.State.Status}} {{.HostConfig.RestartPolicy.Name}}", docker.ContainerName(stuck))
	if state != "exited no" {
		t.Fatalf("failed start container %q", state)
	}
	if body := h.do("GET", fmt.Sprintf("/api/v1/servers/%d/logs", stuckSv.ID), tok, nil).Body.String(); !strings.Contains(body, "fakegame started as uid=65532") {
		t.Fatalf("logs of the failed start %q", body)
	}
	errBody(t, h.do("GET", fmt.Sprintf("/api/v1/servers/%d/runtime", stuckSv.ID), tok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)

	wait(h.do("POST", base+"/stop", tok, nil))
	if state := itestDocker(t, "", "container", "inspect", "--format", "{{.State.Status}}", impostor); state != "running" {
		t.Fatalf("impostor state %q after stop", state)
	}
	for _, id := range []int64{sv.ID, stuckSv.ID, wrong.ID} {
		if rr := h.do("DELETE", fmt.Sprintf("/api/v1/servers/%d", id), tok, nil); rr.Code != http.StatusNoContent {
			t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
		}
	}
	if out := itestDocker(t, "", "ps", "--all", "--quiet", "--filter", "name=^/"+docker.ContainerName(name)+"$"); out != "" {
		t.Fatalf("container left: %s", out)
	}
	if state := itestDocker(t, "", "container", "inspect", "--format", "{{.State.Status}}", impostor); state != "running" {
		t.Fatalf("impostor state %q after delete", state)
	}
	if strings.Contains(logs.String(), envMarker) {
		t.Fatal("env value in daemon log")
	}
}
