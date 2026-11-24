package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t       *testing.T
	st      *store.Store
	fake    *dockertest.Fake
	dc      *docker.Client
	svc     *Service
	repoDir string
	tmpDir  string
	static  string
	daemon  *syncBuf
}

func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := dockertest.New()
	daemon := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(daemon, nil))
	dc := docker.New(docker.Options{Runner: fake, InstanceID: dockertest.InstanceID, Logger: logger,
		LookupEnv: func(k string) (string, bool) {
			if k == "PATH" {
				return "/usr/bin", true
			}
			return "", false
		}})
	e := &env{t: t, st: st, fake: fake, dc: dc, repoDir: filepath.Join(dir, "repositories"), tmpDir: filepath.Join(dir, "tmp"),
		static: filepath.Join(dir, "static"), daemon: daemon}
	if err := os.MkdirAll(e.static, 0o700); err != nil {
		t.Fatal(err)
	}
	o := Options{Store: st, Docker: dc, InstanceID: dockertest.InstanceID, RepoDir: e.repoDir, TempDir: e.tmpDir,
		StaticDir: e.static, Logger: logger, Timeout: time.Minute}
	for _, m := range mutate {
		m(&o)
	}
	e.svc = New(o)
	return e
}

// project creates a project with a checkout copied from files (the
// fixture by default).
func (e *env) project(name string, files map[string]string) store.Project {
	e.t.Helper()
	p, err := e.st.CreateProject(context.Background(), store.Project{Name: name, RepoURL: "https://git.example.com/x.git", Branch: "main",
		LastCommit: strings.Repeat("c", 40)}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	dst := filepath.Join(e.repoDir, fmt.Sprint(p.ID))
	if files == nil {
		copyDir(e.t, filepath.Join("testdata", "game"), dst)
	} else {
		src := writeRepo(e.t, files)
		copyDir(e.t, src, dst)
	}
	if err := os.MkdirAll(filepath.Join(dst, ".git"), 0o700); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// treeHash hashes every path and content below dir.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s|%v\n", rel, d.IsDir())
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			h.Write(b)
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

// run creates a build for p and runs it.
func (e *env) run(ctx context.Context, p store.Project, prepare jobs.Func) (store.Build, string, error) {
	e.t.Helper()
	b, err := e.st.CreateBuild(context.Background(), store.NewBuild{ProjectID: p.ID, Branch: p.Branch})
	if err != nil {
		e.t.Fatal(err)
	}
	log := &syncBuf{}
	runErr := e.svc.Run(ctx, b.ID, log, prepare)
	got, err := e.st.GetBuild(context.Background(), b.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got, log.String(), runErr
}

func TestRunSuccess(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	makeZip(t, filepath.Join(e.static, "assets.zip"), map[string]string{"a.txt": "a"})
	addons := []store.StaticAddon{{Name: "assets.zip", Target: "assets"}}
	if _, err := e.st.UpdateProjectRemote(context.Background(), p.ID, store.ProjectRemoteUpdate{StaticAddons: &addons}); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(e.repoDir, fmt.Sprint(p.ID))
	before := treeHash(t, checkout)
	var call dockertest.BuildCall
	var staged map[string]string
	e.fake.SetBuildHook(func(c dockertest.BuildCall) dockertest.BuildOutcome {
		call = c
		staged = readTree(t, c.StaticDir)
		// BuildKit output naming host paths and escape sequences.
		return dockertest.BuildOutcome{Output: "#1 load build definition from " + c.Args[2] + "\n#2 context " + c.ContextDir +
			"\n#3 \x1b[31mred\x1b[0m \u202eevil\n"}
	})
	b, log, err := e.run(context.Background(), p, nil)
	if err != nil || b.Status != store.BuildStatusSuccess {
		t.Fatalf("%v %+v\n%s", err, b, log)
	}
	if b.ImageTag != "golem-deploy/arena:cccccccccccc" || b.EngineVersion != "v0.4.0-rc.1" || !strings.HasPrefix(b.ImageID, "sha256:") ||
		b.Commit != strings.Repeat("c", 40) || b.StartedAt == nil || b.FinishedAt == nil || b.Detail != "" {
		t.Fatalf("recorded %+v", b)
	}
	// Exact argv: only validated values, the context last after "--".
	dfDir := filepath.Dir(call.Args[2])
	buildDir := filepath.Dir(dfDir)
	want := []string{"build", "--file", filepath.Join(buildDir, "docker", "Dockerfile"), "--tag", "golem-deploy/arena:cccccccccccc",
		"--label", "dev.golemengine.deploy.managed=true",
		"--label", "dev.golemengine.deploy.instance=" + dockertest.InstanceID,
		"--label", fmt.Sprintf("dev.golemengine.deploy.project-id=%d", p.ID),
		"--label", "dev.golemengine.deploy.commit=" + strings.Repeat("c", 40),
		"--label", fmt.Sprintf("dev.golemengine.deploy.build-id=%d", b.ID),
		"--label", "dev.golemengine.deploy.engine-version=v0.4.0-rc.1",
		"--iidfile", filepath.Join(buildDir, "iid"), "--metadata-file", filepath.Join(buildDir, "metadata.json"),
		"--progress", "plain", "--provenance=false", "--sbom=false", "--load",
		"--build-context", "static=" + filepath.Join(buildDir, "static"), "--", checkout}
	if !slices.Equal(call.Args, want) {
		t.Fatalf("argv\n%q\nwant\n%q", call.Args, want)
	}
	if !strings.HasPrefix(buildDir, e.tmpDir) {
		t.Fatalf("build dir %s not below tmp", buildDir)
	}
	calls := e.fake.CallsTo("build")
	for _, v := range docker.BuildEnv {
		if !slices.Contains(calls[0].Env, v) {
			t.Errorf("CLI env lacks %s: %v", v, calls[0].Env)
		}
	}
	if !strings.Contains(string(call.Dockerfile), "COPY --from=static / /app/") || !strings.Contains(string(call.Dockerignore), "\n.git\n") {
		t.Fatalf("dockerfile/dockerignore:\n%s\n%s", call.Dockerfile, call.Dockerignore)
	}
	if staged["assets/a.txt"] != "a" || len(staged) != 1 {
		t.Fatalf("staged %v", staged)
	}
	// Nothing left behind, checkout untouched.
	if entries, _ := os.ReadDir(e.tmpDir); len(entries) != 0 {
		t.Fatalf("tmp not cleaned: %v", entries)
	}
	if treeHash(t, checkout) != before {
		t.Fatal("the checkout was modified")
	}
	// Host paths are replaced and control characters escaped in the log.
	for _, leak := range []string{e.tmpDir, e.repoDir, filepath.ToSlash(e.tmpDir), "\x1b", "\u202e"} {
		if strings.Contains(log, leak) {
			t.Errorf("log contains %q:\n%s", leak, log)
		}
	}
	if !strings.Contains(log, "<build-dir>") || !strings.Contains(log, "<repository>") || !strings.Contains(log, `\x1b[31mred`) {
		t.Errorf("log:\n%s", log)
	}
	img, err := e.dc.ManagedImage(context.Background(), b.ImageID)
	if err != nil || img.Labels[docker.LabelBuildID] != fmt.Sprint(b.ID) {
		t.Fatalf("%+v %v", img, err)
	}
}

func TestRunContainerdImageStore(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	e.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		return dockertest.BuildOutcome{ContainerdStore: true}
	})
	b, log, err := e.run(context.Background(), p, nil)
	if err != nil || b.Status != store.BuildStatusSuccess || !e.fake.HasImage(b.ImageID) {
		t.Fatalf("%v %+v\n%s", err, b, log)
	}
}

func TestRunFailures(t *testing.T) {
	cases := []struct {
		name   string
		files  func(map[string]string)
		hook   dockertest.BuildOutcome
		down   bool
		addon  bool
		detail string
	}{
		{name: "docker build fails", hook: dockertest.BuildOutcome{Fail: true, Output: "step failed\n"}, detail: DetailBuildFailed},
		{name: "docker unreachable", down: true, detail: DetailDockerDown},
		{name: "image not found", hook: dockertest.BuildOutcome{ContainerdStore: true, NoMetadata: true}, detail: "could not be found"},
		{name: "foreign namespace label", hook: dockertest.BuildOutcome{ExtraLabels: map[string]string{docker.LabelServerID: "1"}}, detail: "reserved for golem-deploy"},
		{name: "preflight", files: func(f map[string]string) { delete(f, "golem.yaml") }, detail: "golem.yaml was not found"},
		{name: "missing add-on", addon: true, detail: "does not exist in the static store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			files := baseRepo()
			if tc.files != nil {
				tc.files(files)
			}
			p := e.project("arena", files)
			if tc.addon {
				addons := []store.StaticAddon{{Name: "gone.zip", Target: "."}}
				_, _ = e.st.UpdateProjectRemote(context.Background(), p.ID, store.ProjectRemoteUpdate{StaticAddons: &addons})
			}
			e.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome { return tc.hook })
			e.fake.SetDown(tc.down)
			b, log, err := e.run(context.Background(), p, nil)
			var se *jobs.SafeError
			if !errors.As(err, &se) || !strings.Contains(se.Msg, tc.detail) {
				t.Fatalf("err %v", err)
			}
			if b.Status != store.BuildStatusError || !strings.Contains(b.Detail, tc.detail) || b.ImageID != "" {
				t.Fatalf("%+v\n%s", b, log)
			}
			if tc.name == "foreign namespace label" {
				e.fake.SetDown(false)
				imgs, _ := e.dc.ListImages(context.Background())
				if len(imgs) != 0 {
					t.Fatalf("image with foreign labels kept: %+v", imgs)
				}
			}
			if entries, _ := os.ReadDir(e.tmpDir); len(entries) != 0 {
				t.Fatalf("tmp not cleaned: %v", entries)
			}
		})
	}
}

func TestRunNoCheckoutAndPrepare(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	b, _, err := e.run(context.Background(), p, func(ctx context.Context, w io.Writer) error {
		return jobs.Fail("authentication failed", errors.New("secret detail"))
	})
	if err == nil || b.Status != store.BuildStatusError || b.Detail != "updating the checkout failed: authentication failed" {
		t.Fatalf("%v %+v", err, b)
	}
	if err := os.RemoveAll(filepath.Join(e.repoDir, fmt.Sprint(p.ID), ".git")); err != nil {
		t.Fatal(err)
	}
	b, _, err = e.run(context.Background(), p, nil)
	if err == nil || b.Status != store.BuildStatusError || b.Detail != DetailNoCheckout {
		t.Fatalf("%v %+v", err, b)
	}
}
