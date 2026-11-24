package servers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
)

type testDockerRunner func(context.Context, string, docker.Cmd) error

func (f testDockerRunner) Run(ctx context.Context, path string, cmd docker.Cmd) error {
	return f(ctx, path, cmd)
}

func (fx *fixture) interceptDocker(t *testing.T, f testDockerRunner) {
	t.Helper()
	fx.svc.docker = docker.New(docker.Options{
		Runner: f, InstanceID: dockertest.InstanceID, TempDir: t.TempDir(), Logger: fx.svc.logger,
	})
}

func TestInterruptedCreateRecoveredFromRecordedIntent(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, Config{})
	b := fx.build(t, 1, commitA)
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "lost-reply", BuildID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	lost := true
	fx.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		switch cmd.Args[0] {
		case "create":
			row, err := fx.st.GetServer(ctx, sv.ID)
			if err != nil || row.ContainerID != "" || row.PendingLaunch == "" ||
				!slices.Contains(cmd.Args, docker.LabelLaunchID+"="+row.PendingLaunch) {
				t.Fatalf("create issued without a durable intent: %v", err)
			}
			if lost {
				lost = false
				cmd.Stdout = io.Discard
				if err := fx.f.Run(ctx, path, cmd); err != nil {
					return err
				}
				return context.DeadlineExceeded
			}
		case "start":
			row, err := fx.st.GetServer(ctx, sv.ID)
			if err != nil || row.ContainerID == "" || row.PendingLaunch != "" ||
				row.StartComplete || row.ContainerID != cmd.Args[len(cmd.Args)-1] || len(row.AdminToken) == 0 {
				t.Fatalf("start issued before recording the container and token: %v", err)
			}
		}
		return fx.f.Run(ctx, path, cmd)
	})
	if err := fx.svc.Start(ctx, sv.ID, io.Discard); !errors.Is(err, docker.ErrUnavailable) {
		t.Fatalf("lost create reply: %v", err)
	}
	left, ok := fx.f.Container("golem-deploy-lost-reply")
	if !ok || left.State != "created" {
		t.Fatal("the interrupted create should leave only a stopped container")
	}
	row, err := fx.st.GetServer(ctx, sv.ID)
	if err != nil || row.PendingLaunch == "" {
		t.Fatalf("missing persisted launch identity: %v", err)
	}
	foreign := fx.f.AddContainer(dockertest.Container{
		Name: "foreign-with-copied-labels", Image: b.ImageID, Labels: left.Labels,
	})
	var queued []int64
	if err := fx.svc.Reconcile(ctx, func(id int64) error {
		queued = append(queued, id)
		return fx.svc.Recover(ctx, id, io.Discard)
	}); err != nil {
		t.Fatal(err)
	}
	row, err = fx.st.GetServer(ctx, sv.ID)
	if !slices.Equal(queued, []int64{sv.ID}) || err != nil || !row.StartComplete || row.ContainerID == left.ID {
		t.Fatalf("interrupted launch did not recover: jobs=%v completed=%t err=%v", queued, row.StartComplete, err)
	}
	if _, found := fx.f.Container(left.ID); found {
		t.Fatal("interrupted create was left behind")
	}
	if _, found := fx.f.Container(foreign); !found {
		t.Fatal("recovery removed an unrelated container")
	}
}

func TestStartupReverifiesImageReadinessAndCompletion(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, Config{})
	b := fx.build(t, 1, commitA)
	sv, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "recovery", BuildID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.Start(ctx, sv.ID, io.Discard); err != nil {
		t.Fatal(err)
	}
	row, _ := fx.st.GetServer(ctx, sv.ID)
	_, before := fx.admin.Requests()
	if err := fx.svc.Recover(ctx, sv.ID, io.Discard); err != nil {
		t.Fatal(err)
	}
	_, after := fx.admin.Requests()
	same, _ := fx.st.GetServer(ctx, sv.ID)
	if same.ContainerID != row.ContainerID || !slices.Equal(after[len(before):], []string{"/ready"}) {
		t.Fatalf("completed start was not re-verified in place: %v", after)
	}
	for _, fault := range []string{"incomplete start", "not ready", "wrong build image"} {
		t.Run(fault, func(t *testing.T) {
			before, _ := fx.st.GetServer(ctx, sv.ID)
			switch fault {
			case "incomplete start":
				// Crash after recording and starting, before the completed
				// marker was persisted.
				if err := fx.st.SetServerContainer(ctx, sv.ID, before.ContainerID, 0, before.AdminToken); err != nil {
					t.Fatal(err)
				}
			case "not ready":
				fx.admin.Set(func(s *gameadmintest.Server) { s.NotReadyFor = 1 })
			case "wrong build image":
				next := fx.build(t, 2, commitB)
				if _, err := fx.svc.Update(ctx, sv.ID, apitypes.UpdateServerRequest{BuildID: &next.ID}); err != nil {
					t.Fatal(err)
				}
			}
			if err := fx.svc.Recover(ctx, sv.ID, io.Discard); err != nil {
				t.Fatal(err)
			}
			after, _ := fx.st.GetServer(ctx, sv.ID)
			ct, found := fx.f.Container(after.ContainerID)
			if !found || after.ContainerID == before.ContainerID || !after.StartComplete || ct.RestartPolicy != docker.RestartPolicy {
				t.Fatalf("startup recovery did not repeat the health-gated start")
			}
		})
	}
}

func TestReadinessDoesNotUseStoredPort(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, Config{StartTimeout: 50 * time.Millisecond})
	b := fx.build(t, 1, commitA)
	created, err := fx.svc.Create(ctx, apitypes.CreateServerRequest{Name: "unbound", BuildID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	cid := fx.f.AddContainer(dockertest.Container{Image: b.ImageID,
		Labels: dockertest.OurLabels(docker.LabelServerID, itoa(created.ID))})
	token, sealed, err := fx.svc.newAdminToken(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.SetServerContainer(ctx, created.ID, cid, fx.admin.Port, sealed); err != nil {
		t.Fatal(err)
	}
	fx.admin.Set(func(s *gameadmintest.Server) { s.ExpectToken = func() string { return token } })
	sv, _ := fx.st.GetServer(ctx, created.ID)
	if _, err := fx.svc.Runtime(ctx, sv.ID, time.Now()); !errors.Is(err, ErrAdminUnavailable) {
		t.Fatalf("runtime without a current binding: %v", err)
	}
	if err := fx.svc.waitReady(ctx, sv, b, nil, token, io.Discard); err == nil {
		t.Fatal("an unbound container was reported ready")
	}
	if _, paths := fx.admin.Requests(); len(paths) != 0 {
		t.Fatalf("connected to a previously recorded port: %v", paths)
	}
}

func TestReadinessDeadlineBoundsInspectAndProbe(t *testing.T) {
	for _, slow := range []string{"inspect", "probe"} {
		t.Run(slow, func(t *testing.T) {
			fx := newFixture(t, Config{StartTimeout: 80 * time.Millisecond})
			b := fx.build(t, 1, commitA)
			if slow == "inspect" {
				afterStart := false
				fx.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
					if cmd.Args[0] == "start" {
						afterStart = true
					}
					if afterStart && len(cmd.Args) >= 2 && slices.Equal(cmd.Args[:2], []string{"container", "inspect"}) {
						afterStart = false
						<-ctx.Done()
						return ctx.Err()
					}
					return fx.f.Run(ctx, path, cmd)
				})
			} else {
				h := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
				defer h.Close()
				fx.f.AddImage(dockertest.Image{ID: b.ImageID, AssignPort: h.Listener.Addr().(*net.TCPAddr).Port,
					Labels: dockertest.BuildLabels(buildRef(b))})
			}
			sv, err := fx.svc.Create(context.Background(), apitypes.CreateServerRequest{Name: "slow", BuildID: b.ID})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err = startJob(t, fx, sv.ID)
			if !strings.Contains(failDetail(err), "within 80ms") || time.Since(start) > time.Second {
				t.Fatalf("%s overran readiness deadline: %v after %s", slow, err, time.Since(start))
			}
		})
	}
}
