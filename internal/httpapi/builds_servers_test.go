package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// The build pipeline and the servers agree: an image produced by a build
// (labelled by docker build from the command line, found by BuiltImage) is
// exactly what a server start verifies (BuildImage) and runs; the start is
// health-gated and the runtime view and the build's schema fingerprint
// come from the game.
func TestBuildToServer(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	for _, containerd := range []bool{false, true} {
		b.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
			return dockertest.BuildOutcome{AssignPort: b.gameAdmin.Port, ContainerdStore: containerd}
		})
		res := b.startBuild(b.devTok, p.ID, nil)
		if j := b.waitJob(res.Job); j.Status != store.JobSuccess {
			t.Fatalf("build: %s %s", j.Detail, b.jobLog(res.Job.ID))
		}
		built := b.getBuild(res.Build.ID)
		if built.Status != apitypes.BuildStatusSuccess || built.ImageID == "" || built.EngineVersion == "" {
			t.Fatalf("%+v", built)
		}
		name := fmt.Sprintf("arena-%v", containerd)
		sv := b.createServer(b.devTok, apitypes.CreateServerRequest{Name: name, BuildID: built.ID})
		if sv.BuildID == nil || *sv.BuildID != built.ID || sv.Image != built.ImageID || sv.Commit != built.Commit {
			t.Fatalf("%+v", sv)
		}
		if j := b.action(b.devTok, sv.ID, "start"); j.Status != store.JobSuccess {
			t.Fatalf("start: %s\n%s", j.Detail, b.jobLog(j.ID))
		}
		run := b.fake.CallsTo("run")
		last := run[len(run)-1]
		if !strings.Contains(strings.Join(last.Args, " "), built.ImageID) {
			t.Fatalf("server not run from the build's image: %v", last.Args)
		}
		rr := b.call("GET", fmt.Sprintf("/api/v1/servers/%d/runtime", sv.ID), b.devTok, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("runtime: %d %s", rr.Code, rr.Body.String())
		}
		if rt := decode[apitypes.ServerRuntime](t, rr); !rt.Ready {
			t.Fatalf("%+v", rt)
		}
		if got := b.getBuild(built.ID); got.SchemaFingerprint == "" || len(got.ServerIDs) != 1 || got.ServerIDs[0] != sv.ID {
			t.Fatalf("%+v", got)
		}
		ct, ok := b.fake.Container(b.getServerContainer(sv.ID))
		if !ok || ct.Labels[docker.LabelBuildID] != fmt.Sprint(built.ID) || ct.Labels[docker.LabelEngineVersion] != built.EngineVersion {
			t.Fatalf("container labels %+v", ct.Labels)
		}
		if j := b.action(b.devTok, sv.ID, "stop"); j.Status != store.JobSuccess {
			t.Fatalf("stop: %s", j.Detail)
		}
	}
}

func (b *buildHarness) getServerContainer(id int64) string {
	b.t.Helper()
	sv, err := b.st.GetServer(b.t.Context(), id)
	if err != nil || sv.ContainerID == "" {
		b.t.Fatalf("no container recorded: %v", err)
	}
	return sv.ContainerID
}
