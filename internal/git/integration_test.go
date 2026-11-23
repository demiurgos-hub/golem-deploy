package git

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The integration tests serve bare repositories over loopback HTTPS with
// "git http-backend" (CGI) and basic auth, so they exercise the production
// hardening unchanged (file:// stays disabled) and the real askpass path
// (this test binary answers as the helper; see TestMain).

const (
	srvUser = "deployer"
	srvPass = "s3cr3t-Planted/Pass+word"
)

func requireGit(t *testing.T) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; skipping integration test")
	}
	out, err := exec.Command(gitPath, "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed; skipping integration test")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available; skipping integration test")
	}
	return backend
}

// fixtureGit runs plain git for building fixtures (not the Runner).
func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type gitServer struct {
	t          *testing.T
	srv        *httptest.Server
	root       string // bare repositories
	work       string // working copy pushing to repo.git
	caFile     string
	mu         sync.Mutex
	hang       bool
	goodAuth   int    // requests with the right credentials
	redirectTo string // base URL that /redirect/ paths are sent to
	// Minimal Git LFS server (see lfs_integration_test.go): objects by oid,
	// and the paths of the batch requests received.
	lfsObjects map[string][]byte
	lfsBatches []string
}

func newGitServer(t *testing.T) *gitServer {
	backend := requireGit(t)
	base := t.TempDir()
	g := &gitServer{t: t, root: filepath.Join(base, "srv"), work: filepath.Join(base, "work")}
	if err := os.MkdirAll(g.root, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, base, "init", "-q", "--bare", "-b", "main", filepath.Join(g.root, "repo.git"))
	fixtureGit(t, base, "init", "-q", "--bare", "-b", "main", filepath.Join(g.root, "empty.git"))
	fixtureGit(t, base, "init", "-q", "-b", "main", g.work)
	g.commit("README.md", "hello\n")
	fixtureGit(t, g.work, "branch", "feature")
	g.push()

	cgiHandler := &cgi.Handler{
		Path:       backend,
		Root:       "/git",
		Env:        []string{"GIT_PROJECT_ROOT=" + g.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull},
		InheritEnv: []string{"PATH", "SYSTEMROOT", "SystemRoot", "TEMP", "TMP"},
	}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		hang := g.hang
		g.mu.Unlock()
		if hang {
			<-r.Context().Done()
			return
		}
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			g.mu.Lock()
			target := g.redirectTo
			g.mu.Unlock()
			http.Redirect(w, r, target+"/git/"+strings.TrimPrefix(r.URL.Path, "/redirect/")+"?"+r.URL.RawQuery, http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/lfs-obj/") {
			g.serveLFSObject(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != srvUser || p != srvPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		g.goodAuth++
		g.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/info/lfs/objects/batch") {
			g.serveLFSBatch(w, r)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	g.caFile = filepath.Join(base, "ca.pem")
	if err := os.WriteFile(g.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *gitServer) port() string {
	_, port, _ := net.SplitHostPort(g.srv.Listener.Addr().String())
	return port
}

func (g *gitServer) url(repo string) string { return "https://127.0.0.1:" + g.port() + "/git/" + repo }

func (g *gitServer) commit(name, content string) string {
	if err := os.MkdirAll(filepath.Dir(filepath.Join(g.work, name)), 0o700); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.work, name), []byte(content), 0o600); err != nil {
		g.t.Fatal(err)
	}
	fixtureGit(g.t, g.work, "add", "-A")
	fixtureGit(g.t, g.work, "commit", "-q", "-m", "update "+name)
	return fixtureGit(g.t, g.work, "rev-parse", "HEAD")
}

func (g *gitServer) push() {
	fixtureGit(g.t, g.work, "push", "-q", "--all", "--force", filepath.Join(g.root, "repo.git"))
}

func (g *gitServer) setHang(v bool) {
	g.mu.Lock()
	g.hang = v
	g.mu.Unlock()
}

// newTestRunner builds a runner whose scratch paths contain a space (to
// exercise quoting) and which trusts the test server's certificate.
func (g *gitServer) runner(t *testing.T, mutate ...func(*Options)) (*Runner, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "data dir")
	o := Options{
		TempDir:        filepath.Join(base, "tmp"),
		KnownHostsFile: filepath.Join(base, "known_hosts"),
		Environ:        append(os.Environ(), "GIT_SSL_CAINFO="+g.caFile, "GIT_DIR=/nonexistent", "GIT_CONFIG_PARAMETERS='protocol.file.allow=always'"),
	}
	for _, m := range mutate {
		m(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return r, base
}

func httpsAuth(pass string) Auth { return Auth{Method: AuthHTTPS, Username: srvUser, Password: pass} }

// assertNoLeftovers checks that no session dir (with keys) or staging dir
// survived an operation.
func assertNoLeftovers(t *testing.T, tmp string) {
	t.Helper()
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		t.Errorf("leftover in temp dir: %s", e.Name())
	}
}

func TestIntegrationCloneAndPull(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t)
	dir := filepath.Join(base, "repositories", "1")
	var log bytes.Buffer
	ctx := context.Background()

	co, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log)
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, log.String())
	}
	head := fixtureGit(t, g.work, "rev-parse", "main")
	if co.Branch != "main" || co.Commit != head {
		t.Fatalf("checkout %+v, want main@%s", co, head)
	}
	if !HasCheckout(dir) {
		t.Fatal("no checkout")
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if strings.Contains(string(cfg), srvPass) || strings.Contains(string(cfg), srvUser+":") {
		t.Fatalf(".git/config contains credentials:\n%s", cfg)
	}
	if !strings.Contains(log.String(), "$ git clone") {
		t.Fatalf("log lacks command line: %s", log.String())
	}
	if strings.Contains(log.String(), "data dir") {
		t.Fatalf("log reveals data-dir paths:\n%s", log.String())
	}
	assertNoLeftovers(t, filepath.Join(base, "tmp"))

	// New upstream commit, plus local damage that a pull must discard.
	newHead := g.commit("src/main.go", "package main\n")
	g.push()
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("local edit\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("junk\n"), 0o600)
	co, err = r.Pull(ctx, Remote{URL: g.url("repo.git"), Branch: "main", Auth: httpsAuth(srvPass)}, dir, &log)
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, log.String())
	}
	if co.Commit != newHead || co.Branch != "main" {
		t.Fatalf("after pull %+v, want %s", co, newHead)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "hello\n" {
		t.Fatalf("local edit survived: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatal("untracked file survived")
	}
	if h, err := r.HeadCommit(ctx, dir); err != nil || h != newHead {
		t.Fatalf("HeadCommit %q %v", h, err)
	}
	if strings.Contains(log.String(), srvPass) {
		t.Fatal("password in log")
	}
	assertNoLeftovers(t, filepath.Join(base, "tmp"))

	// Cloning into an existing directory is refused.
	if _, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log); err == nil {
		t.Fatal("clone over existing dir")
	}
}

func TestIntegrationBranches(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t)
	ctx := context.Background()
	remote := Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}

	branches, err := r.ListRemoteBranches(ctx, remote)
	if err != nil || strings.Join(branches, ",") != "feature,main" {
		t.Fatalf("branches %q %v", branches, err)
	}

	fixtureGit(t, g.work, "checkout", "-q", "feature")
	featHead := g.commit("feature.txt", "f\n")
	g.push()

	dir := filepath.Join(base, "repositories", "1")
	var log bytes.Buffer
	co, err := r.Clone(ctx, remote, dir, &log)
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, log.String())
	}
	mainHead := co.Commit
	sw := remote
	sw.Branch = "feature"
	co, err = r.SwitchBranch(ctx, sw, dir, &log)
	if err != nil || co.Branch != "feature" || co.Commit != featHead {
		t.Fatalf("switch: %+v %v\n%s", co, err, log.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); err != nil {
		t.Fatal("feature file missing")
	}

	sw.Branch = "does-not-exist"
	_, err = r.SwitchBranch(ctx, sw, dir, &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonBranchNotFound || !ge.Unchanged {
		t.Fatalf("missing branch: %#v", err)
	}
	if h, _ := r.HeadCommit(ctx, dir); h != featHead {
		t.Fatal("failed switch changed the checkout")
	}

	sw.Branch = "main"
	if co, err = r.SwitchBranch(ctx, sw, dir, &log); err != nil || co.Commit != mainHead {
		t.Fatalf("back to main: %+v %v", co, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.txt")); !os.IsNotExist(err) {
		t.Fatal("feature file survived switch")
	}

	// Clone a specific branch.
	dir2 := filepath.Join(base, "repositories", "2")
	if co, err = r.Clone(ctx, Remote{URL: g.url("repo.git"), Branch: "feature", Auth: httpsAuth(srvPass)}, dir2, &log); err != nil || co.Branch != "feature" {
		t.Fatalf("clone feature: %+v %v", co, err)
	}
	// A missing branch on clone.
	_, err = r.Clone(ctx, Remote{URL: g.url("repo.git"), Branch: "nope", Auth: httpsAuth(srvPass)}, filepath.Join(base, "repositories", "3"), &log)
	if !errors.As(err, &ge) || ge.Reason != ReasonBranchNotFound {
		t.Fatalf("clone missing branch: %v", err)
	}
	assertNoLeftovers(t, filepath.Join(base, "tmp"))
}

func TestIntegrationAuthFailuresDoNotLeak(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t)
	ctx := context.Background()
	const wrong = "Wr0ng-Planted-Password!"
	dir := filepath.Join(base, "repositories", "1")

	for name, auth := range map[string]Auth{"wrong password": httpsAuth(wrong), "no credentials": {}} {
		t.Run(name, func(t *testing.T) {
			var log bytes.Buffer
			start := time.Now()
			_, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: auth}, dir, &log)
			var ge *Error
			if !errors.As(err, &ge) || ge.Reason != ReasonAuth || !ge.Unchanged {
				t.Fatalf("err %v\n%s", err, log.String())
			}
			if time.Since(start) > 30*time.Second {
				t.Fatal("auth failure took too long (prompt?)")
			}
			for _, text := range []string{log.String(), err.Error(), ge.Message()} {
				if strings.Contains(text, wrong) || strings.Contains(text, srvPass) {
					t.Fatalf("secret leaked: %q", text)
				}
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("failed clone left a directory")
			}
			if _, err := r.ListRemoteBranches(ctx, Remote{URL: g.url("repo.git"), Auth: auth}); !errors.As(err, &ge) || ge.Reason != ReasonAuth {
				t.Fatalf("ls-remote: %v", err)
			}
			assertNoLeftovers(t, filepath.Join(base, "tmp"))
		})
	}

	// A redirect to another host (another port of the loopback address,
	// which shares the test certificate): the helper refuses to answer for
	// it, so the credentials never reach the redirect target.
	other := newGitServer(t)
	g.mu.Lock()
	g.redirectTo = "https://127.0.0.1:" + other.port()
	g.mu.Unlock()
	var log bytes.Buffer
	_, err := r.Clone(ctx, Remote{URL: "https://127.0.0.1:" + g.port() + "/redirect/repo.git", Auth: httpsAuth(srvPass)}, dir, &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonAuth {
		t.Fatalf("clone through cross-host redirect: %v\n%s", err, log.String())
	}
	other.mu.Lock()
	leaked := other.goodAuth
	other.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("credentials sent to the redirect target %d times", leaked)
	}
	if strings.Contains(log.String(), srvPass) {
		t.Fatal("password in log")
	}
}

func TestIntegrationEmptyRepository(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t)
	var log bytes.Buffer
	_, err := r.Clone(context.Background(), Remote{URL: g.url("empty.git"), Auth: httpsAuth(srvPass)}, filepath.Join(base, "repositories", "1"), &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonEmptyRepository {
		t.Fatalf("empty: %v\n%s", err, log.String())
	}
}

func TestIntegrationLFSMissing(t *testing.T) {
	g := newGitServer(t)
	g.commit(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	g.commit("asset.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat("0", 64)+"\nsize 3\n")
	g.push()
	r, base := g.runner(t)
	r.lfsOnce.Do(func() { r.lfsOK = false }) // pretend git-lfs is not installed
	var log bytes.Buffer
	dir := filepath.Join(base, "repositories", "1")
	_, err := r.Clone(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log)
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonLFSMissing {
		t.Fatalf("lfs: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "git-lfs is not installed") {
		t.Fatalf("log lacks explanation: %s", log.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("checkout left behind")
	}
}

func TestIntegrationCancelAndTimeout(t *testing.T) {
	g := newGitServer(t)
	g.setHang(true)

	t.Run("cancel", func(t *testing.T) {
		r, base := g.runner(t)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(500*time.Millisecond, cancel)
		start := time.Now()
		var log bytes.Buffer
		_, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, filepath.Join(base, "repositories", "1"), &log)
		var ge *Error
		if !errors.As(err, &ge) || ge.Reason != ReasonCancelled {
			t.Fatalf("cancel: %v", err)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Fatalf("cancellation took %v", d)
		}
		assertNoLeftovers(t, filepath.Join(base, "tmp"))
	})
	t.Run("timeout", func(t *testing.T) {
		r, base := g.runner(t, func(o *Options) { o.NetworkTimeout = time.Second })
		start := time.Now()
		_, err := r.ListRemoteBranches(context.Background(), Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)})
		var ge *Error
		if !errors.As(err, &ge) || ge.Reason != ReasonTimeout {
			t.Fatalf("timeout: %v", err)
		}
		if d := time.Since(start); d > 15*time.Second {
			t.Fatalf("timeout took %v", d)
		}
		assertNoLeftovers(t, filepath.Join(base, "tmp"))
	})
}

func TestIntegrationRemoteURLChangeAppliesOnPull(t *testing.T) {
	g := newGitServer(t)
	r, base := g.runner(t)
	ctx := context.Background()
	dir := filepath.Join(base, "repositories", "1")
	var log bytes.Buffer
	if _, err := r.Clone(ctx, Remote{URL: g.url("repo.git"), Auth: httpsAuth(srvPass)}, dir, &log); err != nil {
		t.Fatal(err)
	}
	// Same repository under another accepted spelling of the URL.
	newURL := "https://127.0.0.1:" + g.port() + "/git/repo.git/"
	if _, err := r.Pull(ctx, Remote{URL: newURL, Branch: "main", Auth: httpsAuth(srvPass)}, dir, &log); err != nil {
		t.Fatalf("pull: %v\n%s", err, log.String())
	}
	if got := fixtureGit(t, dir, "config", "remote.origin.url"); got != newURL {
		t.Fatalf("remote url %q", got)
	}
}

// TestIntegrationSSHCommand checks that the generated GIT_SSH_COMMAND runs
// (quoting of paths with spaces included) by connecting to a closed port.
func TestIntegrationSSHCommand(t *testing.T) {
	requireGit(t)
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH; skipping")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // now closed: connection refused
	_, port, _ := net.SplitHostPort(addr)

	base := filepath.Join(t.TempDir(), "data dir")
	r, err := New(Options{TempDir: filepath.Join(base, "tmp"), KnownHostsFile: filepath.Join(base, "known_hosts")})
	if err != nil {
		t.Fatal(err)
	}
	key, err := PrepareSSHKey(genKey(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ListRemoteBranches(context.Background(), Remote{URL: "ssh://git@127.0.0.1:" + port + "/x.git", Auth: Auth{Method: AuthSSH, PrivateKey: key}})
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonNetwork {
		t.Fatalf("want network failure from ssh, got %v", err)
	}
	assertNoLeftovers(t, filepath.Join(base, "tmp"))
}
