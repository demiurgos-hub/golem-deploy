package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

const envMarker = "ENV-MARKER-c0ffee-9d1e"

// dockerHarness is a harness whose server has the servers service (on a
// fake docker CLI) and the static store wired in.
type dockerHarness struct {
	*harness
	fake    *dockertest.Fake
	svc     *servers.Service
	static  *staticfiles.Store
	logs    *syncBuffer
	logDir  string
	bodies  *syncBuffer
	admin   store.Account
	dev     store.Account
	devTok  string
	adminSs string
	dc      *docker.Client
	// gameAdmin is the fake admin endpoint every built image publishes;
	// certs the game certificate manager.
	gameAdmin *gameadmintest.Server
	certs     *certs.Manager
	project   int64
}

// harnessCLIEnv is the daemon environment the fake docker CLI child gets.
var harnessCLIEnv = map[string]string{"PATH": "/usr/bin", "HOME": "/home/gd", "DOCKER_HOST": "unix:///run/docker.sock"}

func newDockerHarness(t *testing.T, mutate ...func(*servers.Config)) *dockerHarness {
	return newDockerHarnessWith(t, staticfiles.Options{}, mutate...)
}

func newDockerHarnessWith(t *testing.T, staticOpts staticfiles.Options, mutate ...func(*servers.Config)) *dockerHarness {
	t.Helper()
	h := newHarness(t)
	fake := dockertest.New()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dc := docker.New(docker.Options{Runner: fake, Logger: logger, InstanceID: dockertest.InstanceID, TempDir: t.TempDir(),
		LookupEnv: func(k string) (string, bool) { v, ok := harnessCLIEnv[k]; return v, ok }})
	box, err := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	cm, err := certs.Open(certs.Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: h.st})
	if err != nil {
		t.Fatal(err)
	}
	gameAdmin := gameadmintest.New(t)
	cfg := servers.Config{
		Ports:           ports.Range{Lo: 7000, Hi: 7004},
		StartTimeout:    2 * time.Second,
		ReadyPoll:       5 * time.Millisecond,
		StopTimeout:     time.Second,
		ProbePort:       func(string, int) error { return nil },
		DefaultGameHost: "play.example.com",
		Certs:           cm,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	svc := servers.New(h.st, box, dc, cfg, logger)
	if staticOpts.ReserveBytes == 0 {
		staticOpts.ReserveBytes = -1 // the test machine's free space is not under test
	}
	static, err := staticfiles.Open(filepath.Join(t.TempDir(), "static"), staticOpts)
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	runner := jobs.New(h.st, logDir, jobs.Options{Workers: 4, Pools: map[string]int{build.JobPool: build.MaxActiveBuilds}, Logger: logger})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Shutdown(ctx)
	})
	srv, err := New(Config{Listen: "127.0.0.1:0", Version: "test", PublicHosts: []string{"example.com"}}, Deps{
		Store: h.st, Sessions: h.sessions, Hasher: h.hasher, Secrets: box, Jobs: runner, Logger: logger,
		Docker: dc, Servers: svc, Static: static, Certs: cm, Now: h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.runner = srv, runner
	dh := &dockerHarness{harness: h, fake: fake, svc: svc, static: static, logs: logs, logDir: logDir, bodies: &syncBuffer{}, dc: dc,
		gameAdmin: gameAdmin, certs: cm}
	gameAdmin.Set(func(s *gameadmintest.Server) {
		s.ExpectToken = func() string { return fake.LatestEnv(servers.EnvAdminToken) }
	})
	dh.admin = h.account("admin1", store.RoleAdmin, true)
	dh.dev = h.account("dev1", store.RoleDeveloper, true)
	dh.devTok = h.apiToken(dh.dev, nil)
	dh.adminSs = h.session(dh.admin)
	return dh
}

// call sends a request and records the response body for leak checks.
func (d *dockerHarness) call(method, path, token string, body any) *httptest.ResponseRecorder {
	d.t.Helper()
	rr := d.do(method, path, token, body)
	_, _ = d.bodies.Write([]byte(rr.Body.String() + "\n"))
	return rr
}

// builtImage is a successful build and its image id.
type builtImage struct {
	ID    int64
	Image string
}

// testCommit is the commit of the build of image n.
func testCommit(n int) string { return fmt.Sprintf("%040x", n) }

// managedImage records a successful build of project "game" whose image is
// image n (labelled like the build pipeline labels it).
func (d *dockerHarness) managedImage(n int, tags ...string) builtImage {
	return d.buildImage(n, dockertest.Image{Tags: tags, Logs: []string{"hello from the game"}})
}

// buildImage is managedImage with image options (volumes, exit, logs).
func (d *dockerHarness) buildImage(n int, img dockertest.Image) builtImage {
	d.t.Helper()
	ctx := context.Background()
	if d.project == 0 {
		p, err := d.st.CreateProject(ctx, store.Project{Name: "game", RepoURL: "https://example.com/game.git"}, nil)
		if err != nil {
			d.t.Fatal(err)
		}
		d.project = p.ID
	}
	b, err := d.st.CreateBuild(ctx, store.NewBuild{ProjectID: d.project, Branch: "main"})
	if err != nil {
		d.t.Fatal(err)
	}
	img.ID = dockertest.ImageID(n)
	if err := d.st.MarkBuildRunning(ctx, b.ID, "main", testCommit(n)); err != nil {
		d.t.Fatal(err)
	}
	if err := d.st.FinishBuild(ctx, b.ID, store.BuildResult{Status: store.BuildStatusSuccess, ImageID: img.ID, EngineVersion: "v0.4.0-rc.1"}); err != nil {
		d.t.Fatal(err)
	}
	b, _ = d.st.GetBuild(ctx, b.ID)
	img.Labels = dockertest.BuildLabels(docker.BuildRef{ImageID: img.ID, BuildID: b.ID, ProjectID: b.ProjectID, Commit: b.Commit, EngineVersion: b.EngineVersion})
	if img.AssignPort == 0 {
		img.AssignPort = d.gameAdmin.Port
	}
	d.fake.AddImage(img)
	return builtImage{ID: b.ID, Image: img.ID}
}

func (d *dockerHarness) waitJob(job apitypes.Job) store.Job {
	d.t.Helper()
	select {
	case <-d.runner.Done(job.ID):
	case <-time.After(10 * time.Second):
		d.t.Fatalf("job %d did not finish", job.ID)
	}
	j, err := d.st.GetJob(context.Background(), job.ID)
	if err != nil {
		d.t.Fatal(err)
	}
	return j
}

func (d *dockerHarness) jobLog(id int64) string {
	b, _ := os.ReadFile(d.runner.LogPath(id))
	return string(b)
}

func (d *dockerHarness) createServer(token string, req apitypes.CreateServerRequest) apitypes.Server {
	d.t.Helper()
	rr := d.call("POST", "/api/v1/servers", token, req)
	if rr.Code != http.StatusCreated {
		d.t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.Server](d.t, rr)
}

func (d *dockerHarness) action(token string, id int64, action string) store.Job {
	d.t.Helper()
	rr := d.call("POST", fmt.Sprintf("/api/v1/servers/%d/%s", id, action), token, nil)
	if rr.Code != http.StatusAccepted {
		d.t.Fatalf("%s: %d %s", action, rr.Code, rr.Body.String())
	}
	return d.waitJob(decode[apitypes.Job](d.t, rr))
}

func (d *dockerHarness) getServer(token string, id int64) apitypes.Server {
	d.t.Helper()
	rr := d.call("GET", fmt.Sprintf("/api/v1/servers/%d", id), token, nil)
	if rr.Code != http.StatusOK {
		d.t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.Server](d.t, rr)
}

// assertNoMarker checks every response body, job detail, job log and
// daemon log line for the planted env value.
func (d *dockerHarness) assertNoMarker() {
	d.t.Helper()
	if strings.Contains(d.bodies.String(), envMarker) {
		d.t.Fatal("env value in an HTTP response")
	}
	if strings.Contains(d.logs.String(), envMarker) {
		d.t.Fatalf("env value in the daemon log")
	}
	list, err := d.st.ListJobs(context.Background(), store.JobFilter{Limit: 500})
	if err != nil {
		d.t.Fatal(err)
	}
	for _, j := range list {
		if strings.Contains(j.Detail, envMarker) || strings.Contains(d.jobLog(j.ID), envMarker) {
			d.t.Fatalf("env value in job %d (%s)", j.ID, j.Kind)
		}
	}
	for _, c := range d.fake.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), envMarker) {
			d.t.Fatalf("env value in docker argv %q", c.Args)
		}
		if strings.Contains(strings.Join(c.Env, " "), envMarker) {
			d.t.Fatalf("env value in the docker CLI environment %q", c.Env)
		}
	}
}

func TestServerLifecycle(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1, "game:v1")
	created := d.createServer(d.devTok, apitypes.CreateServerRequest{
		Name: "arena", BuildID: img.ID, Env: map[string]string{"GAME_SECRET": envMarker, "MODE": "ffa"},
	})
	if created.Image != img.Image || *created.BuildID != img.ID || created.Commit != testCommit(1) || created.PublicURL != "https://play.example.com:7000/" || created.Port != 7000 || created.State != apitypes.ServerStateMissing ||
		!slices.Equal(created.EnvKeys, []string{"GAME_SECRET", "MODE"}) || created.DesiredState != apitypes.DesiredStateStopped {
		t.Fatalf("created %+v", created)
	}

	j := d.action(d.devTok, created.ID, "start")
	if j.Status != store.JobSuccess {
		t.Fatalf("start job %s %q\n%s", j.Status, j.Detail, d.jobLog(j.ID))
	}
	runs := d.fake.CallsTo("create")
	if len(runs) != 1 {
		t.Fatalf("run calls %v", runs)
	}
	args := runs[0].Args
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name golem-deploy-arena", "--label dev.golemengine.deploy.managed=true",
		"--label dev.golemengine.deploy.server-id=" + strconv.FormatInt(created.ID, 10),
		"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--pids-limit 512",
		"--user 65532:65532", "--network golem-deploy", "--tmpfs /tmp:rw,noexec,nosuid,size=64m",
		"--publish 7000:7777/udp", "--publish 7000:7777/tcp", "--env-file ", "--init", "--no-healthcheck",
		"--label dev.golemengine.deploy.instance=" + dockertest.InstanceID,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run argv lacks %q: %s", want, joined)
		}
	}
	if args[len(args)-2] != "--" || args[len(args)-1] != img.Image {
		t.Fatalf("image not after --: %q", args)
	}
	if !strings.HasPrefix(runs[0].EnvFile, "GAME_SECRET="+envMarker+"\nGOLEM_ADDR=:7777\n") || !strings.HasSuffix(runs[0].EnvFile, "MODE=ffa\n") {
		t.Fatalf("env file %q", runs[0].EnvFile)
	}
	if ct, _ := d.fake.Container("golem-deploy-arena"); ct.Env["GAME_SECRET"] != envMarker || ct.RestartPolicy != docker.RestartPolicy {
		t.Fatalf("container %+v", ct)
	}
	if len(d.fake.CallsTo("network", "create")) != 1 {
		t.Fatal("network not ensured")
	}

	got := d.getServer(d.devTok, created.ID)
	if got.State != apitypes.ServerStateRunning || got.ContainerID == "" || got.StartedAt == nil || got.RestartRequired ||
		got.DesiredState != apitypes.DesiredStateRunning {
		t.Fatalf("after start %+v", got)
	}

	// One container query batch per list call, not one per row.
	d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "second", BuildID: img.ID})
	d.fake.ResetCalls()
	rr := d.call("GET", "/api/v1/servers", d.devTok, nil)
	list := decode[apitypes.ServerList](t, rr)
	if len(list.Items) != 2 || len(d.fake.CallsTo("ps")) != 1 || len(d.fake.CallsTo("container", "inspect")) != 1 {
		t.Fatalf("list %d items, calls %v", len(list.Items), d.fake.Calls())
	}

	// PATCH env: no implicit restart, restart_required reported.
	rr = d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", created.ID), d.devTok, apitypes.UpdateServerRequest{EnvSet: map[string]string{"MODE": "tdm"}})
	patched := decode[apitypes.Server](t, rr)
	if !patched.RestartRequired || patched.State != apitypes.ServerStateRunning || len(d.fake.CallsTo("create")) != 0 {
		t.Fatalf("patched %+v", patched)
	}
	rr = d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", created.ID), d.devTok, apitypes.UpdateServerRequest{EnvSet: map[string]string{"A": "1"}, EnvUnset: []string{"A"}})
	errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)

	// Logs and stats.
	rr = d.call("GET", fmt.Sprintf("/api/v1/servers/%d/logs?tail=10", created.ID), d.devTok, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "hello from the game") || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("logs %d %q", rr.Code, rr.Body.String())
	}
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d/logs?tail=0", created.ID), d.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d/logs?tail=5001", created.ID), d.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	rr = d.call("GET", fmt.Sprintf("/api/v1/servers/%d/stats", created.ID), d.devTok, nil)
	stats := decode[apitypes.ServerStats](t, rr)
	if stats.PIDs != 3 || stats.MemoryBytes != 12<<20 {
		t.Fatalf("stats %+v", stats)
	}

	// Restart recreates the container with the new definition.
	j = d.action(d.devTok, created.ID, "restart")
	if j.Status != store.JobSuccess {
		t.Fatalf("restart %s %q", j.Status, j.Detail)
	}
	if got := d.getServer(d.devTok, created.ID); got.RestartRequired || got.State != apitypes.ServerStateRunning {
		t.Fatalf("after restart %+v", got)
	}
	if n := len(d.fake.Containers()); n != 1 {
		t.Fatalf("%d containers after restart", n)
	}

	// Stop keeps the container (logs stay readable).
	j = d.action(d.devTok, created.ID, "stop")
	if j.Status != store.JobSuccess {
		t.Fatalf("stop %s %q", j.Status, j.Detail)
	}
	got = d.getServer(d.devTok, created.ID)
	if got.State != apitypes.ServerStateExited || got.ExitCode == nil || got.DesiredState != apitypes.DesiredStateStopped {
		t.Fatalf("after stop %+v", got)
	}
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d/stats", created.ID), d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)

	// Delete removes container and row and frees the port.
	rr = d.call("DELETE", fmt.Sprintf("/api/v1/servers/%d", created.ID), d.devTok, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := d.fake.Container("golem-deploy-arena"); ok {
		t.Fatal("container left after delete")
	}
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d", created.ID), d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	reused := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "third", BuildID: img.ID})
	if reused.Port != 7000 {
		t.Fatalf("port not released: %d", reused.Port)
	}
	d.assertNoMarker()
}

func TestServerValidation(t *testing.T) {
	d := newDockerHarness(t)
	gameV1 := d.managedImage(1, "game:v1")
	failed, _ := d.st.CreateBuild(context.Background(), store.NewBuild{ProjectID: d.project})
	bad := []struct {
		req   apitypes.CreateServerRequest
		field string
	}{
		{apitypes.CreateServerRequest{Name: "Bad_Name", BuildID: gameV1.ID}, "name"},
		{apitypes.CreateServerRequest{Name: "a"}, "build_id"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: 999}, "build_id"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: failed.ID}, "build_id"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, AllowedOrigins: []string{"https://ok.example", "https://x.example/path"}}, "allowed_origins[1]"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Port: ptr(6999)}, "port"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Port: ptr(22)}, "port"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Env: map[string]string{"GOLEM_ENGINE": envMarker}}, "env"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Env: map[string]string{"1X": envMarker}}, "env"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Env: map[string]string{"X": "a\x00" + envMarker}}, "env.X"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Env: map[string]string{"MOTD": "a\n" + envMarker}}, "env.MOTD"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Env: map[string]string{"MOTD": envMarker + "\r"}}, "env.MOTD"},
		{apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, DesiredState: ptr(apitypes.DesiredState("paused"))}, "desired_state"},
	}
	for _, c := range bad {
		e := errBody(t, d.call("POST", "/api/v1/servers", d.devTok, c.req), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
		if e.Field != c.field {
			t.Errorf("%+v: field %q, want %q", c.req, e.Field, c.field)
		}
	}
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID, Port: ptr(7003)})
	errBody(t, d.call("POST", "/api/v1/servers", d.devTok, apitypes.CreateServerRequest{Name: "a", BuildID: gameV1.ID}), http.StatusConflict, apitypes.ErrorCodeNameTaken)
	errBody(t, d.call("POST", "/api/v1/servers", d.devTok, apitypes.CreateServerRequest{Name: "b", BuildID: gameV1.ID, Port: ptr(7003)}), http.StatusConflict, apitypes.ErrorCodePortInUse)
	e := errBody(t, d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, apitypes.UpdateServerRequest{BuildID: &failed.ID}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "build_id" {
		t.Fatalf("field %q", e.Field)
	}
	// The old wire fields are unknown now.
	errBody(t, d.call("POST", "/api/v1/servers", d.devTok, `{"name":"z","image":"game:v1"}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	// Allowed origins are normalised and replaced as a whole.
	got0 := decode[apitypes.Server](t, d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok,
		apitypes.UpdateServerRequest{AllowedOrigins: &[]string{"HTTPS://Web.Example.com:443", "http://localhost:5173"}}))
	if !slices.Equal(got0.AllowedOrigins, []string{"https://web.example.com", "http://localhost:5173"}) {
		t.Fatalf("%v", got0.AllowedOrigins)
	}
	// Auto-assignment fills the lowest free ports, then reports exhaustion.
	var got []int
	for _, n := range []string{"b", "c", "d", "e"} {
		got = append(got, d.createServer(d.devTok, apitypes.CreateServerRequest{Name: n, BuildID: gameV1.ID}).Port)
	}
	if !slices.Equal(got, []int{7000, 7001, 7002, 7004}) {
		t.Fatalf("ports %v", got)
	}
	errBody(t, d.call("POST", "/api/v1/servers", d.devTok, apitypes.CreateServerRequest{Name: "f", BuildID: gameV1.ID}), http.StatusConflict, apitypes.ErrorCodePortInUse)
	d.assertNoMarker()
}

// Two services (as two processes would) racing on the same database
// recover from UNIQUE violations on the port column.
func TestAutoPortRace(t *testing.T) {
	d := newDockerHarness(t, func(c *servers.Config) { c.Ports = ports.Range{Lo: 7000, Hi: 7040} })
	img := d.managedImage(1)
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	other := servers.New(d.st, box, docker.New(docker.Options{Runner: d.fake}), servers.Config{Ports: ports.Range{Lo: 7000, Hi: 7040}}, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for j, svc := range []*servers.Service{d.svc, other} {
			wg.Add(1)
			go func(name string, svc *servers.Service) {
				defer wg.Done()
				_, err := svc.Create(context.Background(), apitypes.CreateServerRequest{Name: name, BuildID: img.ID})
				errs <- err
			}(fmt.Sprintf("s%d-%d", i, j), svc)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("create failed: %v", err)
		}
	}
	rows, _ := d.st.ListServers(context.Background())
	seen := map[int]bool{}
	for _, r := range rows {
		if seen[*r.Port] {
			t.Fatalf("port %d assigned twice", *r.Port)
		}
		seen[*r.Port] = true
	}
	if len(rows) != 40 {
		t.Fatalf("%d rows", len(rows))
	}
}

func TestDockerDown(t *testing.T) {
	d := newDockerHarness(t)
	gameV1 := d.managedImage(1, "game:v1")
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: gameV1.ID})
	if j := d.action(d.devTok, s.ID, "start"); j.Status != store.JobSuccess {
		t.Fatalf("start %q", j.Detail)
	}
	// A server that never had a container is "missing" even without Docker.
	never := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "never", BuildID: gameV1.ID})
	d.fake.SetDown(true)
	list := decode[apitypes.ServerList](t, d.call("GET", "/api/v1/servers", d.devTok, nil))
	if len(list.Items) != 2 || list.Items[0].State != apitypes.ServerStateUnknown || list.Items[1].State != apitypes.ServerStateMissing {
		t.Fatalf("list %+v", list)
	}
	if got := d.getServer(d.devTok, s.ID); got.State != apitypes.ServerStateUnknown {
		t.Fatalf("get %+v", got)
	}
	if got := d.getServer(d.devTok, never.ID); got.State != apitypes.ServerStateMissing {
		t.Fatalf("get %+v", got)
	}
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || j.Detail != "Docker is unreachable" {
		t.Fatalf("start %s %q", j.Status, j.Detail)
	}
	errBody(t, d.call("DELETE", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	if _, err := d.st.GetServer(context.Background(), s.ID); err != nil {
		t.Fatal("row deleted although the container could not be removed")
	}
	errBody(t, d.call("GET", "/api/v1/images", d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d/stats", s.ID), d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	// Creating a server needs no Docker (the image is verified at start).
	if rr := d.call("POST", "/api/v1/servers", d.devTok, apitypes.CreateServerRequest{Name: "b", BuildID: gameV1.ID}); rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	errBody(t, d.call("GET", fmt.Sprintf("/api/v1/servers/%d/runtime", s.ID), d.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	if st := decode[apitypes.Status](t, d.call("GET", "/api/v1/status", d.devTok, nil)); st.DockerReachable {
		t.Fatal("status reports docker reachable")
	}
}

func TestStartFailsWhenContainerExits(t *testing.T) {
	d := newDockerHarness(t)
	code := 3
	id := d.buildImage(5, dockertest.Image{ExitOnRun: &code, Logs: []string{"boot", "fatal: bad config, secret=" + envMarker}})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "crashy", BuildID: id.ID, Env: map[string]string{"TOKEN": envMarker}})
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || !strings.Contains(j.Detail, "exited before the game became ready (exit code 3)") {
		t.Fatalf("job %s %q", j.Status, j.Detail)
	}
	log := d.jobLog(j.ID)
	if !strings.Contains(log, "fatal: bad config, secret=[redacted]") || strings.Contains(j.Detail, "fatal") {
		t.Fatalf("job log %q / detail %q", log, j.Detail)
	}
	d.assertNoMarker()
}

func TestStartRefusesForeignContainer(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	foreign := d.fake.AddContainer(dockertest.Container{Name: "golem-deploy-arena", Image: dockertest.ImageID(9)})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || !strings.Contains(j.Detail, "this installation does not manage it") || !strings.Contains(j.Detail, "docker rm -f golem-deploy-arena") {
		t.Fatalf("job %s %q", j.Status, j.Detail)
	}
	if c, ok := d.fake.Container(foreign); !ok || c.State != "running" {
		t.Fatal("foreign container touched")
	}
	for _, c := range d.fake.Calls() {
		if slices.Contains(c.Args, foreign) && c.Args[0] != "container" {
			t.Fatalf("call on foreign container: %q", c.Args)
		}
	}
	// A foreign network named golem-deploy is refused too.
	d2 := newDockerHarness(t)
	img2 := d2.managedImage(1)
	d2.fake.AddNetwork(docker.NetworkName, nil, nil)
	s2 := d2.createServer(d2.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img2.ID})
	if j := d2.action(d2.devTok, s2.ID, "start"); j.Status != store.JobError || !strings.Contains(j.Detail, "network") {
		t.Fatalf("job %s %q", j.Status, j.Detail)
	}
}

func TestStartFailsWhenPortBusy(t *testing.T) {
	d := newDockerHarness(t, func(c *servers.Config) {
		c.ProbePort = func(string, int) error { return ports.ErrBusy }
	})
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || j.Detail != "port 7000 is in use by another process on the host" {
		t.Fatalf("job %s %q", j.Status, j.Detail)
	}
	if len(d.fake.CallsTo("create")) != 0 {
		t.Fatal("ran despite a busy port")
	}
}

func TestExportImport(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1, "game:v1")
	d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID, Env: map[string]string{"K": "exported-value"}})

	errBody(t, d.call("GET", "/api/v1/servers/export", d.devTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	doc := decode[apitypes.ServerExport](t, d.do("GET", "/api/v1/servers/export", d.adminSs, nil))
	if doc.FormatVersion != 2 || len(doc.Servers) != 1 || doc.Servers[0].Env["K"] != "exported-value" || doc.Servers[0].Project != "game" ||
		doc.Servers[0].Commit != testCommit(1) {
		t.Fatalf("export %+v", doc)
	}

	count := func() int { n, _ := d.st.CountServers(context.Background()); return n }
	cases := []struct {
		doc    apitypes.ServerExport
		status int
		code   apitypes.ErrorCode
	}{
		{apitypes.ServerExport{FormatVersion: 1, Servers: []apitypes.ServerSpec{{Name: "x", Project: "game"}}}, 400, apitypes.ErrorCodeValidationFailed},
		{apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "ok1", Project: "game"}, {Name: "x", Project: "game", Commit: testCommit(9)}}}, 400, apitypes.ErrorCodeValidationFailed},
		{apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "ok1", Project: "game"}, {Name: "bad", Project: "game", Env: map[string]string{"GOLEM_X": envMarker}}}}, 400, apitypes.ErrorCodeValidationFailed},
		{apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "ok1", Project: "game"}, {Name: "arena", Project: "game"}}}, 409, apitypes.ErrorCodeNameTaken},
		{apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "ok1", Project: "game"}, {Name: "ok1", Project: "game"}}}, 400, apitypes.ErrorCodeValidationFailed},
		{apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "ok1", Project: "game"}, {Name: "ok2", Project: "game", Port: ptr(7000)}}}, 409, apitypes.ErrorCodePortInUse},
		{apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{{Name: "ok1", Project: "game"}, {Name: "ok2", Project: "nope"}}}, 400, apitypes.ErrorCodeValidationFailed},
	}
	for i, c := range cases {
		errBody(t, d.do("POST", "/api/v1/servers/import", d.adminSs, c.doc), c.status, c.code)
		if count() != 1 {
			t.Fatalf("case %d: partial import", i)
		}
	}
	rr := d.do("POST", "/api/v1/servers/import", d.adminSs, apitypes.ServerExport{FormatVersion: 2, Servers: []apitypes.ServerSpec{
		{Name: "imp1", Project: "game", Env: map[string]string{"S": envMarker}, DesiredState: apitypes.DesiredStateRunning},
		{Name: "imp2", Project: "game", Port: ptr(7001)},
	}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("import %d %s", rr.Code, rr.Body.String())
	}
	_, _ = d.bodies.Write(rr.Body.Bytes())
	res := decode[apitypes.ServerImportResult](t, rr)
	if len(res.Created) != 2 || res.Created[0].Port != 7002 || res.Created[1].Port != 7001 || *res.Created[0].BuildID != img.ID ||
		res.Created[0].DesiredState != apitypes.DesiredStateStopped || res.Created[0].EnvKeys[0] != "S" {
		t.Fatalf("import result %+v", res)
	}
	if len(d.fake.CallsTo("create")) != 0 {
		t.Fatal("import started a server")
	}
	d.assertNoMarker()
}

func TestServerLogFollow(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	if j := d.action(d.devTok, s.ID, "start"); j.Status != store.JobSuccess {
		t.Fatalf("start %q", j.Detail)
	}
	path := fmt.Sprintf("/api/v1/servers/%d/logs?follow=1", s.ID)
	type result struct{ rr *httptest.ResponseRecorder }
	results := make(chan result, 8)
	for i := 0; i < logStreamsPerAccount; i++ {
		go func() { results <- result{d.do("GET", path, d.devTok, nil)} }()
	}
	waitFor(t, func() bool {
		return d.srv.logStreams.Count(strconv.FormatInt(d.dev.ID, 10)) == logStreamsPerAccount
	})
	// The per-account cap applies; another account is unaffected.
	errBody(t, d.do("GET", path, d.devTok, nil), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	go func() { results <- result{d.do("GET", path, d.adminSs, nil)} }()
	waitFor(t, func() bool {
		return d.srv.logStreams.Count(strconv.FormatInt(d.admin.ID, 10)) == 1
	})
	// Deleting the server removes the container, which ends every stream.
	if rr := d.do("DELETE", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	for i := 0; i < logStreamsPerAccount+1; i++ {
		select {
		case r := <-results:
			if r.rr.Code != http.StatusOK || !strings.Contains(r.rr.Body.String(), "hello from the game") {
				t.Fatalf("follow response %d %q", r.rr.Code, r.rr.Body.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("follow stream did not end")
		}
	}
	if left := d.srv.logStreams.Len(); left != 0 {
		t.Fatalf("follow counters not released: %d", left)
	}
}

func TestServerLogFollowEndsOnClientDisconnect(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	d.action(d.devTok, s.ID, "start")
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/servers/%d/logs?follow=1", s.ID), nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+d.devTok)
	done := make(chan struct{})
	go func() {
		d.srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end on disconnect")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestImagesAndVolumesAPI(t *testing.T) {
	d := newDockerHarness(t)
	used := d.managedImage(1, "game:v1")
	built := d.managedImage(2, "game:old")
	free := dockertest.ImageID(4) // ours, but no build records it
	d.fake.AddImage(dockertest.Image{ID: free, Tags: []string{"game:dangling"}, Labels: dockertest.OurLabels()})
	unmanaged := dockertest.ImageID(3)
	d.fake.AddImage(dockertest.Image{ID: unmanaged, Tags: []string{"postgres:16"}})
	d.fake.AddImage(dockertest.Image{ID: dockertest.ImageID(5), Tags: []string{"hand:1"}, Labels: dockertest.ManagedLabels()})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: used.ID})

	list := decode[apitypes.ImageList](t, d.call("GET", "/api/v1/images", d.devTok, nil))
	if len(list.Items) != 3 {
		t.Fatalf("images %+v", list)
	}
	for _, img := range list.Items {
		switch img.ID {
		case used.Image:
			if !img.InUse || !slices.Equal(img.ServerIDs, []int64{s.ID}) || !slices.Equal(img.BuildIDs, []int64{used.ID}) {
				t.Fatalf("%+v", img)
			}
		case built.Image:
			if !img.InUse || len(img.ServerIDs) != 0 || !slices.Equal(img.BuildIDs, []int64{built.ID}) {
				t.Fatalf("%+v", img)
			}
		case free:
			if img.InUse || len(img.ServerIDs) != 0 || len(img.BuildIDs) != 0 {
				t.Fatalf("%+v", img)
			}
		default:
			t.Fatalf("unexpected image %s", img.ID)
		}
	}
	errBody(t, d.call("DELETE", "/api/v1/images/"+used.Image, d.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, d.call("DELETE", "/api/v1/images/"+built.Image, d.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, d.call("DELETE", "/api/v1/images/"+dockertest.ImageID(5), d.adminSs, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, d.call("DELETE", "/api/v1/images/"+unmanaged, d.adminSs, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, d.call("DELETE", "/api/v1/images/postgres:16", d.adminSs, nil), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if rr := d.call("DELETE", "/api/v1/images/"+free, d.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if !d.fake.HasImage(unmanaged) || !d.fake.HasImage(used.Image) || d.fake.HasImage(free) {
		t.Fatal("wrong images removed")
	}

	d.fake.AddVolume(dockertest.Volume{Name: "golem-deploy-arena-data", Labels: dockertest.OurLabels(docker.LabelServerID, strconv.FormatInt(s.ID, 10), docker.LabelServer, "arena")})
	d.fake.AddVolume(dockertest.Volume{Name: "golem-deploy-gone-data", Labels: dockertest.OurLabels(docker.LabelServerID, "999", docker.LabelServer, "gone")})
	d.fake.AddVolume(dockertest.Volume{Name: "pixel_pgdata"})
	d.fake.AddContainer(dockertest.Container{Name: "x", Mounts: []string{"golem-deploy-arena-data"}})
	vols := decode[apitypes.VolumeList](t, d.call("GET", "/api/v1/volumes", d.devTok, nil))
	if len(vols.Items) != 2 || vols.Items[0].ServerID == nil || *vols.Items[0].ServerID != s.ID || !vols.Items[0].InUse ||
		vols.Items[1].ServerID != nil || vols.Items[1].ServerName != "gone" {
		t.Fatalf("volumes %+v", vols)
	}
	errBody(t, d.call("DELETE", "/api/v1/volumes/pixel_pgdata", d.adminSs, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, d.call("DELETE", "/api/v1/volumes/golem-deploy-arena-data", d.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, d.call("DELETE", "/api/v1/volumes/-rf", d.adminSs, nil), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if rr := d.call("DELETE", "/api/v1/volumes/golem-deploy-gone-data", d.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete volume %d %s", rr.Code, rr.Body.String())
	}
	if !d.fake.HasVolume("pixel_pgdata") || d.fake.HasVolume("golem-deploy-gone-data") {
		t.Fatal("wrong volumes removed")
	}
	for _, c := range d.fake.CallsTo("volume", "rm") {
		if c.Args[len(c.Args)-1] != "golem-deploy-gone-data" {
			t.Fatalf("rm on %q", c.Args)
		}
	}
}

func multipartBody(t *testing.T, files map[string]string, order ...string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range order {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="files"; filename="%s"`, name)}
		h["Content-Type"] = []string{"application/octet-stream"}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, files[name])
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (d *dockerHarness) upload(query string, files map[string]string, order ...string) *httptest.ResponseRecorder {
	body, ct := multipartBody(d.t, files, order...)
	req := httptest.NewRequest("POST", "/api/v1/static"+query, body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+d.devTok)
	rr := httptest.NewRecorder()
	d.srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestStaticAPI(t *testing.T) {
	d := newDockerHarness(t)
	rr := d.upload("", map[string]string{"addon.zip": "PK\x03\x04data", "readme.txt": "hi"}, "addon.zip", "readme.txt")
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload %d %s", rr.Code, rr.Body.String())
	}
	got := decode[apitypes.StaticFileList](t, rr)
	if len(got.Items) != 2 || !got.Items[0].IsZip || got.Items[1].IsZip || got.Items[1].SizeBytes != 2 {
		t.Fatalf("%+v", got)
	}
	errBody(t, d.upload("", map[string]string{"readme.txt": "new"}, "readme.txt"), http.StatusConflict, apitypes.ErrorCodeConflict)
	if rr := d.upload("?replace=1", map[string]string{"readme.txt": "new!"}, "readme.txt"); rr.Code != http.StatusCreated {
		t.Fatalf("replace %d %s", rr.Code, rr.Body.String())
	}
	for _, bad := range []string{"../evil.zip", `..\evil.zip`, "sub/x.zip", ".hidden", "CON.zip", "x."} {
		rr := d.upload("", map[string]string{bad: "x"}, bad)
		errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(d.static.Dir()), "evil.zip")); err == nil {
		t.Fatal("traversal upload written")
	}
	errBody(t, d.upload("", map[string]string{"a.txt": "1"}, "a.txt", "a.txt"), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	errBody(t, d.do("POST", "/api/v1/static", d.devTok, `{}`), http.StatusUnsupportedMediaType, apitypes.ErrorCodeUnsupportedMediaType)

	list := decode[apitypes.StaticFileList](t, d.do("GET", "/api/v1/static", d.devTok, nil))
	if len(list.Items) != 2 {
		t.Fatalf("%+v", list)
	}
	rr = d.do("GET", "/api/v1/static/readme.txt", d.devTok, nil)
	if rr.Code != http.StatusOK || rr.Body.String() != "new!" || rr.Header().Get("Content-Type") != "application/octet-stream" ||
		rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Content-Disposition") != `attachment; filename=readme.txt` {
		t.Fatalf("download %d %q %v", rr.Code, rr.Body.String(), rr.Header())
	}
	errBody(t, d.do("GET", "/api/v1/static/nope.zip", d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, d.do("GET", "/api/v1/static/..%2Fgolem-deploy.db", d.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	if rr := d.do("DELETE", "/api/v1/static/readme.txt", d.devTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d", rr.Code)
	}
	errBody(t, d.do("DELETE", "/api/v1/static/readme.txt", d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

// Every server/image/volume/static route works for the identities its
// access level allows when the services are wired (no 5xx from handlers).
func TestServiceRoutesDoNotFail(t *testing.T) {
	d := newDockerHarness(t)
	fill := strings.NewReplacer("{id}", "999999", "{name}", "nope")
	for _, rt := range d.srv.routes() {
		p := rt.path
		if !strings.HasPrefix(p, "/servers") && !strings.HasPrefix(p, "/images") && !strings.HasPrefix(p, "/volumes") && !strings.HasPrefix(p, "/static") &&
			!strings.HasPrefix(p, "/certificates") {
			continue
		}
		rr := d.do(rt.method, "/api/v1"+fill.Replace(p), d.adminSs, "{}")
		if rr.Code >= 500 {
			t.Errorf("%s %s: %d %s", rt.method, p, rr.Code, rr.Body.String())
		}
	}
}
