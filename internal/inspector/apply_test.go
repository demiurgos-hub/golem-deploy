package inspector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// applyEnv is a Service on the Docker fake, whose hub image "runs" the
// fake hub: the published hub port is the fake hub's port.
type applyEnv struct {
	s      *Service
	st     *store.Store
	f      *dockertest.Fake
	hub    *fakeHub
	runner *jobs.Runner
	logs   *syncBuffer
	img    string
}

const testImage = "ghcr.io/demiurgos-hub/golem-inspector:test"

func newApplyEnv(t *testing.T, imagePresent bool) *applyEnv {
	t.Helper()
	st, box := openStore(t)
	f := dockertest.New()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dc := docker.New(docker.Options{Runner: f, InstanceID: dockertest.InstanceID, TempDir: t.TempDir(), Logger: logger,
		BuildConfigDir: filepath.Join(t.TempDir(), "docker-build")})
	runner := jobs.New(st, t.TempDir(), jobs.Options{Logger: logger})
	t.Cleanup(func() { <-runner.Stop() })
	hub := newFakeHub(t)
	hub.hash = func() string { return f.LatestEnv("GOLEM_INSPECTOR_PASSWORD_HASH") }
	hub.agentToken = func() string { return f.LatestEnv("GOLEM_INSPECTOR_AGENT_TOKEN") }
	img := dockertest.Image{ID: dockertest.ImageID(77), AssignPort: hub.port(), Logs: []string{"golem-inspector: listening"}}
	if imagePresent {
		img.Tags = []string{testImage}
		f.AddImage(img)
	} else {
		f.AddRegistryImage(testImage, img)
	}
	s, err := New(context.Background(), Config{Store: st, Secrets: box, Docker: dc, Jobs: runner, Policy: e2ePolicy(), Logger: logger,
		Image: testImage, Listen: "127.0.0.1:0", GOOS: "linux", BcryptCost: bcrypt.MinCost, ReadyTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return &applyEnv{s: s, st: st, f: f, hub: hub, runner: runner, logs: logs, img: img.ID}
}

// update runs PATCH /api/v1/inspector's service call and waits for the job.
func (a *applyEnv) update(t *testing.T, req apitypes.UpdateInspectorRequest) (store.Job, string) {
	t.Helper()
	job, err := a.s.Update(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.runner.Done(job.ID):
	case <-time.After(20 * time.Second):
		t.Fatal("job did not finish")
	}
	j, err := a.st.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return j, a.jobLog(t, j.ID)
}

func (a *applyEnv) jobLog(t *testing.T, id int64) string {
	t.Helper()
	b, err := os.ReadFile(a.runner.LogPath(id))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func boolPtr(v bool) *bool { return &v }

// Turning the Inspector on creates the hardened hub from the resolved
// image with its settings in an env file, waits for it, logs in and opens
// the listener; applying again keeps a matching hub; turning it off
// removes everything.
func TestApplyLifecycle(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.hub.set(func(h *fakeHub) {
		h.targets = []map[string]any{{"targetId": "arena", "connected": true, "entityCount": 12, "connectedAt": "2026-11-25T09:00:00Z",
			"labels": map[string]string{"x": "<script>"}}, {"targetId": "ghost", "connected": true}}
	})
	j, log := a.update(t, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)})
	if j.Status != store.JobSuccess || !j.Restricted || j.Kind != string(apitypes.JobKindInspectorApply) ||
		j.SubjectType != string(apitypes.SubjectTypeInspector) || j.SubjectID != "hub" {
		t.Fatalf("%+v\n%s", j, log)
	}
	creates := a.f.CallsTo("create")
	if len(creates) != 1 {
		t.Fatalf("creates %d", len(creates))
	}
	args := strings.Join(creates[0].Args, " ")
	for _, want := range []string{"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--user 65532:65532", "--pull never",
		"--memory 512m", "--publish 127.0.0.1::8787/tcp", "--network golem-deploy", "dev.golemengine.deploy.inspector-hub=true", "-- " + a.img} {
		if !strings.Contains(args, want) {
			t.Errorf("create lacks %q", want)
		}
	}
	if len(a.f.CallsTo("pull")) != 0 {
		t.Fatal("pulled an image the host has")
	}
	sec := a.s.sec
	for _, secret := range []string{sec.Password, sec.AgentToken, sec.PasswordHash, sec.TokenKey} {
		if strings.Contains(args, secret) || strings.Contains(log, secret) || strings.Contains(a.logs.String(), secret) {
			t.Fatal("a secret reached argv or a log")
		}
	}
	env := creates[0].EnvFile
	for _, want := range []string{"GOLEM_INSPECTOR_PRODUCTION=true", "GOLEM_INSPECTOR_BEHIND_PROXY=true", "GOLEM_INSPECTOR_PUBLIC_ORIGINS=" + HubOrigin,
		"GOLEM_INSPECTOR_SESSION_MAX_AGE=12h0m0s", "GOLEM_INSPECTOR_AGENT_TOKEN=" + sec.AgentToken, "GOLEM_INSPECTOR_PASSWORD_HASH=" + sec.PasswordHash,
		"GOLEM_INSPECTOR_ADDR=:8787"} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("env file lacks %q", want)
		}
	}
	if cost, _ := bcrypt.Cost([]byte(sec.PasswordHash)); cost != bcrypt.MinCost {
		t.Fatalf("cost %d", cost)
	}
	st, err := a.s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != apitypes.InspectorStateRunning || !st.Enabled || !st.Available || st.ImageID != a.img || st.Image != testImage ||
		st.StartedAt == nil || st.Port == 0 || st.AgentsReachable || st.AgentAddress != "" || st.Detail != "" {
		t.Fatalf("%+v", st)
	}
	if len(st.Targets) != 0 || st.OtherTargets != 2 {
		// No server exists yet: both targets are other targets.
		t.Fatalf("targets %+v other %d", st.Targets, st.OtherTargets)
	}
	p := 7001
	arena, err := a.st.CreateServer(ctx, store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(targetsTTL)
	st, _ = a.s.Status(ctx)
	if len(st.Targets) != 1 || st.Targets[0].ServerID != arena.ID || !st.Targets[0].Connected || st.Targets[0].Entities != 12 ||
		st.Targets[0].ConnectedAt == nil || st.OtherTargets != 1 {
		t.Fatalf("targets %+v other %d", st.Targets, st.OtherTargets)
	}
	// The listener is open and serves the Inspector.
	addr := "127.0.0.1:" + itoa(a.s.port())
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// Applying again keeps the matching hub.
	j, log = a.update(t, apitypes.UpdateInspectorRequest{})
	if j.Status != store.JobSuccess || len(a.f.CallsTo("create")) != 1 || !strings.Contains(log, "running with the current settings") {
		t.Fatalf("%+v\n%s", j, log)
	}
	// A browser session for the rotation below.
	acct, _ := a.st.CreateAccount(ctx, store.NewAccount{Username: "dev", PasswordHash: "x", Role: store.RoleDeveloper, Approved: true})
	ses, err := a.s.CreateSession(ctx, acct, nil)
	if err != nil {
		t.Fatal(err)
	}
	entryOf(t, postTicket(t, plainClient(), "http://"+addr, ticketOf(t, ses.Path), "http://"+addr, nil))
	if a.s.sessions.count() != 1 {
		t.Fatal("no session")
	}
	// Rotating replaces the secrets (a new generation), the hub and the
	// sessions.
	gen, tok := a.s.sec.Generation, mustToken(t, a.s, arena.ID)
	j, log = a.update(t, apitypes.UpdateInspectorRequest{RotateSecrets: true})
	if j.Status != store.JobSuccess || a.s.sec.Generation <= gen || mustToken(t, a.s, arena.ID) == tok ||
		len(a.f.CallsTo("create")) != 2 || a.s.sessions.count() != 0 {
		t.Fatalf("rotate: %+v gen %d\n%s", j, a.s.sec.Generation, log)
	}
	hubs, _ := a.s.cfg.Docker.InspectorHubs(ctx)
	if len(hubs) != 1 {
		t.Fatalf("hubs %d", len(hubs))
	}
	// Turning it off closes the listener and removes the container.
	j, log = a.update(t, apitypes.UpdateInspectorRequest{Enabled: boolPtr(false)})
	if j.Status != store.JobSuccess {
		t.Fatalf("%+v\n%s", j, log)
	}
	if hubs, _ := a.s.cfg.Docker.InspectorHubs(ctx); len(hubs) != 0 {
		t.Fatal("hub kept")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener still open")
	}
	st, _ = a.s.Status(ctx)
	if st.State != apitypes.InspectorStateOff || st.Enabled || st.Targets != nil {
		t.Fatalf("%+v", st)
	}
	if v, _, _ := a.st.GetSetting(ctx, store.SettingInspectorEnabled); v != "false" {
		t.Fatalf("setting %q", v)
	}
	// Pull and rotate need the Inspector on.
	var fe *FieldError
	if _, err := a.s.Update(ctx, apitypes.UpdateInspectorRequest{PullImage: true}, nil); !errors.As(err, &fe) || fe.Field != "pull_image" {
		t.Fatalf("pull while off: %v", err)
	}
	if _, err := a.s.Update(ctx, apitypes.UpdateInspectorRequest{RotateSecrets: true}, nil); !errors.As(err, &fe) || fe.Field != "rotate_secrets" {
		t.Fatalf("rotate while off: %v", err)
	}
}

func mustToken(t *testing.T, s *Service, id int64) string {
	t.Helper()
	tok, err := s.AgentToken(id)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func itoa(n int) string { return strconv.Itoa(n) }

// The image is downloaded only when the host does not have it or when
// asked; the automatic repair never downloads.
func TestApplyImage(t *testing.T) {
	a := newApplyEnv(t, false)
	ctx := context.Background()
	var log bytes.Buffer
	err := a.s.apply(ctx, &log, applyRequest{enabled: true, auto: true})
	if err == nil || len(a.f.CallsTo("pull")) != 0 || !strings.Contains(a.s.detail, "not on this host") {
		t.Fatalf("auto repair: %v %q", err, a.s.detail)
	}
	st, _ := a.s.Status(ctx)
	if st.State != apitypes.InspectorStateOff {
		// Not enabled yet: the setting is stored by Update only.
		t.Fatalf("%+v", st)
	}
	j, out := a.update(t, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)})
	pulls := a.f.CallsTo("pull")
	if j.Status != store.JobSuccess || len(pulls) != 1 || !slices.Equal(pulls[0].Args, []string{"pull", "--quiet", "--", testImage}) {
		t.Fatalf("%+v %v\n%s", j, pulls, out)
	}
	if !slices.ContainsFunc(pulls[0].Env, func(v string) bool {
		return strings.HasPrefix(v, "DOCKER_CONFIG=") && strings.HasSuffix(v, "docker-build")
	}) {
		t.Fatalf("pull env %v", pulls[0].Env)
	}
	// pull_image downloads again even though the host has the image.
	j, out = a.update(t, apitypes.UpdateInspectorRequest{PullImage: true})
	if j.Status != store.JobSuccess || len(a.f.CallsTo("pull")) != 2 {
		t.Fatalf("%+v\n%s", j, out)
	}
	// A failed download leaves a clear detail.
	a.f.SetPullHook(func(string) error { return errors.New("pull access denied") })
	j, out = a.update(t, apitypes.UpdateInspectorRequest{PullImage: true})
	if j.Status != store.JobError || !strings.Contains(j.Detail, "could not be downloaded") || strings.Contains(out, "pull access denied") {
		t.Fatalf("%+v\n%s", j, out)
	}
}

// A hub that exits during start fails the job with a user-safe detail and
// its output (sanitised, prefixed, secrets redacted) in the job log.
func TestApplyHubExits(t *testing.T) {
	a := newApplyEnv(t, true)
	code := 2
	a.f.AddImage(dockertest.Image{ID: a.img, Tags: []string{testImage}, ExitOnRun: &code,
		Logs: []string{"golem-inspector: production requires \x1b[31mthings", "line two"}})
	j, log := a.update(t, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)})
	if j.Status != store.JobError || !strings.Contains(j.Detail, "exited during start") {
		t.Fatalf("%+v\n%s", j, log)
	}
	if !strings.Contains(log, "golem-inspector: production requires \\x1b[31mthings") || strings.Contains(log, "\x1b") ||
		strings.Count(log, "\n"+hubLogPrefix) != 2 {
		t.Fatalf("log:\n%s", log)
	}
	st, _ := a.s.Status(context.Background())
	if st.State != apitypes.InspectorStateStopped || !strings.Contains(st.Detail, "exited during start") {
		t.Fatalf("%+v", st)
	}
	// One active job per subject.
	block := make(chan struct{})
	a.f.SetPullHook(func(string) error { <-block; return nil })
	if _, err := a.s.Update(context.Background(), apitypes.UpdateInspectorRequest{PullImage: true}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.Update(context.Background(), apitypes.UpdateInspectorRequest{}, nil); !errors.Is(err, jobs.ErrSubjectBusy) {
		t.Fatalf("second job: %v", err)
	}
	close(block)
}

// Without a listener (--inspector-listen off) the Inspector cannot be
// turned on.
func TestUnavailable(t *testing.T) {
	st, box := openStore(t)
	s, err := New(context.Background(), Config{Store: st, Secrets: box, Policy: e2ePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	var fe *FieldError
	if _, err := s.Update(context.Background(), apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)}, nil); !errors.As(err, &fe) || fe.Field != "enabled" {
		t.Fatalf("%v", err)
	}
	v, err := s.Status(context.Background())
	if err != nil || v.Available || v.Port != 0 || v.State != apitypes.InspectorStateOff {
		t.Fatalf("%+v %v", v, err)
	}
}

// The repair runs for a hub that should be on but is not serving, at most
// every RepairInterval.
func TestRepair(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	if a.s.needsRepair(ctx) {
		t.Fatal("repair while off")
	}
	j, log := a.update(t, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)})
	if j.Status != store.JobSuccess {
		t.Fatalf("%+v\n%s", j, log)
	}
	if a.s.needsRepair(ctx) {
		t.Fatal("repair while serving")
	}
	// The daemon restarted: a new Service finds the setting on and the hub
	// running; the repair reuses it and opens the listener.
	s2, err := New(ctx, a.s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.s.Close()
	defer s2.Close()
	if !s2.needsRepair(ctx) {
		t.Fatal("no repair after a restart")
	}
	var out bytes.Buffer
	if err := s2.apply(ctx, &out, applyRequest{enabled: true, auto: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(a.f.CallsTo("create")) != 1 || !strings.Contains(out.String(), "running with the current settings") {
		t.Fatalf("repair replaced the hub:\n%s", out.String())
	}
	if s2.needsRepair(ctx) {
		t.Fatal("repair right after a repair")
	}
}
