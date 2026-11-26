package servers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
)

func intp(v int) *int { return &v }

func TestProfileSeconds(t *testing.T) {
	for _, tc := range []struct {
		kind    apitypes.ProfileKind
		seconds *int
		want    int
		field   string
	}{
		{kind: apitypes.ProfileKindCPU, want: ProfileCPUDefaultSeconds},
		{kind: apitypes.ProfileKindCPU, seconds: intp(60), want: 60},
		{kind: apitypes.ProfileKindCPU, seconds: intp(61), field: "seconds"},
		{kind: apitypes.ProfileKindCPU, seconds: intp(0), field: "seconds"},
		{kind: apitypes.ProfileKindTrace, want: ProfileTraceDefaultSeconds},
		{kind: apitypes.ProfileKindTrace, seconds: intp(16), field: "seconds"},
		{kind: apitypes.ProfileKindHeap},
		{kind: apitypes.ProfileKindGoroutine, seconds: intp(5), field: "seconds"},
		{kind: "block", field: "kind"},
		{kind: "", field: "kind"},
	} {
		got, err := ProfileSeconds(tc.kind, tc.seconds)
		var fe *FieldError
		switch {
		case tc.field != "" && (!errors.As(err, &fe) || fe.Field != tc.field):
			t.Errorf("%s %v: %v, want a %s field error", tc.kind, tc.seconds, err, tc.field)
		case tc.field == "" && (err != nil || got != tc.want):
			t.Errorf("%s %v: %d %v, want %d", tc.kind, tc.seconds, got, err, tc.want)
		}
	}
}

// A server profiles by default: its container gets GOLEM_ADMIN_PROFILING
// and the label, and Profile relays the game's file under a dated name.
func TestProfileRelaysTheGamesFile(t *testing.T) {
	fx := newFixture(t, Config{})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID})
	if err != nil || !sv.Profiling {
		t.Fatalf("%+v %v", sv, err)
	}
	if _, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindHeap, nil, time.Now()); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("before a start: %v", err)
	}
	if log, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	row, _ := fx.st.GetServer(ctx, sv.ID)
	ct, _ := fx.f.Container(row.ContainerID)
	if ct.Env[EnvAdminProfiling] != "1" || ct.Labels[docker.LabelProfiling] != docker.ManagedValue {
		t.Fatalf("env %q label %q", ct.Env[EnvAdminProfiling], ct.Labels[docker.LabelProfiling])
	}
	view, err := fx.svc.Get(ctx, sv.ID)
	if err != nil || !view.ProfilingActive {
		t.Fatalf("%+v %v", view, err)
	}
	want := []byte{0x1f, 0x8b, 'c', 'p', 'u'}
	fx.admin.Set(func(s *gameadmintest.Server) { s.ProfileBody = want })
	at := time.Date(2026, 11, 26, 17, 4, 5, 0, time.FixedZone("x", 3600))
	p, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindCPU, intp(2), at)
	if err != nil || !bytes.Equal(p.Data, want) || p.Filename != "arena-cpu-20261126T160405Z.pb.gz" {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindTrace, nil, at); err != nil || !strings.HasSuffix(p.Filename, "-trace-20261126T160405Z.trace") {
		t.Fatalf("%+v %v", p, err)
	}
	_, paths := fx.admin.Requests()
	if got := strings.Join(paths[len(paths)-2:], " "); got != "/profile/cpu?seconds=2 /profile/trace?seconds=5" {
		t.Fatalf("paths %q", got)
	}
	token := ct.Env[EnvAdminToken]
	if strings.Contains(fx.logs.String(), token) {
		t.Fatal("admin token in the daemon log")
	}
}

func TestProfileRefusals(t *testing.T) {
	fx := newFixture(t, Config{})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	off := false
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID, Profiling: &off})
	if err != nil || sv.Profiling {
		t.Fatalf("%+v %v", sv, err)
	}
	if log, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	row, _ := fx.st.GetServer(ctx, sv.ID)
	ct, _ := fx.f.Container(row.ContainerID)
	if _, ok := ct.Env[EnvAdminProfiling]; ok || ct.Labels[docker.LabelProfiling] != "" {
		t.Fatalf("a server with the switch off got profiling: %v", ct.Labels)
	}
	if _, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindHeap, nil, time.Now()); !errors.Is(err, ErrProfilingOff) {
		t.Fatalf("switch off: %v", err)
	}
	// Turning the switch on applies from the next start: until then the
	// running container lacks it.
	on := true
	view, err := fx.svc.Update(ctx, sv.ID, apitypes.UpdateServerRequest{Profiling: &on})
	if err != nil || !view.Profiling || view.ProfilingActive || !view.RestartRequired {
		t.Fatalf("%+v %v", view, err)
	}
	if _, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindHeap, nil, time.Now()); !errors.Is(err, ErrProfilingOff) {
		t.Fatalf("before the restart: %v", err)
	}
	if _, err := fx.svc.Profile(ctx, sv.ID, "block", nil, time.Now()); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := fx.svc.Profile(ctx, 999, apitypes.ProfileKindHeap, nil, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown server: %v", err)
	}
}

func TestProfileMapsTheGamesAnswers(t *testing.T) {
	fx := newFixture(t, Config{})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "arena", BuildID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if log, err := startJob(t, fx, sv.ID); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	for status, want := range map[int]error{
		http.StatusNotFound:           ErrProfilingOff,
		http.StatusConflict:           ErrProfileInProgress,
		http.StatusServiceUnavailable: ErrAdminUnavailable,
		http.StatusBadGateway:         ErrAdminUnavailable,
	} {
		fx.admin.Set(func(s *gameadmintest.Server) { s.ProfileStatus = status })
		if _, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindHeap, nil, time.Now()); !errors.Is(err, want) {
			t.Errorf("game status %d: %v, want %v", status, err, want)
		}
	}
	fx.admin.Set(func(s *gameadmintest.Server) { s.ProfileStatus = 0 })

	// One capture per server at a time.
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	fx.admin.Set(func(s *gameadmintest.Server) {
		s.OnProfile = func() {
			started <- struct{}{}
			<-release
		}
	})
	first := make(chan error, 1)
	go func() {
		_, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindCPU, intp(1), time.Now())
		first <- err
	}()
	<-started
	if _, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindHeap, nil, time.Now()); !errors.Is(err, ErrProfileInProgress) {
		t.Fatalf("concurrent capture: %v", err)
	}
	fx.admin.Set(func(s *gameadmintest.Server) { s.OnProfile = nil })
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if _, err := fx.svc.Profile(ctx, sv.ID, apitypes.ProfileKindHeap, nil, time.Now()); err != nil {
		t.Fatalf("after the first capture: %v", err)
	}
}

// Existing servers profile after the upgrade, and an export keeps the
// switch; an import without it turns it on.
func TestProfilingExportImport(t *testing.T) {
	fx := newFixture(t, Config{})
	ctx := context.Background()
	b := fx.build(t, 1, commitA)
	off := false
	if _, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "quiet", BuildID: b.ID, Profiling: &off}); err != nil {
		t.Fatal(err)
	}
	exp, err := fx.svc.Export(ctx)
	if err != nil || len(exp.Servers) != 1 || exp.Servers[0].Profiling == nil || *exp.Servers[0].Profiling {
		t.Fatalf("%+v %v", exp, err)
	}
	specs := []apitypes.ServerSpec{
		{Name: "kept", Project: "game", DesiredState: apitypes.DesiredStateStopped, Profiling: &off},
		{Name: "default", Project: "game", DesiredState: apitypes.DesiredStateStopped},
	}
	if _, err := fx.svc.Import(ctx, apitypes.ServerExport{FormatVersion: ExportFormatVersion, Servers: specs}); err != nil {
		t.Fatal(err)
	}
	rows, err := fx.st.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Name] = r.Profiling
	}
	if got["kept"] || !got["default"] {
		t.Fatalf("%v", got)
	}
}
