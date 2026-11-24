package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// TestIntegrationBuildToServer is the vertical slice against a real Docker
// daemon (GOLEM_DEPLOY_DOCKER_TESTS=1; network access for the base images,
// Go modules and npm): the fixture game in internal/build/testdata/game is
// built through the API, a server runs that build, its start is gated on
// the game's admin /ready, the runtime view and the build's schema
// fingerprint come from the game, and the game serves its page and its
// realtime config (public URL, fallback URL, certificate hashes) over HTTPS
// with the installation's certificate. No GitHub access is needed: the
// checkout is a copy of the fixture.
func TestIntegrationBuildToServer(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
	ctx := context.Background()
	sfx := make([]byte, 4)
	_, _ = rand.Read(sfx)
	project := "gdm3b-slice-" + hex.EncodeToString(sfx)
	serverName := project
	_, netErr := exec.Command("docker", "network", "inspect", docker.NetworkName).Output()
	networkExisted := netErr == nil

	h := newHarness(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	dir := t.TempDir()
	repoDir, tmpDir := filepath.Join(dir, "repositories"), filepath.Join(dir, "tmp")
	p, err := h.st.CreateProject(ctx, store.Project{Name: project, RepoURL: "https://git.example.com/fixture.git", Branch: "main",
		LastCommit: strings.Repeat("5e", 20), Status: "idle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	copyFixture(t, filepath.Join("..", "build", "testdata", "game"), filepath.Join(repoDir, fmt.Sprint(p.ID)))

	dc := docker.New(docker.Options{Logger: logger, InstanceID: dockertest.IntegrationInstanceID, TempDir: tmpDir})
	box, _ := secrets.New(bytes.Repeat([]byte{5}, secrets.KeySize))
	cm, err := certs.Open(certs.Options{Dir: filepath.Join(dir, "certs", "game"), Settings: h.st, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	lo := 0
	for port := 7900; port < 7990; port += 10 {
		if ports.ProbeFree("127.0.0.1", port) == nil && ports.ProbeFree("127.0.0.1", port+1) == nil {
			lo = port
			break
		}
	}
	if lo == 0 {
		t.Fatal("no free port window")
	}
	svc := servers.New(h.st, box, dc, servers.Config{Ports: ports.Range{Lo: lo, Hi: lo + 9}, BindAddress: "127.0.0.1", StopTimeout: 5 * time.Second,
		StartTimeout: 60 * time.Second, DefaultGameHost: "localhost", Certs: cm}, logger)
	builds := build.New(build.Options{Store: h.st, Docker: dc, InstanceID: dockertest.IntegrationInstanceID, RepoDir: repoDir, TempDir: tmpDir,
		Timeout: 20 * time.Minute, Logger: logger})
	runner := jobs.New(h.st, t.TempDir(), jobs.Options{Logger: logger, Pools: map[string]int{build.JobPool: build.MaxActiveBuilds}})
	srv, err := New(Config{Listen: "127.0.0.1:0", PublicHosts: []string{"example.com"}}, Deps{Store: h.st, Sessions: h.sessions, Hasher: h.hasher,
		Secrets: box, Jobs: runner, Logger: logger, Docker: dc, Servers: svc, Certs: cm, Builds: builds})
	if err != nil {
		t.Fatal(err)
	}
	srv.proj.repoDir, srv.proj.tmpDir = repoDir, tmpDir
	h.srv, h.runner = srv, runner

	var imageID string
	t.Cleanup(func() {
		_ = runner.Shutdown(context.Background())
		itestQuiet("rm", "--force", docker.ContainerName(serverName))
		if imageID != "" {
			itestQuiet("image", "rm", "--force", imageID)
		}
		if !networkExisted {
			itestQuiet("network", "rm", docker.NetworkName)
		}
	})
	tok := h.session(h.account("admin1", store.RoleAdmin, true))
	accepted := func(rr *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		if rr.Code != http.StatusAccepted {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		return rr
	}
	wait := func(job apitypes.Job, timeout time.Duration) store.Job {
		t.Helper()
		select {
		case <-runner.Done(job.ID):
		case <-time.After(timeout):
			t.Fatalf("job %d timeout", job.ID)
		}
		j, _ := h.st.GetJob(ctx, job.ID)
		if j.Status != store.JobSuccess {
			log, _ := os.ReadFile(runner.LogPath(j.ID))
			t.Fatalf("job %s %s %q\n%s\n%s", j.Kind, j.Status, j.Detail, log, logs.String())
		}
		return j
	}

	// Build.
	wait(decode[apitypes.CreateBuildResponse](t, accepted(h.do("POST", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), tok, nil))).Job, 20*time.Minute)
	list := decode[apitypes.BuildList](t, h.do("GET", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), tok, nil))
	if len(list.Items) != 1 || list.Items[0].Status != apitypes.BuildStatusSuccess {
		t.Fatalf("%+v", list)
	}
	b := list.Items[0]
	imageID = b.ImageID
	if b.EngineVersion != "v0.4.0-rc.1" || b.WebTransportPath != "/wt" || b.WebSocketPath != "/api/ws" ||
		b.ImageTag != fmt.Sprintf("golem-deploy/%s:%s-b%d", project, strings.Repeat("5e", 6), b.ID) {
		t.Fatalf("%+v", b)
	}
	labels := itestDocker(t, "", "image", "inspect", "--format", "{{json .Config.Labels}}", b.ImageID)
	var got map[string]string
	if err := json.Unmarshal([]byte(labels), &got); err != nil {
		t.Fatal(err, labels)
	}
	want := docker.BuildRef{ImageID: b.ImageID, BuildID: b.ID, ProjectID: p.ID, Commit: b.Commit, EngineVersion: b.EngineVersion}.Labels(dockertest.IntegrationInstanceID)
	for k, v := range got {
		if strings.HasPrefix(k, docker.LabelPrefix) && want[k] != v {
			t.Fatalf("image label %s=%q, want %q", k, v, want[k])
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("image label %s=%q, want %q", k, got[k], v)
		}
	}

	// Server from the build: health-gated start.
	rr := h.do("POST", "/api/v1/servers", tok, apitypes.CreateServerRequest{Name: serverName, BuildID: b.ID})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create server: %d %s", rr.Code, rr.Body.String())
	}
	sv := decode[apitypes.Server](t, rr)
	start := wait(decode[apitypes.Job](t, accepted(h.do("POST", fmt.Sprintf("/api/v1/servers/%d/start", sv.ID), tok, nil))), 2*time.Minute)
	startLog, _ := os.ReadFile(runner.LogPath(start.ID))
	if !strings.Contains(string(startLog), "game ready: engine v0.4.0-rc.1, transport webtransport") {
		t.Fatalf("start log:\n%s", startLog)
	}
	rt := decode[apitypes.ServerRuntime](t, h.do("GET", fmt.Sprintf("/api/v1/servers/%d/runtime", sv.ID), tok, nil))
	built := decode[apitypes.Build](t, h.do("GET", fmt.Sprintf("/api/v1/builds/%d", b.ID), tok, nil))
	if !rt.Ready || rt.GameName != "golem-deploy-fixture" || rt.EngineVersion != "v0.4.0-rc.1" || rt.Transport != "webtransport" ||
		rt.SchemaFingerprint == "" || rt.SchemaFingerprint != built.SchemaFingerprint || !rt.CertificateHashesPublished || rt.Entities < 1 {
		t.Fatalf("runtime %+v, build fingerprint %q", rt, built.SchemaFingerprint)
	}
	if len(built.ServerIDs) != 1 || built.ServerIDs[0] != sv.ID {
		t.Fatalf("%+v", built)
	}

	// The game's page and realtime config over HTTPS on the server's port,
	// with the installation's certificate.
	cert, err := cm.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fp := served(lo); fp != cert.Fingerprint {
		t.Fatalf("served certificate %q, installed %q", fp, cert.Fingerprint)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed, compared by fingerprint above
	get := func(path string) (int, string) {
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d%s", lo, path))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get("/"); code != http.StatusOK || !strings.Contains(body, "golem-deploy fixture client") {
		t.Fatalf("page: %d %q", code, body)
	}
	code, body := get("/api/realtime-config")
	var rc struct {
		Transport string `json:"transport"`
		URL       string `json:"url"`
		Hashes    []struct {
			Algorithm string `json:"algorithm"`
			Value     string `json:"value"`
		} `json:"serverCertificateHashes"`
		Fallback struct {
			Transport string `json:"transport"`
			URL       string `json:"url"`
		} `json:"fallback"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &rc) != nil {
		t.Fatalf("realtime config: %d %q", code, body)
	}
	if rc.Transport != "webtransport" || rc.URL != fmt.Sprintf("https://localhost:%d/wt", lo) || len(rc.Hashes) != 1 ||
		rc.Hashes[0].Algorithm != "sha-256" || rc.Hashes[0].Value != cert.Fingerprint ||
		rc.Fallback.Transport != "websocket" || rc.Fallback.URL != fmt.Sprintf("wss://localhost:%d/api/ws", lo) {
		t.Fatalf("realtime config %s", body)
	}

	// The build a server uses cannot be deleted.
	errBody(t, h.do("DELETE", fmt.Sprintf("/api/v1/builds/%d", b.ID), tok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	wait(decode[apitypes.Job](t, accepted(h.do("POST", fmt.Sprintf("/api/v1/servers/%d/stop", sv.ID), tok, nil))), time.Minute)
}

// copyFixture copies the build fixture into a fake checkout (with a .git
// directory, as a clone has).
func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err == nil {
		err = os.MkdirAll(filepath.Join(dst, ".git"), 0o700)
	}
	if err != nil {
		t.Fatal(err)
	}
}
