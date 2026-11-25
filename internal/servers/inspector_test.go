package servers

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// fakeInspector is a servers.Inspector with settable answers. It counts
// the calls of Agent.
type fakeInspector struct {
	mu    sync.Mutex
	agent InspectorAgent
	ok    bool
	calls int
	// afterAgent runs at the end of Agent, with the lock released.
	afterAgent func()
}

func (f *fakeInspector) Agent(context.Context) (InspectorAgent, bool) {
	f.mu.Lock()
	f.calls++
	a, ok, after := f.agent, f.ok, f.afterAgent
	f.mu.Unlock()
	gen := a.Generation
	a.Token = func(id int64) (string, error) { return fakeToken(id, gen), nil }
	if after != nil {
		after()
	}
	return a, ok
}

func (f *fakeInspector) AgentToken(id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeToken(id, f.agent.Generation), nil
}

func fakeToken(id, gen int64) string {
	return "gdi_" + strconv.FormatInt(id, 10) + "_secret-token-" + strconv.FormatInt(gen, 10)
}

func (f *fakeInspector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeInspector) set(fn func(*fakeInspector)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// The hash of a server with the switch off is the same whatever the
// Inspector does (and the same as before the Inspector existed: the golden
// value was computed by the previous code). A server with the switch on
// changes its hash with the agent endpoint and the token generation, and
// only while the Inspector injects anything.
func TestInspectorConfigHash(t *testing.T) {
	fx := newFixture(t, Config{})
	svc := fx.svc
	port := 7001
	b := store.Build{ID: 1, ImageID: dockertest.ImageID(1)}
	off := store.Server{ID: 1, Port: &port, Env: []byte("ct")}
	p := runtimeParams{gameHost: "play.example.com", certMode: certs.ModeSelfSigned, certDir: "/data/certs/game", groupAdd: []int{1000}}
	base := svc.configHash(off, &b, p)
	const golden = "d076f13df2d942ee5b49fd98fa1caaff" // computed at commit 7f3ef69
	if base != golden {
		t.Fatalf("hash of a server without the Inspector changed: %s", base)
	}
	withAgent := p
	withAgent.inspector, withAgent.inspectorOK = InspectorAgent{HubURL: "ws://172.30.0.1:8444/ws/agent", Generation: 5}, true
	if got := svc.configHash(off, &b, withAgent); got != base {
		t.Fatalf("the Inspector changed the hash of a server with the switch off: %s", got)
	}
	on := off
	on.Inspector = true
	if got := svc.configHash(on, &b, p); got != base {
		t.Fatalf("the switch alone changed the hash while nothing is injected: %s", got)
	}
	active := svc.configHash(on, &b, withAgent)
	if active == base {
		t.Fatal("injecting the agent did not change the hash")
	}
	gen := withAgent
	gen.inspector.Generation = 6
	moved := withAgent
	moved.inspector.HubURL = "ws://172.30.0.1:9444/ws/agent"
	for name, q := range map[string]runtimeParams{"generation": gen, "endpoint": moved} {
		if svc.configHash(on, &b, q) == active {
			t.Errorf("%s change did not change the hash", name)
		}
	}
	if !inspectorActive(on, withAgent) || inspectorActive(off, withAgent) || inspectorActive(on, p) {
		t.Fatal("inspectorActive")
	}
}

func TestInspectorEnvLabels(t *testing.T) {
	sv := store.Server{ID: 12, Name: "arena", Inspector: true}
	p := runtimeParams{inspector: InspectorAgent{HubURL: "ws://h:8444/ws/agent"}, inspectorOK: true}
	b := store.Build{Commit: commitA}
	env := inspectorEnv(sv, b, "game", p, "tok")
	want := map[string]string{
		EnvInspectorHubURL: "ws://h:8444/ws/agent", EnvInspectorToken: "tok", EnvInspectorTargetID: "arena",
		EnvInspectorLabels: "project=game,commit=" + commitA + ",server=12",
	}
	if len(env) != len(want) {
		t.Fatalf("%v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if got := inspectorEnv(sv, b, "a,b=c", p, "tok")[EnvInspectorLabels]; got != "commit="+commitA+",server=12" {
		t.Fatalf("labels %q", got)
	}
	sv.Inspector = false
	if inspectorEnv(sv, b, "game", p, "tok") != nil {
		t.Fatal("variables for a server with the switch off")
	}
}

// A server with the switch on gets the agent variables in its env file
// (never in argv or the job log), the container records it in its label
// and the view reports inspector_active; a new token generation makes the
// running server report restart_required. A server with the switch off
// gets none of it.
func TestInspectorInjection(t *testing.T) {
	insp := &fakeInspector{agent: InspectorAgent{HubURL: "ws://172.30.0.1:8444/ws/agent", Generation: 1}, ok: true}
	fx := newFixture(t, Config{Inspector: insp})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	on := true
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID, Inspector: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !sv.Inspector || sv.InspectorActive {
		t.Fatalf("%+v", sv)
	}
	log, err := startJob(t, fx, sv.ID)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	token, _ := insp.AgentToken(sv.ID)
	if strings.Contains(log, token) {
		t.Fatal("token in the job log")
	}
	for _, c := range fx.f.CallsTo("create") {
		if strings.Contains(strings.Join(c.Args, " "), token) {
			t.Fatal("token in argv")
		}
		if !strings.Contains(c.EnvFile, EnvInspectorToken+"="+token) {
			t.Fatalf("env file %q", c.EnvFile)
		}
	}
	row, _ := fx.st.GetServer(ctx, sv.ID)
	ct, _ := fx.f.Container(row.ContainerID)
	if ct.Labels[docker.LabelInspector] != "true" || ct.Labels[docker.LabelInspectorHub] != "" {
		t.Fatalf("labels %v", ct.Labels)
	}
	if ct.Env[EnvInspectorHubURL] != "ws://172.30.0.1:8444/ws/agent" || ct.Env[EnvInspectorTargetID] != "arena" ||
		ct.Env[EnvInspectorLabels] != "project=game,commit="+commitA+",server="+strconv.FormatInt(sv.ID, 10) {
		t.Fatalf("env %v", ct.Env)
	}
	v, err := fx.svc.Get(ctx, sv.ID)
	if err != nil || !v.InspectorActive || v.RestartRequired {
		t.Fatalf("%+v %v", v, err)
	}
	insp.set(func(f *fakeInspector) { f.agent.Generation = 2 })
	if v, _ := fx.svc.Get(ctx, sv.ID); !v.RestartRequired || !v.InspectorActive {
		t.Fatalf("after rotation %+v", v)
	}
	// Turning the server's switch off applies on the next start.
	off := false
	if v, err := fx.svc.Update(ctx, sv.ID, apitypes.UpdateServerRequest{Inspector: &off}); err != nil || v.Inspector || !v.RestartRequired {
		t.Fatalf("%+v %v", v, err)
	}
	if log, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	row, _ = fx.st.GetServer(ctx, sv.ID)
	ct, _ = fx.f.Container(row.ContainerID)
	if ct.Labels[docker.LabelInspector] != "" || ct.Env[EnvInspectorHubURL] != "" || ct.Env[EnvInspectorToken] != "" {
		t.Fatalf("switch off: %v %v", ct.Labels, ct.Env)
	}
	if v, _ := fx.svc.Get(ctx, sv.ID); v.InspectorActive || v.RestartRequired {
		t.Fatalf("%+v", v)
	}
	// With the switch on while the Inspector injects nothing, the start
	// says so and the container gets nothing.
	if _, err := fx.svc.Update(ctx, sv.ID, apitypes.UpdateServerRequest{Inspector: &on}); err != nil {
		t.Fatal(err)
	}
	insp.set(func(f *fakeInspector) { f.ok = false })
	log, err = startJob(t, fx, sv.ID)
	if err != nil || !strings.Contains(log, "the agent is not configured") {
		t.Fatalf("%v\n%s", err, log)
	}
	row, _ = fx.st.GetServer(ctx, sv.ID)
	ct, _ = fx.f.Container(row.ContainerID)
	if ct.Labels[docker.LabelInspector] != "" || ct.Env[EnvInspectorHubURL] != "" {
		t.Fatalf("not injected: %v", ct.Env)
	}
}

// The token, the endpoint and the generation come from one snapshot: a
// rotation right after the launch read them does not give the container a
// token of another generation than its config hash.
func TestInspectorSnapshot(t *testing.T) {
	insp := &fakeInspector{agent: InspectorAgent{HubURL: "ws://172.30.0.1:8444/ws/agent", Generation: 1}, ok: true}
	fx := newFixture(t, Config{Inspector: insp})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	on := true
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID, Inspector: &on})
	if err != nil {
		t.Fatal(err)
	}
	insp.set(func(f *fakeInspector) {
		f.afterAgent = func() { insp.set(func(f *fakeInspector) { f.agent.Generation = 2; f.afterAgent = nil }) }
	})
	if log, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	row, _ := fx.st.GetServer(ctx, sv.ID)
	ct, _ := fx.f.Container(row.ContainerID)
	if got := ct.Env[EnvInspectorToken]; got != fakeToken(sv.ID, 1) {
		t.Fatalf("token %q is not of the generation the launch read", got)
	}
	// The hash is of generation 1 too, so generation 2 asks for a restart.
	if v, _ := fx.svc.Get(ctx, sv.ID); !v.RestartRequired {
		t.Fatalf("%+v", v)
	}
}

// Listings ask the Inspector only when some server has the switch on.
func TestInspectorSkippedWithoutSwitch(t *testing.T) {
	insp := &fakeInspector{agent: InspectorAgent{HubURL: "ws://172.30.0.1:8444/ws/agent", Generation: 1}, ok: true}
	fx := newFixture(t, Config{Inspector: insp})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	before := insp.callCount()
	if _, err := fx.svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Get(ctx, sv.ID); err != nil {
		t.Fatal(err)
	}
	if log, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if n := insp.callCount() - before; n != 0 {
		t.Fatalf("the Inspector was asked %d times without a server with the switch on", n)
	}
	on := true
	if _, err := fx.svc.Update(ctx, sv.ID, apitypes.UpdateServerRequest{Inspector: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	if insp.callCount() == before {
		t.Fatal("the Inspector was not asked for a server with the switch on")
	}
}

// The switch survives export and import, and the hub container is not a
// foreign container.
func TestInspectorExportImportAndHub(t *testing.T) {
	fx := newFixture(t, Config{})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	on := true
	if _, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID, Inspector: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "plain", BuildID: b.ID}); err != nil {
		t.Fatal(err)
	}
	doc, err := fx.svc.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sp := range doc.Servers {
		got[sp.Name] = sp.Inspector
	}
	if !got["arena"] || got["plain"] {
		t.Fatalf("%v", got)
	}
	for i := range doc.Servers {
		doc.Servers[i].Name += "-copy"
		doc.Servers[i].Port = nil
	}
	created, err := fx.svc.Import(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range created {
		if v.Inspector != (v.Name == "arena-copy") {
			t.Fatalf("%s: %v", v.Name, v.Inspector)
		}
	}
	fx.f.AddContainer(dockertest.Container{Name: docker.InspectorHubName, Image: dockertest.ImageID(99),
		Labels: dockertest.OurLabels(docker.LabelInspectorHub, "true")})
	if _, err := fx.svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fx.logs.String(), "not the container recorded for any server") {
		t.Fatalf("hub reported as foreign:\n%s", fx.logs.String())
	}
}
