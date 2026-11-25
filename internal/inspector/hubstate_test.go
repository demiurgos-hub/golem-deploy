package inspector

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// dockerList makes the env's Status list the fake hub's container like
// Docker would.
func (e *testEnv) dockerList() {
	e.s.listHubs = func(ctx context.Context) ([]docker.Container, error) {
		ct, err := e.hub.inspect(ctx, testHubID)
		if errors.Is(err, docker.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []docker.Container{ct}, nil
	}
}

// A sweep whose Docker call outlives its budget learned nothing about the
// hub: no repair starts, no session ends, the state is "unknown" and the
// port is not used until Docker answers again. A step whose context is
// done does nothing.
func TestSweepWithoutDockerAnswer(t *testing.T) {
	a := newApplyEnv(t, true)
	ctx := context.Background()
	a.enable(t)
	acct, _ := a.st.CreateAccount(ctx, store.NewAccount{Username: "dev", PasswordHash: "x", Role: store.RoleDeveloper, Approved: true})
	ses, err := a.s.CreateSession(ctx, acct, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:" + itoa(a.s.port())
	key := entryOf(t, postTicket(t, plainClient(), base, ticketOf(t, ses.Path), base, nil)).Key
	get := func() int {
		t.Helper()
		req, _ := http.NewRequest("GET", base+"/api/targets", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := plainClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("before: %d", c)
	}
	jobsBefore, _ := a.st.ListJobs(ctx, store.JobFilter{})

	inspect, list := a.s.inspectHub, a.s.listHubs
	release := make(chan struct{})
	var asked atomic.Int32
	a.s.inspectHub = func(ctx context.Context, _ string) (docker.Container, error) {
		asked.Add(1)
		select { // Docker does not answer within the budget
		case <-ctx.Done():
		case <-release:
		}
		return docker.Container{}, context.DeadlineExceeded
	}
	a.s.listHubs = func(context.Context) ([]docker.Container, error) { return nil, context.DeadlineExceeded }
	a.s.stepTimeout = 50 * time.Millisecond
	a.s.sweep(ctx)
	if asked.Load() == 0 {
		t.Fatal("the sweep did not ask Docker")
	}
	jobsAfter, _ := a.st.ListJobs(ctx, store.JobFilter{})
	if len(jobsAfter) != len(jobsBefore) || a.s.applying() {
		t.Fatal("a sweep without an answer from Docker started a repair")
	}
	if n := a.s.sessions.count(); n != 1 {
		t.Fatalf("%d sessions", n)
	}
	if st := a.status(t); st.State != apitypes.InspectorStateUnknown {
		t.Fatalf("state %+v", st)
	}
	if v := a.s.hubNow(); v.port != 0 || v.container == "" {
		t.Fatalf("hub %+v: the port must be unverified and the container kept", v)
	}
	if c := get(); c != http.StatusServiceUnavailable {
		t.Fatalf("unverified port used: %d", c)
	}
	// Docker answers again: the hub is used again.
	close(release)
	a.s.inspectHub, a.s.listHubs = inspect, list
	if st := a.status(t); st.State != apitypes.InspectorStateRunning {
		t.Fatalf("after Docker answered: %+v", st)
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("after Docker answered: %d", c)
	}
	if hubs, _ := a.s.cfg.Docker.InspectorHubs(ctx); len(hubs) != 1 || len(a.f.CallsTo("create")) != 1 {
		t.Fatal("the hub was replaced")
	}
	// A step whose context is done does nothing.
	done, cancel := context.WithCancel(ctx)
	cancel()
	var late atomic.Int32
	a.s.inspectHub = func(ctx context.Context, id string) (docker.Container, error) {
		late.Add(1)
		return inspect(ctx, id)
	}
	a.s.mu.Lock()
	a.s.hubVerified = time.Time{}
	a.s.mu.Unlock()
	if a.s.needsRepair(done) {
		t.Fatal("repair from a step whose context is done")
	}
	a.s.refreshHub(done, 0)
	a.s.checkHub(done)
	if p := a.s.hubNow().port; p == 0 || late.Load() != 0 {
		t.Fatal("a step whose context is done asked Docker or changed the hub")
	}
}

// Every Docker error is "no answer": Status, the repair check and a login
// leave the port unverified and start no repair. A check that started
// before a later change of the hub cannot undo it.
func TestHubObservations(t *testing.T) {
	ctx := context.Background()
	t.Run("Status", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.s.listHubs = func(context.Context) ([]docker.Container, error) { return nil, errors.New("docker: broken pipe") }
		e.s.mu.Lock()
		e.s.hubVerified = e.clock.Now() // confirmed just now
		e.s.mu.Unlock()
		if st, err := e.s.Status(ctx); err != nil || st.State != apitypes.InspectorStateUnknown {
			t.Fatalf("%+v %v", st, err)
		}
		if v := e.s.hubNow(); v.port != 0 || v.container != testHubID {
			t.Fatalf("hub %+v after a Docker error", v)
		}
	})
	t.Run("repair check", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.hub.set(func(h *fakeHub) { h.inspectErr = docker.ErrUnavailable })
		if e.s.needsRepair(ctx) {
			t.Fatal("repair after a Docker error")
		}
		if v := e.s.hubNow(); v.port != 0 || v.container != testHubID {
			t.Fatalf("hub %+v after a Docker error", v)
		}
	})
	t.Run("older check", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.dockerList()
		gate := make(chan struct{})
		e.hub.set(func(h *fakeHub) { h.inspectGate = gate })
		done := make(chan struct{})
		go func() {
			e.s.refreshHub(ctx, 0) // started first, answered last: "running"
			close(done)
		}()
		waitFor(t, "the refresh to start", func() bool {
			e.s.hubFlight.mu.Lock()
			defer e.s.hubFlight.mu.Unlock()
			return e.s.hubFlight.done != nil
		})
		// A newer check: Docker does not answer.
		e.s.listHubs = func(context.Context) ([]docker.Container, error) { return nil, docker.ErrUnavailable }
		if st, _ := e.s.Status(ctx); st.State != apitypes.InspectorStateUnknown {
			t.Fatalf("%+v", st)
		}
		e.hub.set(func(h *fakeHub) { h.inspectGate = nil })
		close(gate)
		<-done
		if p := e.s.hubNow().port; p != 0 {
			t.Fatalf("an older check restored port %d", p)
		}
	})
}

// A request replayed with a renewed hub session asks for a fresh Docker
// confirmation itself; the password login goes out on a new connection
// every time.
func TestSecretSends(t *testing.T) {
	ctx := context.Background()
	t.Run("replay", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		e.clock.Advance(reloginInterval + time.Second)
		e.hub.restart() // the hub lost every session: the request is replayed
		e.s.hooks.beforeHubLogin = func() {
			// Docker's confirmation for the login ages and the container
			// goes before the replay.
			e.clock.Advance(hubCheckInterval)
			e.hub.set(func(h *fakeHub) { h.gone = true })
		}
		if r := e.do("GET", "/api/targets", key, nil, nil); r.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("replay: %d", r.StatusCode)
		}
		n := 0
		for _, p := range e.hub.paths() {
			if p == "/api/targets" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the hub got %d target requests; the replay must not go out", n)
		}
	})
	t.Run("login connection", func(t *testing.T) {
		e := newTestEnv(t, nil)
		for i := 0; i < 2; i++ {
			// Another request leaves a connection in the pool first.
			if err := e.s.hub.ready(ctx, e.hub.port()); err != nil {
				t.Fatal(err)
			}
			if _, err := e.s.hubLogin(ctx, e.s.hubNow()); err != nil {
				t.Fatal(err)
			}
		}
		e.hub.mu.Lock()
		defer e.hub.mu.Unlock()
		used := map[string]string{}
		for _, r := range e.hub.requests {
			if prev, ok := used[r.RemoteAddr]; ok && (r.Path == "/auth/login" || prev == "/auth/login") {
				t.Fatalf("the login shared a connection with %s", prev)
			}
			used[r.RemoteAddr] = r.Path
		}
	})
}

// The apply's confirmation is as old as Docker's answer, not as the step
// that publishes the hub.
func TestApplyConfirmationTime(t *testing.T) {
	a := newApplyEnv(t, true)
	clock := &fakeClock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	a.s.cfg.Now = clock.Now
	a.s.hooks.beforePublish = func() { clock.Advance(hubCheckInterval) }
	a.enable(t)
	if a.s.confirmedRecently(a.s.hubNow()) {
		t.Fatal("the confirmation was stamped when the hub was published")
	}
}

// Status's own Docker answer confirms the hub, so the target list needs no
// second Docker call; a refresh skips Docker while a confirmation is
// fresh; a slow Docker check leaves the request to the hub its whole
// budget.
func TestStatusConfirms(t *testing.T) {
	ctx := context.Background()
	t.Run("Status", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.dockerList()
		sid, err := e.s.hubLogin(ctx, e.s.hubNow())
		if err != nil {
			t.Fatal(err)
		}
		e.s.statusMu.Lock()
		e.s.statusSID = sid
		e.s.statusMu.Unlock()
		e.clock.Advance(hubCheckInterval) // the login's confirmation is old
		inspects := e.hub.inspectCount()
		st, err := e.s.Status(ctx)
		if err != nil || st.State != apitypes.InspectorStateRunning || st.Targets == nil {
			t.Fatalf("%+v %v", st, err)
		}
		// One call: Status's own listing (dockerList reads the same fake).
		if n := e.hub.inspectCount() - inspects; n != 1 {
			t.Fatalf("%d Docker calls; the target list asked again after Status's own", n)
		}
	})
	t.Run("refresh", func(t *testing.T) {
		e := newTestEnv(t, nil)
		if !e.s.freshHub(ctx, e.s.hubNow()) {
			t.Fatal("not confirmed")
		}
		inspects := e.hub.inspectCount()
		e.s.refreshHub(ctx, 0)
		if n := e.hub.inspectCount() - inspects; n != 0 {
			t.Fatalf("%d Docker calls with a fresh confirmation", n)
		}
	})
	t.Run("slow Docker", func(t *testing.T) {
		e := newTestEnv(t, nil)
		sid, err := e.s.hubLogin(ctx, e.s.hubNow())
		if err != nil {
			t.Fatal(err)
		}
		e.s.statusMu.Lock()
		e.s.statusSID = sid
		e.s.statusMu.Unlock()
		e.clock.Advance(hubCheckInterval)
		inspect := e.s.inspectHub
		e.s.inspectHub = func(ctx context.Context, id string) (docker.Container, error) {
			time.Sleep(targetsTimeout + 200*time.Millisecond)
			return inspect(ctx, id)
		}
		if _, ok := e.s.hubTargets(ctx); !ok {
			t.Fatal("a slow Docker check used up the budget of the request to the hub")
		}
	})
}

// A Docker failure while an apply waits for a kept hub fails the job with
// the Docker message and does not mark the hub as not answering; a
// readiness result counts only for the container, port and apply it was
// started for.
func TestHubFaultsOnly(t *testing.T) {
	ctx := context.Background()
	t.Run("apply", func(t *testing.T) {
		a := newApplyEnv(t, true)
		a.enable(t)
		inspect := a.s.inspectHub
		a.s.inspectHub = func(context.Context, string) (docker.Container, error) {
			return docker.Container{}, docker.ErrUnavailable
		}
		j, log := a.update(t, apitypes.UpdateInspectorRequest{})
		if j.Status != store.JobError || j.Detail != "Docker is unreachable" {
			t.Fatalf("%+v\n%s", j, log)
		}
		a.s.mu.Lock()
		sick := a.s.hubSick
		a.s.mu.Unlock()
		if sick != "" {
			t.Fatal("a Docker failure marked the hub as not answering")
		}
		a.s.inspectHub = inspect
		a.enable(t)
		if n := len(a.f.CallsTo("create")); n != 1 {
			t.Fatalf("the hub was replaced (%d creates)", n)
		}
	})
	t.Run("readiness", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.hub.set(func(h *fakeHub) { h.readyStatus = http.StatusServiceUnavailable })
		// An apply publishes the same container and port again while each
		// check runs.
		e.s.hooks.afterReadyCheck = func() { e.s.setHub(testHubID, e.hub.port()) }
		for i := 0; i < hubSickAfter; i++ {
			e.s.checkHub(ctx)
		}
		e.s.mu.Lock()
		sick, failures := e.s.hubSick, e.s.hubFailures
		e.s.mu.Unlock()
		if sick != "" || failures != 0 {
			t.Fatalf("results from before the apply counted: sick %q, %d failures", sick, failures)
		}
	})
}

// A repair job that fails before its body runs counts as an attempt: the
// next one waits like after a failed attempt (a removal cleanupRetry, a
// start RepairInterval).
func TestRepairStartCounts(t *testing.T) {
	ctx := context.Background()
	for _, on := range []bool{false, true} {
		a := newApplyEnv(t, true)
		a.enable(t)
		bad := jobs.New(a.st, filepath.Join(t.TempDir(), "missing", "logs"), jobs.Options{})
		t.Cleanup(func() { <-bad.Stop() })
		a.s.cfg.Jobs = bad
		if !on {
			if err := a.s.storeEnabled(ctx, false); err != nil {
				t.Fatal(err)
			}
		} else {
			a.s.closeListener() // not serving: the repair is due
		}
		if !a.s.needsRepair(ctx) {
			t.Fatalf("on %v: no repair due", on)
		}
		a.s.startRepair(ctx)
		if a.s.needsRepair(ctx) {
			t.Fatalf("on %v: a repair that could not run is started again at once", on)
		}
	}
}

// A look for a leftover hub that failed does not count: the next sweep
// looks again.
func TestStrayLookFailed(t *testing.T) {
	ctx := context.Background()
	a := newApplyEnv(t, true)
	var looks atomic.Int32
	a.s.listHubs = func(context.Context) ([]docker.Container, error) {
		looks.Add(1)
		return nil, docker.ErrUnavailable
	}
	for i := 0; i < 2; i++ {
		if a.s.needsRepair(ctx) {
			t.Fatal("repair without a hub")
		}
	}
	if n := looks.Load(); n != 2 {
		t.Fatalf("%d looks; a failed look must not count", n)
	}
	a.s.listHubs = func(context.Context) ([]docker.Container, error) {
		looks.Add(1)
		return nil, nil
	}
	a.s.needsRepair(ctx)
	a.s.needsRepair(ctx)
	if n := looks.Load(); n != 3 {
		t.Fatalf("%d looks; a completed look counts for RepairInterval", n)
	}
}
