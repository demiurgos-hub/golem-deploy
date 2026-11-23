package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

var runtimeGOOS = runtime.GOOS

// Regression tests for the security review of the Docker runner, the
// servers API and the static file store (findings D1-D14).

func (d *dockerHarness) start(id int64) store.Job {
	d.t.Helper()
	j := d.action(d.devTok, id, "start")
	if j.Status != store.JobSuccess {
		d.t.Fatalf("start: %s %q\n%s", j.Status, j.Detail, d.jobLog(j.ID))
	}
	return j
}

func (d *dockerHarness) row(id int64) store.Server {
	d.t.Helper()
	sv, err := d.st.GetServer(context.Background(), id)
	if err != nil {
		d.t.Fatal(err)
	}
	return sv
}

// D1: env values set by a developer never reach the docker CLI process
// (environment or argv), whatever the variable names; they reach the
// container intact.
func TestD1_ServerEnvNeverReachesCLIProcess(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	env := map[string]string{
		"LD_PRELOAD":  "/srv/data/static/evil-" + envMarker + ".so",
		"GODEBUG":     "x509sha1=1," + envMarker,
		"DOCKER_HOST": "tcp://attacker.example:2375/" + envMarker,
		"PATH":        "/tmp/" + envMarker,
		"MOTD":        `  "quoted" #hash $DOLLAR \back ünï ` + envMarker,
	}
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img, Env: env})
	d.start(s.ID)
	want := docker.BaseEnv(func(k string) (string, bool) { v, ok := harnessCLIEnv[k]; return v, ok }, runtimeGOOS)
	for _, c := range d.fake.Calls() {
		if !slices.Equal(c.Env, want) {
			t.Fatalf("%q: child env %q, want the fixed allowlist %q", c.Args, c.Env, want)
		}
		for _, a := range c.Args {
			if strings.Contains(a, envMarker) {
				t.Fatalf("value in argv %q", c.Args)
			}
		}
	}
	ct, _ := d.fake.Container("golem-deploy-arena")
	for k, v := range env {
		if ct.Env[k] != v {
			t.Fatalf("container %s=%q, want %q", k, ct.Env[k], v)
		}
	}
	d.assertNoMarker()
}

// D1: values must be single-line; GOLEM_* is reserved. Errors name the key
// (never the value) on create, patch and import.
func TestD1_EnvValueRules(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	e := errBody(t, d.call("POST", "/api/v1/servers", d.devTok, apitypes.CreateServerRequest{Name: "a", Image: img,
		Env: map[string]string{"MOTD": "line1\nINJECTED=" + envMarker}}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "env.MOTD" {
		t.Fatalf("field %q", e.Field)
	}
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "a", Image: img, Env: map[string]string{"PATH": "/opt/game/bin", "LD_LIBRARY_PATH": "/opt/game/lib"}})
	e = errBody(t, d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, apitypes.UpdateServerRequest{EnvSet: map[string]string{"X": envMarker + "\r"}}),
		http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "env_set.X" {
		t.Fatalf("field %q", e.Field)
	}
	e = errBody(t, d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, apitypes.UpdateServerRequest{EnvSet: map[string]string{"GOLEM_ENGINE_TOKEN": envMarker}}),
		http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "env_set" || !strings.Contains(e.Message, "reserved") {
		t.Fatalf("%+v", e)
	}
	rr := d.call("POST", "/api/v1/servers/import", d.adminSs, apitypes.ServerExport{FormatVersion: 1, Servers: []apitypes.ServerSpec{
		{Name: "imp", Image: img, Env: map[string]string{"K": "a\x00" + envMarker}}}})
	if e := errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "servers[0].env.K" {
		t.Fatalf("field %q", e.Field)
	}
	d.assertNoMarker()
}

// D2: image volumes become tmpfs mounts; images with too many are refused.
func TestD2_ImageVolumesBecomeTmpfs(t *testing.T) {
	d := newDockerHarness(t)
	id := dockertest.ImageID(7)
	d.fake.AddImage(dockertest.Image{ID: id, Labels: dockertest.ManagedLabels(), Volumes: []string{"/data", "/var/lib/game"}})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "vol", Image: id})
	j := d.start(s.ID)
	ct, _ := d.fake.Container("golem-deploy-vol")
	if !slices.Equal(ct.Tmpfs, []string{"/tmp:rw,noexec,nosuid,size=64m", "/data:rw,noexec,nosuid,size=64m", "/var/lib/game:rw,noexec,nosuid,size=64m"}) {
		t.Fatalf("tmpfs %q", ct.Tmpfs)
	}
	if !strings.Contains(d.jobLog(j.ID), "image volumes /data, /var/lib/game get size-limited tmpfs mounts") {
		t.Fatal(d.jobLog(j.ID))
	}
	d.action(d.devTok, s.ID, "restart")
	for _, rm := range d.fake.CallsTo("rm") {
		if !slices.Contains(rm.Args, "--volumes") {
			t.Fatalf("rm without --volumes: %q", rm.Args)
		}
	}
	many := dockertest.ImageID(8)
	d.fake.AddImage(dockertest.Image{ID: many, Labels: dockertest.ManagedLabels(), Volumes: []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h", "/i"}})
	e := errBody(t, d.call("POST", "/api/v1/servers", d.devTok, apitypes.CreateServerRequest{Name: "many", Image: many}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "image" {
		t.Fatalf("%+v", e)
	}
}

// D3: the reviewer's impostor: a container carrying the managed and
// server-id labels (and even our instance id) that is not the recorded
// container is not reported, read, stopped or removed.
func TestD3_ImpostorContainerIsIgnored(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img})
	impostor := d.fake.AddContainer(dockertest.Container{Name: "impostor", Image: img,
		Labels: dockertest.OurLabels(docker.LabelServerID, strconv.FormatInt(s.ID, 10), docker.LabelServer, "arena"),
		Logs:   []string{"IMPOSTOR OUTPUT"}})
	base := fmt.Sprintf("/api/v1/servers/%d", s.ID)

	// Before the server ever ran: nothing is reported or reachable.
	if got := d.getServer(d.devTok, s.ID); got.State != apitypes.ServerStateMissing || got.ContainerID != "" {
		t.Fatalf("before start %+v", got)
	}
	list := decode[apitypes.ServerList](t, d.call("GET", "/api/v1/servers", d.devTok, nil))
	if list.Items[0].State != apitypes.ServerStateMissing {
		t.Fatalf("list %+v", list.Items[0])
	}
	errBody(t, d.call("GET", base+"/logs", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, d.call("GET", base+"/stats", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if j := d.action(d.devTok, s.ID, "stop"); j.Status != store.JobSuccess || !strings.Contains(d.jobLog(j.ID), "no running container") {
		t.Fatalf("stop %s %q", j.Status, d.jobLog(j.ID))
	}

	// After start: the recorded container is the server's container.
	d.start(s.ID)
	real := d.row(s.ID).ContainerID
	if real == "" || real == impostor {
		t.Fatalf("recorded %q", real)
	}
	if got := d.getServer(d.devTok, s.ID); got.ContainerID != real || got.State != apitypes.ServerStateRunning {
		t.Fatalf("after start %+v", got)
	}
	rr := d.call("GET", base+"/logs", d.devTok, nil)
	if strings.Contains(rr.Body.String(), "IMPOSTOR") || !strings.Contains(rr.Body.String(), "hello from the game") {
		t.Fatalf("logs %q", rr.Body.String())
	}
	d.action(d.devTok, s.ID, "stop")
	if got := d.getServer(d.devTok, s.ID); got.State != apitypes.ServerStateExited || got.ContainerID != real {
		t.Fatalf("after stop %+v", got)
	}
	if rr := d.call("DELETE", base, d.devTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d", rr.Code)
	}
	if ct, ok := d.fake.Container(impostor); !ok || ct.State != "running" {
		t.Fatal("impostor stopped or removed")
	}
	for _, c := range d.fake.Calls() {
		// Listing and inspecting labelled containers is how it is found
		// and ignored; nothing else may name it.
		if slices.Contains(c.Args, impostor) && !(c.Args[0] == "container" && c.Args[1] == "inspect") {
			t.Fatalf("call on the impostor: %q", c.Args)
		}
	}
	if n := strings.Count(d.logs.String(), "not the container recorded for any server"); n != 1 {
		t.Fatalf("impostor logged %d times:\n%s", n, d.logs.String())
	}
}

// D3: rows from before container ids were recorded have no container; an
// old container with the derived name blocks the start with instructions.
func TestD3_UpgradeWithoutRecordedContainer(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img})
	legacy := d.fake.AddContainer(dockertest.Container{Name: "golem-deploy-arena", Image: img,
		Labels: dockertest.ManagedLabels(docker.LabelServerID, strconv.FormatInt(s.ID, 10), docker.LabelServer, "arena")})
	if got := d.getServer(d.devTok, s.ID); got.State != apitypes.ServerStateMissing {
		t.Fatalf("%+v", got)
	}
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || !strings.Contains(j.Detail, "docker rm -f golem-deploy-arena") {
		t.Fatalf("%s %q", j.Status, j.Detail)
	}
	if ct, ok := d.fake.Container(legacy); !ok || ct.State != "running" {
		t.Fatal("legacy container touched")
	}
	// After the operator removes it, the start succeeds.
	d.fake.RemoveContainer(legacy)
	d.start(s.ID)
}

// D3: the instance id labels every object and is never exposed.
func TestD3_InstanceIDIsNotExposed(t *testing.T) {
	d := newDockerHarness(t)
	inst, err := d.st.InstanceID(context.Background())
	if err != nil || !docker.ValidInstanceID(inst) {
		t.Fatalf("%q %v", inst, err)
	}
	if again, _ := d.st.InstanceID(context.Background()); again != inst {
		t.Fatal("instance id not stable")
	}
	for _, p := range []string{"/api/v1/settings", "/api/v1/status"} {
		if rr := d.call("GET", p, d.adminSs, nil); strings.Contains(rr.Body.String(), inst) {
			t.Fatalf("%s exposes the instance id", p)
		}
	}
}

// D4: memory limits come without extra swap.
func TestD4_MemoryLimitWithoutSwap(t *testing.T) {
	d := newDockerHarness(t, func(c *servers.Config) { c.MemoryMB, c.CPUs = 2048, 2 })
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img})
	d.start(s.ID)
	run := strings.Join(d.fake.CallsTo("run")[0].Args, " ")
	if !strings.Contains(run, "--memory 2048m --memory-swap 2048m") || !strings.Contains(run, "--cpus 2") {
		t.Fatal(run)
	}
}

// D5: a start reports when the network still allows traffic between
// servers.
func TestD5_StartWarnsWhenNetworkNotIsolated(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	d.fake.AddNetwork(docker.NetworkName, dockertest.OurLabels(), nil)
	d.fake.AddContainer(dockertest.Container{Name: "other", Network: docker.NetworkName, Labels: dockertest.OurLabels()})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img})
	j := d.start(s.ID)
	if !strings.Contains(d.jobLog(j.ID), "warning: the golem-deploy network still allows traffic between servers") {
		t.Fatal(d.jobLog(j.ID))
	}
}

// D6: the static store enforces its quota on received bytes, a file count
// cap, a free-space reserve and a per-account upload concurrency cap, and
// reports its usage.
func TestD6_StaticQuotaAndConcurrency(t *testing.T) {
	free := int64(10 << 30)
	var freeMu sync.Mutex
	d := newDockerHarnessWith(t, staticfiles.Options{QuotaBytes: 100, MaxFiles: 3, ReserveBytes: 1 << 30,
		FreeSpace: func(string) (int64, error) { freeMu.Lock(); defer freeMu.Unlock(); return free, nil }})
	if rr := d.upload("", map[string]string{"a.bin": strings.Repeat("a", 60)}, "a.bin"); rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	errBody(t, d.upload("", map[string]string{"b.bin": strings.Repeat("b", 41)}, "b.bin"), http.StatusInsufficientStorage, apitypes.ErrorCodeInsufficientStorage)
	// Replacing frees the old bytes.
	if rr := d.upload("?replace=1", map[string]string{"a.bin": strings.Repeat("A", 90)}, "a.bin"); rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	list := decode[apitypes.StaticFileList](t, d.do("GET", "/api/v1/static", d.devTok, nil))
	if list.UsedBytes != 90 || list.QuotaBytes != 100 || list.FileCount != 1 || list.MaxFiles != 3 {
		t.Fatalf("usage %+v", list)
	}
	// File count cap.
	if rr := d.upload("", map[string]string{"c": "1", "d": "2"}, "c", "d"); rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	errBody(t, d.upload("", map[string]string{"e": "1"}, "e"), http.StatusInsufficientStorage, apitypes.ErrorCodeInsufficientStorage)
	// Free-space reserve.
	freeMu.Lock()
	free = 1<<30 - 1 // below the reserve
	freeMu.Unlock()
	if err := d.static.Remove("c"); err != nil {
		t.Fatal(err)
	}
	errBody(t, d.upload("?replace=1", map[string]string{"d": "123456"}, "d"), http.StatusInsufficientStorage, apitypes.ErrorCodeInsufficientStorage)
	freeMu.Lock()
	free = 10 << 30
	freeMu.Unlock()
	// Two uploads per account at a time.
	key := strconv.FormatInt(d.dev.ID, 10)
	r1, _ := d.static.BeginUpload(key)
	r2, _ := d.static.BeginUpload(key)
	errBody(t, d.upload("", map[string]string{"f": "1"}, "f"), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	r1()
	r2()
	if rr := d.upload("", map[string]string{"f": "1"}, "f"); rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	entries, _ := os.ReadDir(d.static.Dir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temporary file left: %s", e.Name())
		}
	}
}

// D8: every line of every env value is redacted from the job-log excerpt.
// D9: container output is sanitised and prefixed in job logs and sanitised
// (not redacted) in /logs.
func TestD8_D9_ContainerOutputInJobLogAndLogs(t *testing.T) {
	d := newDockerHarness(t)
	code := 1
	id := dockertest.ImageID(5)
	d.fake.AddImage(dockertest.Image{ID: id, Labels: dockertest.ManagedLabels(), ExitOnRun: &code, Logs: []string{
		"token=" + envMarker + " and again " + envMarker,
		"\x1b[2J\x1b]0;owned\x07red \x1b[31mtext",
		"server \"x\" is running (restart policy on-failure:5)",
		"broken \xff\xfe utf8",
	}})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "crashy", Image: id, Env: map[string]string{"TOKEN": envMarker}})
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || !strings.Contains(j.Detail, "exit code 1") {
		t.Fatalf("%s %q", j.Status, j.Detail)
	}
	log := d.jobLog(j.ID)
	for _, want := range []string{
		"container | 2026-01-01T00:00:00.000000000Z token=[redacted] and again [redacted]\n",
		`container | 2026-01-01T00:00:00.000000000Z \x1b[2J\x1b]0;owned\x07red \x1b[31mtext` + "\n",
		"container | 2026-01-01T00:00:00.000000000Z server \"x\" is running (restart policy on-failure:5)\n",
		"container | 2026-01-01T00:00:00.000000000Z broken \uFFFD\uFFFD utf8\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("job log lacks %q:\n%s", want, log)
		}
	}
	if strings.ContainsRune(log, 0x1b) || strings.ContainsRune(log, 0x07) {
		t.Fatal("raw control characters in the job log")
	}
	d.assertNoMarker()
	// /logs is the game's raw output: sanitised, never redacted.
	rr := d.call("GET", fmt.Sprintf("/api/v1/servers/%d/logs", s.ID), d.devTok, nil)
	body := rr.Body.String()
	if !strings.Contains(body, envMarker) || strings.ContainsRune(body, 0x1b) || !strings.Contains(body, `\x1b[31m`) || strings.Contains(body, "container | ") {
		t.Fatalf("logs %q", body)
	}
}

// D10: a PATCH never undoes a concurrent start or stop, and concurrent env
// changes from two processes are not lost.
func TestD10_PatchDoesNotUndoDesiredState(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img})
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	other := servers.New(d.st, box, d.dc, servers.Config{Ports: ports.Range{Lo: 7000, Hi: 7004}}, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	landed := map[string]bool{}
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			svc := d.svc
			if i%2 == 1 {
				svc = other // a second process on the same database
			}
			k := fmt.Sprintf("K%d", i)
			_, err := svc.Update(context.Background(), s.ID, apitypes.UpdateServerRequest{EnvSet: map[string]string{k: "v"}})
			switch {
			case err == nil:
				mu.Lock()
				landed[k] = true
				mu.Unlock()
			case !errors.Is(err, servers.ErrConflict): // 409: the client retries
				t.Error(err)
			}
		}(i)
		go func() {
			defer wg.Done()
			_ = d.st.SetServerDesiredState(context.Background(), s.ID, "running")
		}()
	}
	wg.Wait()
	got := d.getServer(d.devTok, s.ID)
	if got.DesiredState != apitypes.DesiredStateRunning {
		t.Fatalf("desired state %q", got.DesiredState)
	}
	// Every update that reported success is stored (none lost silently).
	for k := range landed {
		if !slices.Contains(got.EnvKeys, k) {
			t.Fatalf("update of %s reported success but was lost: %v", k, got.EnvKeys)
		}
	}
	if len(got.EnvKeys) != len(landed) || len(landed) < 10 {
		t.Fatalf("stored %v, landed %d", got.EnvKeys, len(landed))
	}
	// Deterministic interleaving: a PATCH between the start job and the
	// read keeps the job's desired state.
	if err := d.st.SetServerDesiredState(context.Background(), s.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	d.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", s.ID), d.devTok, apitypes.UpdateServerRequest{EnvSet: map[string]string{"Z": "1"}})
	if d.row(s.ID).DesiredState != "stopped" {
		t.Fatal("PATCH changed desired_state")
	}
}

// D11: a start re-checks the port against the current range; a port change
// on a running server is refused.
func TestD11_PortRules(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img, Port: ptr(7004)})
	// The daemon restarts with a narrower range.
	box, _ := secrets.New(bytes.Repeat([]byte{3}, secrets.KeySize))
	narrow := servers.New(d.st, box, d.dc, servers.Config{Ports: ports.Range{Lo: 7000, Hi: 7003}, ProbePort: func(string, int) error { return nil }}, nil)
	var log bytes.Buffer
	err := narrow.Start(context.Background(), s.ID, &log)
	if err == nil || !strings.Contains(err.Error(), "port 7004 is no longer allowed") || !strings.Contains(err.Error(), fmt.Sprintf("PATCH /api/v1/servers/%d", s.ID)) {
		t.Fatalf("got %v", err)
	}
	if len(d.fake.CallsTo("run")) != 0 {
		t.Fatal("started on a disallowed port")
	}
	// The panel's own port is never used.
	panel := servers.New(d.st, box, d.dc, servers.Config{Ports: ports.Range{Lo: 7000, Hi: 7004}, PanelPort: 7004, ProbePort: func(string, int) error { return nil }}, nil)
	if err := panel.Start(context.Background(), s.ID, &log); err == nil {
		t.Fatal("started on the panel port")
	}

	d.start(s.ID)
	base := fmt.Sprintf("/api/v1/servers/%d", s.ID)
	e := errBody(t, d.call("PATCH", base, d.devTok, apitypes.UpdateServerRequest{Port: ptr(7001)}), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "stop the server before changing its port") {
		t.Fatal(e.Message)
	}
	// Other fields, and the same port, are fine while running.
	if rr := d.call("PATCH", base, d.devTok, apitypes.UpdateServerRequest{Port: ptr(7004), EnvSet: map[string]string{"A": "1"}}); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	d.action(d.devTok, s.ID, "stop")
	if got := decode[apitypes.Server](t, d.call("PATCH", base, d.devTok, apitypes.UpdateServerRequest{Port: ptr(7001)})); got.Port != 7001 {
		t.Fatalf("%+v", got)
	}
}

// D12: Docker's stderr never reaches job logs or details; it goes to the
// daemon log with the job id.
func TestD12_DockerStderrStaysInDaemonLog(t *testing.T) {
	d := newDockerHarness(t)
	img := d.managedImage(1)
	d.fake.AddContainer(dockertest.Container{Name: "golem-deploy-arena", Image: img})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", Image: img})
	j := d.action(d.devTok, s.ID, "start")
	log := d.jobLog(j.ID)
	if j.Status != store.JobError || strings.Contains(log, "Error response from daemon") || strings.Contains(j.Detail, "Error response") {
		t.Fatalf("detail %q log %q", j.Detail, log)
	}
	if !strings.Contains(d.logs.String(), "Error response from daemon") || !strings.Contains(d.logs.String(), "job_id="+strconv.FormatInt(j.ID, 10)) {
		t.Fatalf("daemon log:\n%s", d.logs.String())
	}
	// Port already allocated by Docker: a fixed message.
	d.fake.RemoveContainer("golem-deploy-arena")
	d.fake.AddContainer(dockertest.Container{Name: "squatter", Ports: []string{"7000:7777/udp"}})
	j = d.action(d.devTok, s.ID, "start")
	if j.Detail != "port 7000 is already in use" || strings.Contains(d.jobLog(j.ID), "Bind for") {
		t.Fatalf("detail %q log %q", j.Detail, d.jobLog(j.ID))
	}
}

// D13: a multi-file upload is all or nothing, including when the commit
// fails half-way.
func TestD13_UploadIsAtomic(t *testing.T) {
	d := newDockerHarness(t)
	if rr := d.upload("", map[string]string{"a.txt": "old-a"}, "a.txt"); rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	// The second file's name is taken by a directory: its placement fails
	// after a.txt was already replaced, which must be rolled back.
	if err := os.Mkdir(filepath.Join(d.static.Dir(), "c.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	rr := d.upload("?replace=1", map[string]string{"a.txt": "new-a", "b.txt": "b", "c.txt": "c"}, "a.txt", "b.txt", "c.txt")
	if rr.Code < 400 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	f, _, err := d.static.OpenFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(f)
	f.Close()
	if buf.String() != "old-a" {
		t.Fatalf("a.txt = %q after a failed upload", buf.String())
	}
	if _, _, err := d.static.OpenFile("b.txt"); !errors.Is(err, staticfiles.ErrNotFound) {
		t.Fatal("b.txt visible after a failed upload")
	}
	// A conflict in the last file leaves nothing either.
	errBody(t, d.upload("", map[string]string{"x.txt": "x", "a.txt": "y"}, "x.txt", "a.txt"), http.StatusConflict, apitypes.ErrorCodeConflict)
	if _, _, err := d.static.OpenFile("x.txt"); !errors.Is(err, staticfiles.ErrNotFound) {
		t.Fatal("x.txt visible after a failed upload")
	}
}

// D14: the settle check reports the real exit code; a settled container
// gets its restart policy.
func TestD14_SettleExitCodeAndRestartPolicy(t *testing.T) {
	d := newDockerHarness(t)
	code := 127
	id := dockertest.ImageID(5)
	d.fake.AddImage(dockertest.Image{ID: id, Labels: dockertest.ManagedLabels(), ExitOnRun: &code})
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "crashy", Image: id})
	j := d.action(d.devTok, s.ID, "start")
	if j.Status != store.JobError || !strings.Contains(j.Detail, "exit code 127") {
		t.Fatalf("%s %q", j.Status, j.Detail)
	}
	if ct, _ := d.fake.Container("golem-deploy-crashy"); ct.RestartPolicy != "no" {
		t.Fatalf("restart policy %q on a failed start", ct.RestartPolicy)
	}
	if len(d.fake.CallsTo("update")) != 0 {
		t.Fatal("restart policy applied to a failed start")
	}
}

// D14: an export of the largest allowed configuration fits the import
// route's body limit and imports again.
func TestD14_ImportRoundTripAtMaximum(t *testing.T) {
	d := newDockerHarness(t, func(c *servers.Config) { c.Ports = ports.Range{Lo: 7000, Hi: 7000 + servers.MaxImportServers + 20} })
	img := d.managedImage(1)
	// Worst case for JSON size: every value byte needs a six-byte escape.
	big := map[string]string{}
	for i := 0; i < docker.MaxEnvBytes/docker.MaxEnvVarBytes; i++ {
		k := fmt.Sprintf("K%d", i)
		big[k] = strings.Repeat("<", docker.MaxEnvVarBytes-len(k))
	}
	doc := apitypes.ServerExport{FormatVersion: 1}
	for i := 0; i < servers.MaxImportServers; i++ {
		doc.Servers = append(doc.Servers, apitypes.ServerSpec{Name: fmt.Sprintf("s-%03d-%s", i, strings.Repeat("x", 50)), Image: img,
			Port: ptr(7000 + i), Env: big, DesiredState: apitypes.DesiredStateRunning})
	}
	rr := d.do("POST", "/api/v1/servers/import", d.adminSs, doc)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import %d %s", rr.Code, rr.Body.String()[:min(200, rr.Body.Len())])
	}
	exp := d.do("GET", "/api/v1/servers/export", d.adminSs, nil)
	if int64(exp.Body.Len()) > servers.MaxImportBytes {
		t.Fatalf("export of %d bytes exceeds the import limit %d", exp.Body.Len(), servers.MaxImportBytes)
	}
	t.Logf("maximum export: %d bytes (limit %d)", exp.Body.Len(), servers.MaxImportBytes)
	// Import it into a fresh installation.
	d2 := newDockerHarness(t, func(c *servers.Config) { c.Ports = ports.Range{Lo: 7000, Hi: 7000 + servers.MaxImportServers + 20} })
	d2.managedImage(1)
	var back apitypes.ServerExport
	if err := json.Unmarshal(exp.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if rr := d2.do("POST", "/api/v1/servers/import", d2.adminSs, back); rr.Code != http.StatusCreated {
		t.Fatalf("re-import %d %s", rr.Code, rr.Body.String()[:min(200, rr.Body.Len())])
	}
	too := apitypes.ServerExport{FormatVersion: 1, Servers: make([]apitypes.ServerSpec, servers.MaxImportServers+1)}
	errBody(t, d2.do("POST", "/api/v1/servers/import", d2.adminSs, too), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
}
