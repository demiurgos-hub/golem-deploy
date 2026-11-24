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

// One rule for build images everywhere: an image is in use while a
// successful build records it or any container (also a stopped server's,
// which keeps its container) was created from it. A server switched to
// another build keeps the old build's container until it is started again,
// so the old build, its image and its project cannot be deleted until then.
func TestBuildImageInUseByStoppedContainer(t *testing.T) {
	b := newBuildHarness(t)
	b.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		return dockertest.BuildOutcome{AssignPort: b.gameAdmin.Port}
	})
	built := func(p store.Project) apitypes.Build {
		res := b.startBuild(b.devTok, p.ID, nil)
		if j := b.waitJob(res.Job); j.Status != store.JobSuccess {
			t.Fatalf("build: %s", j.Detail)
		}
		return b.getBuild(res.Build.ID)
	}
	pa, pb := b.project("alpha"), b.project("beta")
	a1, b1 := built(pa), built(pb)
	sv := b.createServer(b.devTok, apitypes.CreateServerRequest{Name: "s", BuildID: a1.ID})
	if j := b.action(b.devTok, sv.ID, "start"); j.Status != store.JobSuccess {
		t.Fatalf("start: %s", j.Detail)
	}
	if rr := b.call("PATCH", fmt.Sprintf("/api/v1/servers/%d", sv.ID), b.devTok, apitypes.UpdateServerRequest{BuildID: &b1.ID}); rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	if j := b.action(b.devTok, sv.ID, "stop"); j.Status != store.JobSuccess {
		t.Fatalf("stop: %s", j.Detail)
	}
	// No server is configured with a1 any more, but the stopped container
	// of the server still is a1's image.
	// It must still prevent deletion after the first page of project builds.
	for range 501 {
		row, err := b.st.CreateBuild(t.Context(), store.NewBuild{ProjectID: pa.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.st.FinishBuild(t.Context(), row.ID, store.BuildResult{Status: store.BuildStatusError}); err != nil {
			t.Fatal(err)
		}
	}
	images := decode[apitypes.ImageList](t, b.call("GET", "/api/v1/images", b.devTok, nil))
	for _, img := range images.Items {
		if img.ID == a1.ImageID && (!img.InUse || len(img.BuildIDs) != 1 || len(img.ServerIDs) != 0) {
			t.Fatalf("%+v", img)
		}
	}
	want := fmt.Sprintf("the container of server %d still uses", sv.ID)
	e := errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", a1.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, want) {
		t.Fatal(e.Message)
	}
	e = errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/projects/%d", pa.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, want) {
		t.Fatal(e.Message)
	}
	// DELETE /images never removes an image a build records.
	e = errBody(t, b.call("DELETE", "/api/v1/images/"+a1.ImageID, b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, fmt.Sprintf("belongs to build %d", a1.ID)) {
		t.Fatal(e.Message)
	}
	if _, err := b.st.GetProject(b.t.Context(), pa.ID); err != nil || !b.fake.HasImage(a1.ImageID) {
		t.Fatalf("project or image removed: %v", err)
	}
	// Starting the server again recreates its container from b1: alpha
	// can go, with its build and image.
	if j := b.action(b.devTok, sv.ID, "start"); j.Status != store.JobSuccess {
		t.Fatalf("start: %s", j.Detail)
	}
	if rr := b.call("DELETE", fmt.Sprintf("/api/v1/projects/%d", pa.ID), b.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete project: %d %s", rr.Code, rr.Body.String())
	}
	if b.fake.HasImage(a1.ImageID) {
		t.Fatal("the deleted project's image is still there")
	}
	if !b.fake.HasImage(b1.ImageID) {
		t.Fatal("the running server's image was removed")
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

// Rebuilding a commit must not take the older build's image away from the
// servers running it: every build has its own tag, because the containerd
// image store deletes an image whose last tag moves to another image.
func TestRebuildKeepsOlderBuildImage(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	b.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		return dockertest.BuildOutcome{AssignPort: b.gameAdmin.Port, ContainerdStore: true}
	})
	build := func() apitypes.Build {
		res := b.startBuild(b.devTok, p.ID, nil)
		if j := b.waitJob(res.Job); j.Status != store.JobSuccess {
			t.Fatalf("build: %s", j.Detail)
		}
		return b.getBuild(res.Build.ID)
	}
	first := build()
	sv := b.createServer(b.devTok, apitypes.CreateServerRequest{Name: "s", BuildID: first.ID})
	if j := b.action(b.devTok, sv.ID, "start"); j.Status != store.JobSuccess {
		t.Fatalf("start: %s", j.Detail)
	}
	second := build()
	if second.Commit != first.Commit || second.ImageTag == first.ImageTag {
		t.Fatalf("tags %q and %q", first.ImageTag, second.ImageTag)
	}
	if !b.fake.HasImage(first.ImageID) {
		t.Fatal("the first build's image was deleted by the rebuild")
	}
	if j := b.action(b.devTok, sv.ID, "restart"); j.Status != store.JobSuccess {
		t.Fatalf("restart on the first build: %s", j.Detail)
	}
}
