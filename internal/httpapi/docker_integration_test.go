package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

const itestBusybox = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

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

// TestIntegrationServersAPI drives the server API against a real Docker
// daemon (GOLEM_DEPLOY_DOCKER_TESTS=1).
func TestIntegrationServersAPI(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	sfx := hex.EncodeToString(b)
	_, netErr := exec.Command("docker", "network", "inspect", docker.NetworkName).Output()
	networkExisted := netErr == nil
	tag := "golem-deploy-itest-api:" + sfx
	plain := "golem-deploy-itest-api-plain:" + sfx
	name := "itapi-" + sfx
	dockerfile := "FROM " + itestBusybox + "\n" +
		`CMD ["sh", "-c", "echo started; i=0; while true; do i=$((i+1)); echo tick $i; sleep 1; done"]` + "\n"
	itestDocker(t, dockerfile, "build", "--quiet", "--tag", tag, "--label", docker.LabelManaged+"=true", "-")
	itestDocker(t, dockerfile, "build", "--quiet", "--tag", plain, "-")
	itestDocker(t, "", "volume", "create", "itapi-foreign-"+sfx)
	t.Cleanup(func() {
		itestQuiet("rm", "--force", docker.ContainerName(name))
		itestQuiet("image", "rm", "--force", tag, plain)
		itestQuiet("volume", "rm", "--force", "itapi-foreign-"+sfx)
		if !networkExisted {
			itestQuiet("network", "rm", docker.NetworkName)
		}
	})

	h := newHarness(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	dc := docker.New(docker.Options{Logger: logger})
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
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
	svc := servers.New(h.st, box, dc, servers.Config{Ports: ports.Range{Lo: lo, Hi: lo + 9}, BindAddress: "127.0.0.1", SettleWindow: 2 * time.Second, StopTimeout: 5 * time.Second}, logger)
	runner := jobs.New(h.st, t.TempDir(), jobs.Options{Logger: logger})
	t.Cleanup(func() { _ = runner.Shutdown(context.Background()) })
	srv, err := New(Config{Listen: "127.0.0.1:0", PublicHosts: []string{"example.com"}}, Deps{Store: h.st, Sessions: h.sessions, Hasher: h.hasher, Secrets: box, Jobs: runner, Logger: logger, Docker: dc, Servers: svc})
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.runner = srv, runner
	admin := h.account("admin1", store.RoleAdmin, true)
	tok := h.session(admin)
	wait := func(rr *httptest.ResponseRecorder) store.Job {
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
		j, _ := h.st.GetJob(context.Background(), job.ID)
		if j.Status != store.JobSuccess {
			log, _ := os.ReadFile(runner.LogPath(j.ID))
			t.Fatalf("job %s %q\n%s", j.Status, j.Detail, log)
		}
		return j
	}

	errBody(t, h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: name, Image: plain}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	errBody(t, h.do("DELETE", "/api/v1/volumes/itapi-foreign-"+sfx, tok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)

	rr := h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: name, Image: tag, Env: map[string]string{"GAME_SECRET": envMarker}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	sv := decode[apitypes.Server](t, rr)
	base := fmt.Sprintf("/api/v1/servers/%d", sv.ID)
	wait(h.do("POST", base+"/start", tok, nil))
	got := decode[apitypes.Server](t, h.do("GET", base, tok, nil))
	if got.State != apitypes.ServerStateRunning || got.Port != lo {
		t.Fatalf("%+v", got)
	}
	ro := itestDocker(t, "", "container", "inspect", "--format", "{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}}", got.ContainerID)
	if ro != "true [ALL] 65532:65532" {
		t.Fatalf("hardening %q", ro)
	}

	// Follow the log over a real connection and stop the server: the
	// stream ends when the container stops.
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
	patched := decode[apitypes.Server](t, h.do("PATCH", base, tok, apitypes.UpdateServerRequest{EnvSet: map[string]string{"MODE": "x"}}))
	if !patched.RestartRequired {
		t.Fatal("restart_required not set")
	}
	wait(h.do("POST", base+"/restart", tok, nil))
	if got := decode[apitypes.Server](t, h.do("GET", base, tok, nil)); got.RestartRequired || got.State != apitypes.ServerStateRunning {
		t.Fatalf("after restart %+v", got)
	}
	// The stream from before the restart ended with the old container.
	ended := time.After(30 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-lines:
		case <-ended:
			t.Fatal("follow stream did not end when the container went away")
		}
	}
	wait(h.do("POST", base+"/stop", tok, nil))
	if rr := h.do("DELETE", base, tok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if out := itestDocker(t, "", "ps", "--all", "--quiet", "--filter", "name="+docker.ContainerName(name)); out != "" {
		t.Fatalf("container left: %s", out)
	}
	if strings.Contains(logs.String(), envMarker) {
		t.Fatal("env value in daemon log")
	}
}
