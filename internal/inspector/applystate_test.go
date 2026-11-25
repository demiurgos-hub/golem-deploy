package inspector

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// enable turns the Inspector on and waits for the job.
func (a *applyEnv) enable(t *testing.T) {
	t.Helper()
	j, log := a.update(t, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)})
	if j.Status != store.JobSuccess {
		t.Fatalf("%+v\n%s", j, log)
	}
}

func (a *applyEnv) status(t *testing.T) apitypes.Inspector {
	t.Helper()
	st, err := a.s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The hub never starts on a network that still allows traffic between
// containers; a hub already there is removed, and the detail says how to
// get isolation. The automatic repair does the same.
func TestApplyNeedsIsolatedNetwork(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	if !strings.Contains(strings.Join(a.f.CallsTo("create")[0].Args, " "), "--cpus 1") {
		t.Fatal("the hub has no CPU limit")
	}
	// An older network: traffic between containers allowed, a game
	// attached.
	a.f.AddNetwork(docker.NetworkName, dockertest.OurLabels(), nil)
	a.f.AddContainer(dockertest.Container{Name: "golem-deploy-arena", Labels: dockertest.OurLabels(docker.LabelServerID, "1"),
		Network: docker.NetworkName, State: "running"})
	for _, auto := range []bool{false, true} {
		var j store.Job
		var log string
		if auto {
			var out bytes.Buffer
			err := a.s.apply(ctx, &out, applyRequest{auto: true})
			if err == nil {
				t.Fatalf("repair started the hub:\n%s", out.String())
			}
			log = out.String() + err.Error()
		} else {
			j, log = a.update(t, apitypes.UpdateInspectorRequest{})
			if j.Status != store.JobError || !strings.Contains(j.Detail, "stop every server, then restart the daemon") {
				t.Fatalf("%+v\n%s", j, log)
			}
		}
		if hubs, _ := a.s.cfg.Docker.InspectorHubs(ctx); len(hubs) != 0 {
			t.Fatalf("a hub runs on the shared network (auto %v)", auto)
		}
		if len(a.f.CallsTo("create")) != 1 {
			t.Fatalf("a hub was created on the shared network:\n%s", log)
		}
		if st := a.status(t); st.State != apitypes.InspectorStateStopped || !strings.Contains(st.Detail, "stop every server") {
			t.Fatalf("%+v", st)
		}
	}
}

// A failed download happens before anything is rotated or replaced: it
// rotates nothing and leaves a hub that serves as it is. After a failure
// that touched the hub the state is "stopped" with a detail: a kept hub
// that refuses the login is not reported running.
func TestApplyFailureStates(t *testing.T) {
	a := newApplyEnv(t, true)
	a.enable(t)
	gen := a.s.sec.Generation
	a.f.SetPullHook(func(string) error { return errors.New("pull access denied") })
	j, log := a.update(t, apitypes.UpdateInspectorRequest{PullImage: true, RotateSecrets: true})
	if j.Status != store.JobError || a.s.sec.Generation != gen {
		t.Fatalf("rotated before the download: %+v gen %d\n%s", j, a.s.sec.Generation, log)
	}
	if st := a.status(t); st.State != apitypes.InspectorStateRunning || st.Detail != "" || !strings.Contains(j.Detail, "could not be downloaded") {
		t.Fatalf("a failed download took a working Inspector offline: %+v %+v\n%s", st, j, log)
	}
	a.f.SetPullHook(nil)
	a.enable(t)
	if st := a.status(t); st.State != apitypes.InspectorStateRunning {
		t.Fatalf("%+v", st)
	}
	a.hub.set(func(h *fakeHub) { h.loginStatus = http.StatusInternalServerError })
	j, log = a.update(t, apitypes.UpdateInspectorRequest{})
	if j.Status != store.JobError || !strings.Contains(log, "running with the current settings") {
		t.Fatalf("%+v\n%s", j, log)
	}
	st := a.status(t)
	if st.State != apitypes.InspectorStateStopped || !strings.Contains(st.Detail, "did not accept the daemon's login") || st.StartedAt == nil {
		t.Fatalf("%+v", st)
	}
	// started_at is reported while the hub runs, even with the listener
	// closed.
	a.s.closeListener()
	if st := a.status(t); st.StartedAt == nil {
		t.Fatalf("%+v", st)
	}
}

// A hub refused the daemon's password: the automatic repair against the
// same hub and secrets does not log in again; new secrets do.
func TestApplyKeepsLoginHold(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	a.hub.set(func(h *fakeHub) { h.loginStatus = http.StatusUnauthorized })
	if _, err := a.s.hubLogin(ctx, a.s.hubNow()); !errors.Is(err, errHubCredentials) {
		t.Fatalf("%v", err)
	}
	a.hub.set(func(h *fakeHub) { h.loginStatus = 0 })
	logins := a.hub.loginCount()
	var out bytes.Buffer
	if err := a.s.apply(ctx, &out, applyRequest{auto: true}); err == nil {
		t.Fatalf("the repair logged in again:\n%s", out.String())
	}
	if a.hub.loginCount() != logins || !strings.Contains(a.s.detail, "rotate the Inspector's secrets") {
		t.Fatalf("logins %d, detail %q", a.hub.loginCount()-logins, a.s.detail)
	}
	j, log := a.update(t, apitypes.UpdateInspectorRequest{RotateSecrets: true})
	if j.Status != store.JobSuccess {
		t.Fatalf("%+v\n%s", j, log)
	}
}

// The setting is written before the job starts; a failed write fails the
// request and starts nothing. The job follows the setting as it is when
// the job runs.
func TestUpdatePersistsFirst(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	// The setting is off in the database (a turn-off whose job did not
	// run): the repair job follows it and removes the hub.
	if err := a.st.SetSetting(ctx, store.SettingInspectorEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := a.s.apply(ctx, &out, applyRequest{auto: true}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if hubs, _ := a.s.cfg.Docker.InspectorHubs(ctx); len(hubs) != 0 || a.s.enabled || a.s.ln != nil {
		t.Fatalf("the repair did not follow the setting:\n%s", out.String())
	}
	a.s.hooks.beforeStoreEnabled = func() { _ = a.st.Close() }
	if _, err := a.s.Update(ctx, apitypes.UpdateInspectorRequest{Enabled: boolPtr(true)}, nil); err == nil {
		t.Fatal("a failed write answered success")
	}
	if a.s.enabled || a.s.applying() || len(a.f.CallsTo("create")) != 1 {
		t.Fatal("something ran ahead of the database")
	}
}

// A hub that should not exist (setting off, or the listener "off") is
// removed when the daemon starts.
func TestStartRemovesStrayHub(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		a := newApplyEnv(t, true)
		ctx := context.Background()
		a.enable(t)
		cfg := a.s.cfg
		a.s.Close()
		if unavailable {
			cfg.Listen = ""
		} else if err := a.st.SetSetting(ctx, store.SettingInspectorEnabled, "false"); err != nil {
			t.Fatal(err)
		}
		s2, err := New(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			s2.Run(rctx)
			close(done)
		}()
		waitFor(t, "the stray hub to go", func() bool {
			hubs, err := cfg.Docker.InspectorHubs(ctx)
			return err == nil && len(hubs) == 0
		})
		cancel()
		<-done
	}
}

// Cached network addresses are dropped when Docker reports the network as
// not ours.
func TestNetworkCacheDropped(t *testing.T) {
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
	a.s.forgetNetwork()
	if a.s.peerAllowed(ctx, peer) {
		t.Fatal("the addresses of a network that is not ours were kept")
	}
}
