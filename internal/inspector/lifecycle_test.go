package inspector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Status drops the hub port at once when Docker shows the recorded
// container stopped or gone; nothing else has to check first.
func TestStatusDropsHub(t *testing.T) {
	for _, gone := range []bool{false, true} {
		a := newApplyEnv(t, true)
		a.enable(t)
		id := a.s.hubNow().container
		if gone {
			a.f.RemoveContainer(id)
		} else {
			a.f.SetContainerState(id, "exited", 0)
		}
		if st := a.status(t); st.State != apitypes.InspectorStateStopped {
			t.Fatalf("gone %v: %+v", gone, st)
		}
		v := a.s.hubNow()
		if v.port != 0 || (gone && v.container != "") {
			t.Fatalf("gone %v: hub kept %+v", gone, v)
		}
	}
}

// Turning the Inspector off takes effect when the setting is stored, before
// its job runs: admission closes, and every link, session, stream and agent
// relay ends.
func TestTurnOffTakesEffectAtOnce(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
	key := e.enter(nil)
	conn, br, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key))
	if code != http.StatusSwitchingProtocols {
		t.Fatal(code)
	}
	defer conn.Close()
	p := 7001
	sv, _ := e.st.CreateServer(ctx, store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
	tok, _ := e.s.AgentToken(sv.ID)
	agent, abr, code := dialWS(t, e.addr(), "/ws/agent", http.Header{"Authorization": {"Bearer " + tok}})
	if code != http.StatusSwitchingProtocols {
		t.Fatal(code)
	}
	defer agent.Close()
	link, err := e.s.CreateSession(ctx, e.acct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.storeEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn, br)
	waitClosed(t, agent, abr)
	if n, m := e.s.sessions.count(), e.s.agents.count(); n != 0 || m != 0 {
		t.Fatalf("%d sessions, %d relays after the turn-off", n, m)
	}
	if _, err := e.s.CreateSession(ctx, e.acct, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("new link: %v", err)
	}
	if r := e.exchange(ticketOf(t, link.Path), nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("earlier link: %d", r.StatusCode)
	}
}

// A PATCH without enabled writes no setting, so it cannot undo a turn-off
// that completed meanwhile.
func TestUpdateWithoutEnabledWritesNothing(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	a.s.hooks.afterSettingRead = func() {
		if err := a.st.SetSetting(ctx, store.SettingInspectorEnabled, "false"); err != nil {
			t.Error(err)
		}
	}
	a.update(t, apitypes.UpdateInspectorRequest{})
	if v, _, _ := a.st.GetSetting(ctx, store.SettingInspectorEnabled); v != "false" {
		t.Fatalf("setting %q", v)
	}
}

// A repair that read "on" checks the setting again before it publishes the
// hub as serving: a turn-off stored meanwhile wins, and the same job
// removes what it started.
func TestRepairChecksSettingBeforePublishing(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	s2, err := New(ctx, a.s.cfg) // a daemon restart: the repair reuses the hub
	if err != nil {
		t.Fatal(err)
	}
	a.s.Close()
	defer s2.Close()
	s2.hooks.beforePublish = func() {
		if err := s2.storeEnabled(ctx, false); err != nil {
			t.Error(err)
		}
	}
	var out bytes.Buffer
	if err := s2.apply(ctx, &out, applyRequest{auto: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if hubs, _ := s2.cfg.Docker.InspectorHubs(ctx); len(hubs) != 0 || s2.ln != nil || s2.admitting {
		t.Fatalf("the repair brought the hub up after the turn-off:\n%s", out.String())
	}
}

// Removing what a turn-off left is due at once: a failed attempt to start
// the hub just before does not delay it.
func TestCleanupNotPaced(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	a.s.noteAttempt(false) // a failed repair just now
	if err := a.s.storeEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !a.s.needsRepair(ctx) {
		t.Fatal("the removal waits for the pacing of failed attempts")
	}
	var out bytes.Buffer
	if err := a.s.apply(ctx, &out, applyRequest{auto: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if hubs, _ := a.s.cfg.Docker.InspectorHubs(ctx); len(hubs) != 0 || a.s.needsRepair(ctx) {
		t.Fatalf("after the removal:\n%s", out.String())
	}
}

// A hub that Docker reports running but that does not answer its readiness
// check is "stopped" after hubSickAfter sweeps, its port is not restored
// from Docker's metadata, and the repair replaces it.
func TestHubNotAnswering(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	a.hub.set(func(h *fakeHub) { h.readyStatus = http.StatusServiceUnavailable })
	for i := 0; i < hubSickAfter; i++ {
		if st := a.status(t); st.State != apitypes.InspectorStateRunning {
			t.Fatalf("stopped after %d failures: %+v", i, st)
		}
		a.s.checkHub(ctx)
	}
	if st := a.status(t); st.State != apitypes.InspectorStateStopped || st.Detail != sickDetail {
		t.Fatalf("%+v", st)
	}
	a.s.refreshHub(ctx, 0)
	if p := a.s.hubNow().port; p != 0 {
		t.Fatalf("port %d restored from Docker's metadata alone", p)
	}
	if !a.s.needsRepair(ctx) {
		t.Fatal("no repair for a hub that does not answer")
	}
	a.hub.set(func(h *fakeHub) { h.readyStatus = 0 })
	var out bytes.Buffer
	if err := a.s.apply(ctx, &out, applyRequest{auto: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(a.f.CallsTo("create")) != 2 || a.status(t).State != apitypes.InspectorStateRunning {
		t.Fatalf("the hub was not replaced:\n%s", out.String())
	}
}

// Every failure of an apply after the image step closes admission and ends
// every session.
func TestApplyFailureDrains(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	acct, _ := a.st.CreateAccount(ctx, store.NewAccount{Username: "dev", PasswordHash: "x", Role: store.RoleDeveloper, Approved: true})
	ses, err := a.s.CreateSession(ctx, acct, nil)
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://127.0.0.1:" + itoa(a.s.port())
	entryOf(t, postTicket(t, plainClient(), addr, ticketOf(t, ses.Path), addr, nil))
	a.hub.set(func(h *fakeHub) { h.loginStatus = http.StatusInternalServerError })
	if j, log := a.update(t, apitypes.UpdateInspectorRequest{}); j.Status != store.JobError {
		t.Fatalf("%+v\n%s", j, log)
	}
	if n := a.s.sessions.count(); n != 0 || a.s.admitting {
		t.Fatalf("%d sessions, admitting %v after a failed apply", n, a.s.admitting)
	}
}

// Apply drops the cached network addresses when Docker reports the network
// as not ours, and a query that started before cannot bring them back.
func TestApplyDropsNetworkCache(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	if _, err := a.s.cfg.Docker.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	peer := netip.MustParseAddr("172.30.0.5")
	if !a.s.peerAllowed(ctx, peer) {
		t.Fatal("peer of the managed network refused")
	}
	a.f.AddNetwork(docker.NetworkName, map[string]string{docker.LabelManaged: docker.ManagedValue, docker.LabelInstance: "ffffffffffffffffffffffffffffffff"}, nil)
	if err := a.s.storeEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := a.s.apply(ctx, &out, applyRequest{auto: true}); err == nil || !strings.Contains(err.Error(), "another golem-deploy installation") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if a.s.peerAllowed(ctx, peer) {
		t.Fatal("the addresses of a network that is not ours were kept")
	}
	// An older query finishes after the drop.
	started, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	a.s.netQuery = func(context.Context) (docker.NetworkAddresses, error) {
		once.Do(func() { close(started) })
		<-gate
		return docker.NetworkAddresses{Subnets: []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}}, nil
	}
	a.s.forgetNetwork()
	done := make(chan struct{})
	go func() {
		_, _ = a.s.networkAddrs(ctx, false)
		close(done)
	}()
	<-started
	a.s.dropNetwork(docker.ErrNotOurs)
	close(gate)
	<-done
	a.s.netMu.Lock()
	kept := len(a.s.netAddrs.Subnets)
	a.s.netMu.Unlock()
	if kept != 0 {
		t.Fatal("an older query brought the addresses back")
	}
}

// What a server launch gets comes from one snapshot: a replacement of the
// secrets or a turn-off while the agent address is looked up gives nothing.
func TestAgentSnapshot(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t, func(c *Config) { c.AgentHost = "172.30.0.1" })
	e.s.hooks.afterAgentHost = func() {
		if _, err := e.s.ensureSecrets(ctx, io.Discard, true); err != nil {
			t.Error(err)
		}
	}
	if a, ok := e.s.Agent(ctx); ok {
		t.Fatalf("secrets replaced meanwhile: %+v", a)
	}
	e.s.hooks.afterAgentHost = func() {
		e.s.mu.Lock()
		e.s.enabled = false
		e.s.mu.Unlock()
	}
	if a, ok := e.s.Agent(ctx); ok {
		t.Fatalf("turned off meanwhile: %+v", a)
	}
	e.s.hooks.afterAgentHost = nil
	e.s.mu.Lock()
	e.s.enabled = true
	gen := e.s.sec.Generation
	e.s.mu.Unlock()
	if a, ok := e.s.Agent(ctx); !ok || a.Generation != gen {
		t.Fatalf("%+v %v", a, ok)
	}
}
