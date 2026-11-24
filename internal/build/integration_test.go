package build_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Integration tests build the fixture game in testdata/game with a real
// Docker daemon (GOLEM_DEPLOY_DOCKER_TESTS=1, network access needed for the
// base images, Go modules and npm). Images are tagged golem-deploy/gdm3b-*
// and removed at cleanup, as are the containers they start.

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
}

func dockerCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// copyTree copies the fixture into a fake checkout (with a .git directory
// holding a marker that must never reach the build context).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestIntegrationBuildFixtureGame(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := "gdm3b-" + hex.EncodeToString(suffix)
	p, err := st.CreateProject(ctx, store.Project{Name: name, RepoURL: "https://git.example.com/fixture.git", Branch: "main",
		LastCommit: strings.Repeat("ab", 20)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	staticDir := filepath.Join(dir, "static")
	if err := os.MkdirAll(staticDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeZip(t, filepath.Join(staticDir, "assets.zip"), map[string]string{"hello.txt": "static add-on\n", "sub/deep.txt": "deep\n"})
	addons := []store.StaticAddon{{Name: "assets.zip", Target: "web/dist/extra"}}
	if _, err := st.UpdateProjectRemote(ctx, p.ID, store.ProjectRemoteUpdate{StaticAddons: &addons}); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dir, "repositories")
	checkout := filepath.Join(repoDir, "1")
	copyTree(t, filepath.Join("testdata", "game"), checkout)
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[credential]\n\thelper = SECRET-MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dc := docker.New(docker.Options{InstanceID: dockertest.IntegrationInstanceID, TempDir: filepath.Join(dir, "tmp")})
	svc := build.New(build.Options{Store: st, Docker: dc, InstanceID: dockertest.IntegrationInstanceID,
		RepoDir: repoDir, TempDir: filepath.Join(dir, "tmp"), StaticDir: staticDir, Timeout: 20 * time.Minute})
	b, err := st.CreateBuild(ctx, store.NewBuild{ProjectID: p.ID, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuf{}
	start := time.Now()
	runErr := svc.Run(ctx, b.ID, log, nil)
	elapsed := time.Since(start)
	t.Logf("build took %s", elapsed.Round(time.Second))
	got, err := st.GetBuild(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageID != "" {
		t.Cleanup(func() {
			if out, err := dockerCLI(t, "image", "rm", "--force", got.ImageID); err != nil {
				t.Logf("cleanup image: %v %s", err, out)
			}
		})
	}
	if runErr != nil || got.Status != store.BuildStatusSuccess {
		t.Fatalf("build: %v (status %s, detail %q)\n%s", runErr, got.Status, got.Detail, log.String())
	}
	out := log.String()
	for _, leak := range []string{dir, filepath.ToSlash(dir), "SECRET-MARKER"} {
		if strings.Contains(out, leak) {
			t.Errorf("build log contains %q", leak)
		}
	}
	if got.EngineVersion != "v0.4.0-rc.1" || got.ImageTag != "golem-deploy/"+name+":abababababab" {
		t.Errorf("recorded %+v", got)
	}
	labels, err := dockerCLI(t, "image", "inspect", "--format", "{{json .Config.Labels}} {{.Config.User}} {{json .Config.Entrypoint}} {{.Config.WorkingDir}}", got.ImageID)
	if err != nil {
		t.Fatal(err, labels)
	}
	for _, want := range []string{`"dev.golemengine.deploy.build-id":"1"`, `"dev.golemengine.deploy.engine-version":"v0.4.0-rc.1"`,
		`"dev.golemengine.deploy.instance":"` + dockertest.IntegrationInstanceID + `"`, `"dev.golemengine.deploy.managed":"true"`,
		"65532:65532", `["/usr/local/bin/golem-server"]`, " /app"} {
		if !strings.Contains(labels, want) {
			t.Errorf("image config %s lacks %s", labels, want)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(entries) != 0 {
		t.Errorf("build directory left behind: %v", entries)
	}

	// Run the image briefly with the daemon's hardening (read-only root,
	// no capabilities, non-root) and prove it serves the web client, the
	// static add-on and its API.
	port := freePort(t)
	cname := name + "-run"
	cid, err := dockerCLI(t, "run", "--detach", "--name", cname, "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--env", "GOLEM_HTTP_ADDR=:7777", "--publish", "127.0.0.1:"+itoa(port)+":7777/tcp", "--", got.ImageID)
	if err != nil {
		t.Fatalf("docker run: %v %s", err, cid)
	}
	t.Cleanup(func() { _, _ = dockerCLI(t, "rm", "--force", cname) })
	client := &http.Client{Timeout: 2 * time.Second}
	get := func(path string) (string, int) {
		resp, err := client.Get("http://127.0.0.1:" + itoa(port) + path)
		if err != nil {
			return err.Error(), 0
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body), resp.StatusCode
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, code := get("/api/health")
		if code == http.StatusOK && strings.TrimSpace(body) == "ready" {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := dockerCLI(t, "logs", cname)
			t.Fatalf("server did not become ready: %d %q\n%s", code, body, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if body, code := get("/"); code != http.StatusOK || !strings.Contains(body, "golem-deploy fixture client") {
		t.Errorf("web client: %d %q", code, body)
	}
	if body, code := get("/extra/hello.txt"); code != http.StatusOK || body != "static add-on\n" {
		t.Errorf("static add-on: %d %q", code, body)
	}
	if body, code := get("/extra/sub/deep.txt"); code != http.StatusOK || body != "deep\n" {
		t.Errorf("static add-on: %d %q", code, body)
	}
	t.Logf("image %s (%s), build log %d bytes", got.ImageID, got.ImageTag, len(out))
	if os.Getenv("GOLEM_DEPLOY_SHOW_BUILD_LOG") == "1" {
		t.Log(out)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestIntegrationCustomDockerfile runs repository Dockerfiles (opt-in
// dockerfile:) through the real pipeline: the context never contains .git
// or node_modules, host networking is refused (no entitlements are
// granted) and labels in the daemon's namespace fail the build.
func TestIntegrationCustomDockerfile(t *testing.T) {
	requireDocker(t)
	const busybox = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
	cases := []struct {
		name   string
		extra  string
		detail string
	}{
		{"context is clean", "", ""},
		{"host network refused", "RUN --network=host true\n", build.DetailBuildFailed},
		{"reserved label refused", "LABEL dev.golemengine.deploy.server-id=1\n", "reserved for golem-deploy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			st, err := store.Open(ctx, filepath.Join(dir, "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			suffix := make([]byte, 4)
			_, _ = rand.Read(suffix)
			p, err := st.CreateProject(ctx, store.Project{Name: "gdm3b-" + hex.EncodeToString(suffix), RepoURL: "https://git.example.com/x.git",
				Branch: "main", LastCommit: strings.Repeat("cd", 20)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			repoDir := filepath.Join(dir, "repositories")
			checkout := filepath.Join(repoDir, strconv.FormatInt(p.ID, 10))
			copyTree(t, filepath.Join("testdata", "game"), checkout)
			for _, d := range []string{".git", "node_modules/left-pad", "web/node_modules/x"} {
				if err := os.MkdirAll(filepath.Join(checkout, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_ = os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("SECRET-MARKER\n"), 0o600)
			_ = os.WriteFile(filepath.Join(checkout, "golem-deploy.yaml"), []byte("dockerfile: deploy/Dockerfile\n"), 0o644)
			df := "FROM " + busybox + "\nCOPY . /src\nRUN test ! -e /src/.git && test ! -e /src/node_modules && test ! -e /src/web/node_modules && test -f /src/golem.yaml\n" +
				tc.extra + "COPY --from=static / /app/\nENTRYPOINT [\"/bin/sleep\", \"30\"]\n"
			_ = os.MkdirAll(filepath.Join(checkout, "deploy"), 0o755)
			_ = os.WriteFile(filepath.Join(checkout, "deploy", "Dockerfile"), []byte(df), 0o644)

			dc := docker.New(docker.Options{InstanceID: dockertest.IntegrationInstanceID, TempDir: filepath.Join(dir, "tmp")})
			svc := build.New(build.Options{Store: st, Docker: dc, InstanceID: dockertest.IntegrationInstanceID,
				RepoDir: repoDir, TempDir: filepath.Join(dir, "tmp"), StaticDir: filepath.Join(dir, "static"), Timeout: 5 * time.Minute})
			b, _ := st.CreateBuild(ctx, store.NewBuild{ProjectID: p.ID})
			log := &lockedBuf{}
			runErr := svc.Run(ctx, b.ID, log, nil)
			got, _ := st.GetBuild(ctx, b.ID)
			if got.ImageID != "" {
				t.Cleanup(func() { _, _ = dockerCLI(t, "image", "rm", "--force", got.ImageID) })
			}
			if tc.detail == "" {
				if runErr != nil || got.Status != store.BuildStatusSuccess {
					t.Fatalf("%v %+v\n%s", runErr, got, log.String())
				}
				return
			}
			if runErr == nil || got.Status != store.BuildStatusError || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("%v %+v\n%s", runErr, got, log.String())
			}
			if strings.Contains(log.String(), "SECRET-MARKER") {
				t.Fatal("log leaks .git content")
			}
			// No image of a refused build is left behind.
			if out, _ := dockerCLI(t, "image", "ls", "--quiet", "--filter", "label=dev.golemengine.deploy.project-id="+strconv.FormatInt(p.ID, 10),
				"--filter", "label=dev.golemengine.deploy.instance="+dockertest.IntegrationInstanceID, "--filter", "label=dev.golemengine.deploy.build-id="+strconv.FormatInt(b.ID, 10)); out != "" && tc.name == "reserved label refused" {
				t.Fatalf("image kept: %s", out)
			}
		})
	}
}
