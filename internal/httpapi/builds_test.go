package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// buildHarness is a docker harness with the build service, a fake git and
// a repository directory wired in.
type buildHarness struct {
	*dockerHarness
	git     *fakeGit
	repoDir string
	tmpDir  string
	devSs   string
}

func newBuildHarness(t *testing.T) *buildHarness {
	t.Helper()
	d := newDockerHarness(t)
	dir := t.TempDir()
	bh := &buildHarness{dockerHarness: d, git: newFakeGit(), repoDir: filepath.Join(dir, "repositories"), tmpDir: filepath.Join(dir, "tmp")}
	d.srv.proj.git = bh.git
	d.srv.proj.repoDir = bh.repoDir
	d.srv.proj.tmpDir = bh.tmpDir
	d.srv.builds = build.New(build.Options{Store: d.st, Docker: d.dc, InstanceID: dockertest.InstanceID, RepoDir: bh.repoDir,
		TempDir: bh.tmpDir, StaticDir: d.static.Dir(), Timeout: time.Minute,
		HeadCommit: func(context.Context, string) (string, error) { return strings.Repeat("a", 40), nil }})
	bh.devSs = d.session(d.dev)
	return bh
}

// project stores a project whose checkout is the build fixture game.
func (b *buildHarness) project(name string) store.Project {
	b.t.Helper()
	p, err := b.st.CreateProject(context.Background(), store.Project{Name: name, RepoURL: "https://git.example.com/" + name + ".git",
		Branch: "main", LastCommit: strings.Repeat("a", 40), Status: "idle"}, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	dst := filepath.Join(b.repoDir, fmt.Sprint(p.ID))
	src := filepath.Join("..", "build", "testdata", "game")
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, ".git"), 0o700); err != nil {
		b.t.Fatal(err)
	}
	return p
}

func (b *buildHarness) startBuild(token string, id int64, body any) apitypes.CreateBuildResponse {
	b.t.Helper()
	rr := b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", id), token, body)
	if rr.Code != http.StatusAccepted {
		b.t.Fatalf("create build: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.CreateBuildResponse](b.t, rr)
}

func (b *buildHarness) getBuild(id int64) apitypes.Build {
	b.t.Helper()
	rr := b.call("GET", fmt.Sprintf("/api/v1/builds/%d", id), b.devTok, nil)
	if rr.Code != http.StatusOK {
		b.t.Fatalf("get build: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.Build](b.t, rr)
}

func zipBytes(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte(c))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestBuildLifecycle(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	// An empty body builds the current checkout.
	res := b.startBuild(b.devTok, p.ID, nil)
	if res.Build.Status != apitypes.BuildStatusQueued || res.Build.ProjectID != p.ID || res.Job.Kind != apitypes.JobKindProjectBuild ||
		res.Job.SubjectType != apitypes.SubjectTypeProject || res.Job.SubjectID != fmt.Sprint(p.ID) || res.Job.Restricted ||
		res.Build.JobID == nil || *res.Build.JobID != res.Job.ID || res.Build.ServerIDs == nil {
		t.Fatalf("%+v", res)
	}
	if j := b.waitJob(res.Job); j.Status != store.JobSuccess {
		t.Fatalf("job %+v\n%s", j, b.jobLog(res.Job.ID))
	}
	got := b.getBuild(res.Build.ID)
	if got.Status != apitypes.BuildStatusSuccess || got.ImageTag != fmt.Sprintf("golem-deploy/arena:aaaaaaaaaaaa-b%d", got.ID) || got.EngineVersion != "v0.4.0-rc.1" ||
		got.Commit != strings.Repeat("a", 40) || got.StartedAt == nil || got.FinishedAt == nil || len(got.ServerIDs) != 0 {
		t.Fatalf("%+v", got)
	}
	// The build's output is the job log, readable by developers.
	if rr := b.call("GET", fmt.Sprintf("/api/v1/jobs/%d/log", res.Job.ID), b.devTok, nil); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), "Generated Dockerfile") || strings.Contains(rr.Body.String(), b.repoDir) {
		t.Fatalf("log %d %s", rr.Code, rr.Body.String())
	}
	// With pull, the checkout is updated first in the same job.
	res2 := b.startBuild(b.devSs, p.ID, apitypes.CreateBuildRequest{Pull: true})
	if j := b.waitJob(res2.Job); j.Status != store.JobSuccess {
		t.Fatalf("job %+v\n%s", j, b.jobLog(res2.Job.ID))
	}
	if b.git.lastCall().op != "pull" {
		t.Fatalf("git calls %+v", b.git.calls)
	}

	list := decode[apitypes.BuildList](t, b.call("GET", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), b.devTok, nil))
	if len(list.Items) != 2 || list.Items[0].ID != res2.Build.ID {
		t.Fatalf("%+v", list)
	}
	all := decode[apitypes.BuildList](t, b.call("GET", fmt.Sprintf("/api/v1/builds?project_id=%d&status=success&limit=1", p.ID), b.devTok, nil))
	if len(all.Items) != 1 || all.Items[0].ID != res2.Build.ID {
		t.Fatalf("%+v", all)
	}
	none := decode[apitypes.BuildList](t, b.call("GET", "/api/v1/builds?status=error", b.devTok, nil))
	if none.Items == nil || len(none.Items) != 0 {
		t.Fatalf("%+v", none)
	}
	for _, q := range []string{"?status=done", "?limit=0", "?limit=501", "?limit=x", "?project_id=-1", "?project_id=a"} {
		errBody(t, b.call("GET", "/api/v1/builds"+q, b.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	}
	errBody(t, b.call("GET", "/api/v1/projects/99/builds", b.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, b.call("GET", "/api/v1/builds/99", b.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, b.call("GET", "/api/v1/builds/abc", b.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)

	// Deleting is admin-only and removes the image.
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", got.ID), b.devTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", got.ID), b.devSs, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	if rr := b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", got.ID), b.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if b.fake.HasImage(got.ImageID) {
		t.Fatal("image kept")
	}
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", got.ID), b.adminSs, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

func TestBuildConflicts(t *testing.T) {
	b := newBuildHarness(t)
	ctx := context.Background()
	p := b.project("arena")

	// No checkout and no pull.
	bare, _ := b.st.CreateProject(ctx, store.Project{Name: "bare", RepoURL: "https://git.example.com/bare.git", Status: "error"}, nil)
	e := errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", bare.ID), b.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "no checkout") {
		t.Fatal(e.Message)
	}
	errBody(t, b.call("POST", "/api/v1/projects/999/builds", b.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), b.devTok, `{"pull":true,"extra":1}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)

	// A running build blocks other jobs on the project and its own deletion.
	release := make(chan struct{})
	b.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		return dockertest.BuildOutcome{Block: true, Release: release}
	})
	res := b.startBuild(b.devTok, p.ID, nil)
	errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), b.devTok, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/pull", p.ID), b.devTok, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", res.Build.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/projects/%d", p.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	close(release)
	b.waitJob(res.Job)
	b.fake.SetBuildHook(nil)

	// A build a server is configured with cannot be deleted, nor can its
	// project.
	sv, err := b.st.CreateServer(ctx, store.Server{Name: "s1", BuildID: &res.Build.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.getBuild(res.Build.ID); len(got.ServerIDs) != 1 || got.ServerIDs[0] != sv.ID {
		t.Fatalf("%+v", got)
	}
	list := decode[apitypes.BuildList](t, b.call("GET", "/api/v1/builds", b.devTok, nil))
	if len(list.Items[0].ServerIDs) != 1 {
		t.Fatalf("%+v", list)
	}
	e = errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", res.Build.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "servers are configured") {
		t.Fatal(e.Message)
	}
	e = errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/projects/%d", p.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, fmt.Sprint(sv.ID)) {
		t.Fatal(e.Message)
	}
	if _, err := b.st.GetProject(ctx, p.ID); err != nil {
		t.Fatalf("project deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.repoDir, fmt.Sprint(p.ID))); err != nil {
		t.Fatalf("checkout removed although the delete was refused: %v", err)
	}
	// A container from the image blocks deleting the build.
	_ = b.st.DeleteServer(ctx, sv.ID)
	cid := b.fake.AddContainer(dockertest.Container{Image: b.getBuild(res.Build.ID).ImageID})
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", res.Build.ID), b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	b.fake.RemoveContainer(cid)
	b.fake.SetDown(true)
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/builds/%d", res.Build.ID), b.adminSs, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	b.fake.SetDown(false)

	// Too many queued builds.
	var releases []func()
	for range build.MaxActiveBuilds {
		r, err := b.srv.builds.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	e = errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), b.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "too many builds") {
		t.Fatal(e.Message)
	}
	for _, r := range releases {
		r()
	}
	if n, _ := b.st.ListBuilds(ctx, store.BuildFilter{}); len(n) != 1 {
		t.Fatalf("refused requests left build rows: %d", len(n))
	}
}

func TestDeleteProjectRemovesBuildImages(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	res := b.startBuild(b.devTok, p.ID, nil)
	b.waitJob(res.Job)
	img := b.getBuild(res.Build.ID).ImageID
	if !b.fake.HasImage(img) {
		t.Fatal("no image")
	}
	if rr := b.call("DELETE", fmt.Sprintf("/api/v1/projects/%d", p.ID), b.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if b.fake.HasImage(img) {
		t.Fatal("image of a deleted project kept")
	}
	errBody(t, b.call("GET", fmt.Sprintf("/api/v1/builds/%d", res.Build.ID), b.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

func TestFailedBuildDetail(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	if err := os.Remove(filepath.Join(b.repoDir, fmt.Sprint(p.ID), "golem.yaml")); err != nil {
		t.Fatal(err)
	}
	res := b.startBuild(b.devTok, p.ID, nil)
	j := b.waitJob(res.Job)
	got := b.getBuild(res.Build.ID)
	if j.Status != store.JobError || got.Status != apitypes.BuildStatusError || !strings.Contains(got.Detail, "golem.yaml was not found") || j.Detail != got.Detail {
		t.Fatalf("job %+v build %+v", j, got)
	}
	if strings.Contains(got.Detail, b.repoDir) {
		t.Fatal("detail leaks a host path")
	}
}

func TestStaticAddonsAPI(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	if rr := b.upload("", map[string]string{"maps.zip": zipBytes(t, map[string]string{"level.json": "{}"}), "notes.txt": "x"}, "maps.zip", "notes.txt"); rr.Code != http.StatusCreated {
		t.Fatalf("upload %d %s", rr.Code, rr.Body.String())
	}
	path := fmt.Sprintf("/api/v1/projects/%d", p.ID)
	errBody(t, b.call("PATCH", path, b.devSs, map[string]any{"static_addons": []any{}}), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	bad := []struct {
		body  string
		field string
	}{
		{`{"static_addons":[{"name":"missing.zip","target":"."}]}`, "static_addons[0].name"},
		{`{"static_addons":[{"name":"notes.txt","target":"."}]}`, "static_addons[0].name"},
		{`{"static_addons":[{"name":"maps.zip","target":"../up"}]}`, "static_addons[0].target"},
		{`{"static_addons":[{"name":"maps.zip","target":"a\nb"}]}`, "static_addons[0].target"},
		{`{"static_addons":[{"name":"maps.zip","target":"/app"}]}`, "static_addons[0].target"},
		{`{"static_addons":[{"name":"../x.zip","target":"."}]}`, "static_addons[0].name"},
		{`{"static_addons":[{"name":"maps.zip","target":"a"},{"name":"maps.zip","target":"a/"}]}`, "static_addons[1]"},
	}
	for _, tc := range bad {
		e := errBody(t, b.call("PATCH", path, b.adminSs, tc.body), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
		if e.Field != tc.field {
			t.Errorf("%s: field %q", tc.body, e.Field)
		}
	}
	rr := b.call("PATCH", path, b.adminSs, `{"static_addons":[{"name":"maps.zip","target":"./assets/maps/"},{"name":"maps.zip","target":""}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	got := decode[apitypes.Project](t, rr)
	if len(got.StaticAddons) != 2 || got.StaticAddons[0] != (apitypes.StaticAddon{Name: "maps.zip", Target: "assets/maps"}) || got.StaticAddons[1].Target != "." {
		t.Fatalf("%+v", got.StaticAddons)
	}
	// Referenced static files cannot be deleted.
	errBody(t, b.call("DELETE", "/api/v1/static/maps.zip", b.devTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	e := errBody(t, b.call("DELETE", "/api/v1/static/maps.zip", b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "arena") {
		t.Fatal(e.Message)
	}
	if rr := b.call("DELETE", "/api/v1/static/notes.txt", b.devTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("%d", rr.Code)
	}
	// The add-on is extracted into the build.
	var staged map[string]string
	b.fake.SetBuildHook(func(c dockertest.BuildCall) dockertest.BuildOutcome {
		staged = map[string]string{}
		_ = filepath.WalkDir(c.StaticDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(c.StaticDir, path)
				data, _ := os.ReadFile(path)
				staged[filepath.ToSlash(rel)] = string(data)
			}
			return err
		})
		return dockertest.BuildOutcome{}
	})
	res := b.startBuild(b.devTok, p.ID, nil)
	if j := b.waitJob(res.Job); j.Status != store.JobSuccess {
		t.Fatalf("%+v\n%s", j, b.jobLog(res.Job.ID))
	}
	if staged["assets/maps/level.json"] != "{}" || staged["level.json"] != "{}" {
		t.Fatalf("staged %v", staged)
	}
	// Clearing the add-ons frees the file.
	if rr := b.call("PATCH", path, b.adminSs, `{"static_addons":[]}`); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"static_addons":[]`) {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if rr := b.call("DELETE", "/api/v1/static/maps.zip", b.devTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	// Projects always list static_addons (never null).
	if rr := b.call("GET", "/api/v1/projects", b.devTok, nil); !strings.Contains(rr.Body.String(), `"static_addons":[]`) {
		t.Fatal(rr.Body.String())
	}
}

func TestBuildsUnavailable(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	b.srv.builds = nil
	errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), b.devTok, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	errBody(t, b.call("DELETE", "/api/v1/builds/1", b.adminSs, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	if rr := b.call("GET", "/api/v1/builds", b.devTok, nil); rr.Code != http.StatusOK {
		t.Fatalf("%d", rr.Code)
	}
}

func TestCustomDockerfileRequiresAdminOptIn(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	path := fmt.Sprintf("/api/v1/projects/%d", p.ID)
	checkout := filepath.Join(b.repoDir, fmt.Sprint(p.ID))
	for f, content := range map[string]string{"golem-deploy.yaml": "dockerfile: Dockerfile.custom\n", "Dockerfile.custom": "FROM scratch\n"} {
		if err := os.WriteFile(filepath.Join(checkout, f), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := b.startBuild(b.devTok, p.ID, nil)
	if j := b.waitJob(res.Job); j.Status != store.JobError || !strings.Contains(j.Detail, "enable allow_custom_dockerfile") || len(b.fake.CallsTo("build")) != 0 {
		t.Fatalf("custom build without consent: %+v", j)
	}
	errBody(t, b.call("PATCH", path, b.devSs, `{"allow_custom_dockerfile":true}`), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	for _, allow := range []bool{true, false} {
		rr := b.call("PATCH", path, b.adminSs, map[string]bool{"allow_custom_dockerfile": allow})
		if rr.Code != http.StatusOK || decode[apitypes.Project](t, rr).AllowCustomDockerfile != allow {
			t.Fatalf("opt-in PATCH: %d %s", rr.Code, rr.Body.String())
		}
		res := b.startBuild(b.devTok, p.ID, nil)
		if j := b.waitJob(res.Job); (j.Status == store.JobSuccess) != allow {
			t.Fatalf("build with allow=%t: %+v", allow, j)
		}
	}
}

func TestBuildWithoutPullRejectsFailedCheckout(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	if err := b.st.SetProjectStatus(t.Context(), p.ID, "error"); err != nil {
		t.Fatal(err)
	}
	e := errBody(t, b.call("POST", fmt.Sprintf("/api/v1/projects/%d/builds", p.ID), b.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "pull it first") || len(b.fake.CallsTo("build")) != 0 {
		t.Fatalf("unsafe checkout permitted: %+v", e)
	}
}

func TestReferencedStaticFileMutationsRequireAdmin(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	data := zipBytes(t, map[string]string{"level": "original"})
	if rr := b.upload("", map[string]string{"maps.zip": data}, "maps.zip"); rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	path := fmt.Sprintf("/api/v1/projects/%d", p.ID)
	name := "maps.zip"
	if runtime.GOOS == "windows" {
		name = "MAPS.ZIP"
	}
	rr := b.call("PATCH", path, b.adminSs, fmt.Sprintf(`{"static_addons":[{"name":%q,"target":"."}]}`, name))
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	replacement := zipBytes(t, map[string]string{"level": "replacement"})
	errBody(t, b.upload("?replace=1", map[string]string{"maps.zip": replacement, "new.txt": "new"}, "maps.zip", "new.txt"), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	if rr := b.call("GET", "/api/v1/static/maps.zip", b.devTok, nil); rr.Body.String() != data {
		t.Fatal("developer overwrote a referenced file")
	}
	errBody(t, b.call("GET", "/api/v1/static/new.txt", b.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	errBody(t, b.call("DELETE", "/api/v1/static/maps.zip", b.devTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	errBody(t, b.call("DELETE", "/api/v1/static/maps.zip", b.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)

	body, ct := multipartBody(t, map[string]string{"maps.zip": replacement}, "maps.zip")
	req := httptest.NewRequest("POST", "/api/v1/static?replace=1", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+b.adminSs)
	rr = httptest.NewRecorder()
	b.srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin replacement: %d %s", rr.Code, rr.Body)
	}
}

func TestStaticReplaceCannotInterleaveWithAttach(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	data := zipBytes(t, map[string]string{"level": "original"})
	if rr := b.upload("", map[string]string{"maps.zip": data}, "maps.zip"); rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	db, err := sql.Open("sqlite", b.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Block the attach's write after its validation, while WAL still
	// allows other requests to read the project and static references.
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET branch = branch WHERE id = ?", p.ID); err != nil {
		t.Fatal(err)
	}
	attached := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		attached <- b.call("PATCH", fmt.Sprintf("/api/v1/projects/%d", p.ID), b.adminSs,
			`{"static_addons":[{"name":"maps.zip","target":"."}]}`)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for b.srv.proj.addonsMu.TryLock() {
		b.srv.proj.addonsMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("attach never acquired the static reference lock")
		}
		time.Sleep(time.Millisecond)
	}
	replaced := make(chan *httptest.ResponseRecorder, 1)
	go func() { replaced <- b.upload("?replace=1", map[string]string{"maps.zip": "replacement"}, "maps.zip") }()
	select {
	case rr := <-replaced:
		t.Fatalf("replacement finished while attach was waiting to commit: %d %s", rr.Code, rr.Body)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if rr := <-attached; rr.Code != http.StatusOK {
		t.Fatalf("attach: %d %s", rr.Code, rr.Body)
	}
	errBody(t, <-replaced, http.StatusForbidden, apitypes.ErrorCodeForbidden)
}

func TestProjectDeleteKeepsCheckoutWhenDatabaseDeleteFails(t *testing.T) {
	b := newBuildHarness(t)
	p := b.project("arena")
	db, err := sql.Open("sqlite", b.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER refuse_project_delete BEFORE DELETE ON projects
		BEGIN SELECT RAISE(ABORT, 'cannot delete project'); END`); err != nil {
		t.Fatal(err)
	}
	errBody(t, b.call("DELETE", fmt.Sprintf("/api/v1/projects/%d", p.ID), b.adminSs, nil), http.StatusInternalServerError, apitypes.ErrorCodeInternal)
	if _, err := os.Stat(filepath.Join(b.repoDir, fmt.Sprint(p.ID), "golem.yaml")); err != nil {
		t.Fatalf("checkout lost after failed transaction: %v", err)
	}
	if _, err := b.st.GetProject(t.Context(), p.ID); err != nil {
		t.Fatalf("project lost after failed transaction: %v", err)
	}
}
