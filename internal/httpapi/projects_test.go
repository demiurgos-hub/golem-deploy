package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

const plantedProjectPassword = "Pr0ject-Planted-P@ssword"

// fakeGit records operations and simulates checkouts on disk.
type fakeGit struct {
	mu        sync.Mutex
	calls     []fakeCall
	commit    string
	branches  []string
	cloneErr  error
	pullErr   error
	switchErr error
	listErr   error
	listCount int
	// gate, when set, blocks operations until it is closed or the job is
	// cancelled; started receives one value per blocked operation.
	gate    chan struct{}
	started chan string
}

type fakeCall struct {
	op     string
	remote git.Remote
	dir    string
}

func newFakeGit() *fakeGit {
	return &fakeGit{commit: strings.Repeat("a", 40), branches: []string{"feature", "main"}}
}

func (f *fakeGit) enter(ctx context.Context, op string, remote git.Remote, dir string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{op: op, remote: remote, dir: dir})
	gate, started := f.gate, f.started
	f.mu.Unlock()
	if gate == nil {
		return nil
	}
	if started != nil {
		started <- op
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return &git.Error{Op: op, Reason: git.ReasonCancelled, Err: ctx.Err()}
	}
}

func (f *fakeGit) result(remote git.Remote, err error) (git.Checkout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		return git.Checkout{}, err
	}
	b := remote.Branch
	if b == "" {
		b = "main"
	}
	return git.Checkout{Branch: b, Commit: f.commit}, nil
}

func (f *fakeGit) Clone(ctx context.Context, remote git.Remote, dir string, log io.Writer) (git.Checkout, error) {
	if err := f.enter(ctx, "clone", remote, dir); err != nil {
		return git.Checkout{}, err
	}
	fmt.Fprintf(log, "fake clone of %s\n", remote.URL)
	f.mu.Lock()
	cerr := f.cloneErr
	f.mu.Unlock()
	if cerr == nil {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			return git.Checkout{}, err
		}
	}
	return f.result(remote, cerr)
}

func (f *fakeGit) Pull(ctx context.Context, remote git.Remote, dir string, log io.Writer) (git.Checkout, error) {
	if err := f.enter(ctx, "pull", remote, dir); err != nil {
		return git.Checkout{}, err
	}
	f.mu.Lock()
	perr := f.pullErr
	f.mu.Unlock()
	return f.result(remote, perr)
}

func (f *fakeGit) SwitchBranch(ctx context.Context, remote git.Remote, dir string, log io.Writer) (git.Checkout, error) {
	if err := f.enter(ctx, "branch", remote, dir); err != nil {
		return git.Checkout{}, err
	}
	f.mu.Lock()
	serr := f.switchErr
	f.mu.Unlock()
	return f.result(remote, serr)
}

func (f *fakeGit) ListRemoteBranches(ctx context.Context, remote git.Remote) ([]string, error) {
	if err := f.enter(ctx, "ls-remote", remote, ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCount++
	return append([]string{}, f.branches...), f.listErr
}

func (f *fakeGit) lastCall() fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeGit) set(fn func(f *fakeGit)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type projectHarness struct {
	*harness
	git     *fakeGit
	repoDir string
	logs    *lockedBuffer
	admin   string
	dev     string
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newProjectHarness(t *testing.T) *projectHarness {
	h := newHarness(t)
	dir := t.TempDir()
	ph := &projectHarness{harness: h, git: newFakeGit(), repoDir: filepath.Join(dir, "repositories"), logs: &lockedBuffer{}}
	h.srv.proj.git = ph.git
	h.srv.proj.repoDir = ph.repoDir
	h.srv.proj.tmpDir = filepath.Join(dir, "tmp")
	h.srv.logger = slog.New(slog.NewTextHandler(ph.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ph.admin = h.session(h.account("admin1", store.RoleAdmin, true))
	ph.dev = h.session(h.account("dev1", store.RoleDeveloper, true))
	return ph
}

// create posts a project and returns the response.
func (ph *projectHarness) create(body any) apitypes.CreateProjectResponse {
	ph.t.Helper()
	rr := ph.do("POST", "/api/v1/projects", ph.admin, body)
	if rr.Code != http.StatusCreated {
		ph.t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.CreateProjectResponse](ph.t, rr)
}

// wait blocks until the job is no longer active and returns it.
func (ph *projectHarness) wait(job apitypes.Job) apitypes.Job {
	ph.t.Helper()
	select {
	case <-ph.runner.Done(job.ID):
	case <-time.After(10 * time.Second):
		ph.t.Fatalf("job %d did not finish", job.ID)
	}
	rr := ph.do("GET", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10), ph.dev, nil)
	return decode[apitypes.Job](ph.t, rr)
}

func (ph *projectHarness) project(id int64) apitypes.Project {
	ph.t.Helper()
	rr := ph.do("GET", fmt.Sprintf("/api/v1/projects/%d", id), ph.dev, nil)
	if rr.Code != http.StatusOK {
		ph.t.Fatalf("get project: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.Project](ph.t, rr)
}

func (ph *projectHarness) jobLog(id int64) string {
	ph.t.Helper()
	return ph.do("GET", fmt.Sprintf("/api/v1/jobs/%d/log", id), ph.dev, nil).Body.String()
}

func httpsProject(name string) apitypes.CreateProjectRequest {
	return apitypes.CreateProjectRequest{
		Name:    name,
		RepoURL: "https://git.example.com/team/game.git",
		Auth:    apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeHTTPS, Username: "deployer", Password: plantedProjectPassword},
	}
}

func sshKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blk *pem.Block
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func TestProjectLifecycle(t *testing.T) {
	ph := newProjectHarness(t)
	res := ph.create(httpsProject("game"))
	p := res.Project
	if p.Status != apitypes.ProjectStatusCloning || p.AuthType != apitypes.ProjectAuthTypeHTTPS || p.AuthUsername != "deployer" || !p.HasSecret || p.LastPulledAt != nil {
		t.Fatalf("created project %+v", p)
	}
	if res.Job.Kind != apitypes.JobKindProjectClone || res.Job.SubjectID != strconv.FormatInt(p.ID, 10) {
		t.Fatalf("job %+v", res.Job)
	}
	if j := ph.wait(res.Job); j.Status != apitypes.JobStatusSuccess {
		t.Fatalf("clone job %+v\n%s", j, ph.jobLog(j.ID))
	}
	call := ph.git.lastCall()
	wantDir := filepath.Join(ph.repoDir, strconv.FormatInt(p.ID, 10))
	if call.op != "clone" || call.dir != wantDir || call.remote.Auth.Password != plantedProjectPassword ||
		call.remote.Auth.Username != "deployer" || call.remote.Auth.Method != git.AuthHTTPS || call.remote.Branch != "" {
		t.Fatalf("clone call %+v (dir %s)", call, call.dir)
	}
	p = ph.project(p.ID)
	if p.Status != apitypes.ProjectStatusIdle || p.Branch != "main" || p.LastCommit != strings.Repeat("a", 40) || p.LastPulledAt == nil {
		t.Fatalf("after clone %+v", p)
	}

	// Pull picks up a new commit.
	ph.git.set(func(f *fakeGit) { f.commit = strings.Repeat("b", 40) })
	rr := ph.do("POST", fmt.Sprintf("/api/v1/projects/%d/pull", p.ID), ph.dev, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("pull %d %s", rr.Code, rr.Body.String())
	}
	job := decode[apitypes.Job](t, rr)
	if job.Kind != apitypes.JobKindProjectPull {
		t.Fatalf("pull kind %s", job.Kind)
	}
	if j := ph.wait(job); j.Status != apitypes.JobStatusSuccess {
		t.Fatalf("pull job %+v", j)
	}
	if c := ph.git.lastCall(); c.op != "pull" || c.remote.Branch != "main" {
		t.Fatalf("pull call %+v", c)
	}
	if p = ph.project(p.ID); p.LastCommit != strings.Repeat("b", 40) || p.Status != apitypes.ProjectStatusIdle {
		t.Fatalf("after pull %+v", p)
	}

	// Remote branches, cached.
	rr = ph.do("GET", fmt.Sprintf("/api/v1/projects/%d/branches", p.ID), ph.dev, nil)
	bl := decode[apitypes.BranchList](t, rr)
	if bl.Current != "main" || strings.Join(bl.Items, ",") != "feature,main" {
		t.Fatalf("branches %+v", bl)
	}
	ph.do("GET", fmt.Sprintf("/api/v1/projects/%d/branches", p.ID), ph.dev, nil)
	if ph.git.listCount != 1 {
		t.Fatalf("listing not cached: %d calls", ph.git.listCount)
	}
	ph.clock.Add(branchCacheTTL + time.Second)
	ph.do("GET", fmt.Sprintf("/api/v1/projects/%d/branches", p.ID), ph.dev, nil)
	if ph.git.listCount != 2 {
		t.Fatalf("cache did not expire: %d calls", ph.git.listCount)
	}

	// Switch branch.
	rr = ph.do("POST", fmt.Sprintf("/api/v1/projects/%d/branch", p.ID), ph.dev, apitypes.SwitchBranchRequest{Branch: "feature"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("switch %d %s", rr.Code, rr.Body.String())
	}
	job = decode[apitypes.Job](t, rr)
	if job.Kind != apitypes.JobKindProjectBranch {
		t.Fatalf("switch kind %s", job.Kind)
	}
	ph.wait(job)
	if c := ph.git.lastCall(); c.op != "branch" || c.remote.Branch != "feature" {
		t.Fatalf("switch call %+v", c)
	}
	if p = ph.project(p.ID); p.Branch != "feature" || p.Status != apitypes.ProjectStatusIdle {
		t.Fatalf("after switch %+v", p)
	}

	// List.
	list := decode[apitypes.ProjectList](t, ph.do("GET", "/api/v1/projects", ph.dev, nil))
	if len(list.Items) != 1 || list.Items[0].ID != p.ID {
		t.Fatalf("list %+v", list)
	}

	// Delete removes the checkout and the row.
	rr = ph.do("DELETE", fmt.Sprintf("/api/v1/projects/%d", p.ID), ph.admin, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(wantDir); !os.IsNotExist(err) {
		t.Fatal("checkout survived delete")
	}
	errBody(t, ph.do("GET", fmt.Sprintf("/api/v1/projects/%d", p.ID), ph.dev, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

func TestProjectRoleMatrix(t *testing.T) {
	ph := newProjectHarness(t)
	p := ph.create(httpsProject("game"))
	ph.wait(p.Job)
	admin, _ := ph.st.GetAccountByUsername(context.Background(), "admin1")
	dev, _ := ph.st.GetAccountByUsername(context.Background(), "dev1")
	adminToken := ph.apiToken(admin, nil)
	devToken := ph.apiToken(dev, nil)
	base := fmt.Sprintf("/api/v1/projects/%d", p.Project.ID)

	type req struct {
		method, path string
		body         any
		admin        bool
	}
	routes := []req{
		{"GET", "/api/v1/projects", nil, false},
		{"POST", "/api/v1/projects", httpsProject("other"), true},
		{"GET", base, nil, false},
		{"PATCH", base, apitypes.UpdateProjectRequest{}, true},
		{"DELETE", base, nil, true},
		{"POST", base + "/pull", nil, false},
		{"POST", base + "/branch", apitypes.SwitchBranchRequest{Branch: "main"}, false},
		{"GET", base + "/branches", nil, false},
	}
	for _, rt := range routes {
		for _, id := range []struct {
			name    string
			token   string
			isAdmin bool
		}{{"developer session", ph.dev, false}, {"developer token", devToken, false}, {"admin token", adminToken, false}} {
			rr := ph.do(rt.method, rt.path, id.token, rt.body)
			if rt.admin {
				errBody(t, rr, http.StatusForbidden, apitypes.ErrorCodeForbidden)
			} else if rr.Code >= 400 {
				t.Errorf("%s %s as %s: %d %s", rt.method, rt.path, id.name, rr.Code, rr.Body.String())
			}
			// Let any job started by this request finish before the next.
			if rr.Code == http.StatusAccepted {
				ph.wait(decode[apitypes.Job](t, rr))
			}
		}
		errBody(t, ph.do(rt.method, rt.path, "", rt.body), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	}
}

// TestProjectSecretsNeverExposed plants recognisable credentials and checks
// every response, the job log and the daemon log.
func TestProjectSecretsNeverExposed(t *testing.T) {
	ph := newProjectHarness(t)
	key := sshKey(t, "")
	keyBody := strings.Split(strings.TrimSpace(key), "\n")[2] // a base64 line of the key

	var bodies []string
	collect := func(method, path string, body any) apitypes.Job {
		rr := ph.do(method, path, ph.admin, body)
		bodies = append(bodies, rr.Body.String())
		if rr.Code == http.StatusAccepted {
			return decode[apitypes.Job](t, rr)
		}
		return apitypes.Job{}
	}
	resHTTPS := ph.create(httpsProject("game"))
	bodies = append(bodies, fmt.Sprint(resHTTPS))
	ph.wait(resHTTPS.Job)
	resSSH := ph.create(apitypes.CreateProjectRequest{Name: "ssh-game", RepoURL: "git@git.example.com:team/game.git",
		Auth: apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeSSH, PrivateKey: key}})
	ph.wait(resSSH.Job)
	if c := ph.git.lastCall(); c.remote.Auth.Method != git.AuthSSH || !strings.Contains(string(c.remote.Auth.PrivateKey), keyBody) {
		t.Fatal("ssh key not passed to git")
	}

	ph.git.set(func(f *fakeGit) {
		f.pullErr = &git.Error{Op: "pull", Reason: git.ReasonAuth, Unchanged: true, Err: fmt.Errorf("git fetch exited with status 128")}
	})
	jobs := []apitypes.Job{resHTTPS.Job, resSSH.Job}
	for _, id := range []int64{resHTTPS.Project.ID, resSSH.Project.ID} {
		base := fmt.Sprintf("/api/v1/projects/%d", id)
		collect("GET", base, nil)
		if j := collect("POST", base+"/pull", nil); j.ID != 0 {
			jobs = append(jobs, ph.wait(j))
		}
		collect("GET", base+"/branches", nil)
		collect("PATCH", base, apitypes.UpdateProjectRequest{})
	}
	collect("PATCH", fmt.Sprintf("/api/v1/projects/%d", resHTTPS.Project.ID), apitypes.UpdateProjectRequest{
		Auth: &apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeHTTPS, Username: "deployer", Password: plantedProjectPassword + "2"}})
	collect("GET", "/api/v1/projects", nil)
	collect("GET", "/api/v1/jobs?limit=500", nil)
	for _, j := range jobs {
		collect("GET", fmt.Sprintf("/api/v1/jobs/%d", j.ID), nil)
		bodies = append(bodies, ph.jobLog(j.ID))
	}
	bodies = append(bodies, ph.logs.String())
	for _, b := range bodies {
		for _, secret := range []string{plantedProjectPassword, keyBody, "PRIVATE KEY"} {
			if strings.Contains(b, secret) {
				t.Fatalf("secret %q exposed in:\n%s", secret[:8], b)
			}
		}
	}
	// The pull failure carries the safe message.
	if j := jobs[2]; j.Status != apitypes.JobStatusError || !strings.Contains(j.Detail, "authentication") {
		t.Fatalf("failed pull job %+v", j)
	}
	// The stored secret is ciphertext.
	sp, _ := ph.st.GetProject(context.Background(), resHTTPS.Project.ID)
	if bytes.Contains(sp.AuthSecret, []byte(plantedProjectPassword)) || len(sp.AuthSecret) == 0 {
		t.Fatal("secret not encrypted at rest")
	}
}

func TestProjectValidation(t *testing.T) {
	ph := newProjectHarness(t)
	base := httpsProject("game")
	cases := []struct {
		name   string
		mutate func(*apitypes.CreateProjectRequest)
		field  string
	}{
		{"empty name", func(r *apitypes.CreateProjectRequest) { r.Name = "" }, "name"},
		{"upper name", func(r *apitypes.CreateProjectRequest) { r.Name = "Game" }, "name"},
		{"dash name", func(r *apitypes.CreateProjectRequest) { r.Name = "-game" }, "name"},
		{"traversal name", func(r *apitypes.CreateProjectRequest) { r.Name = "../../etc" }, "name"},
		{"dotdot name", func(r *apitypes.CreateProjectRequest) { r.Name = ".." }, "name"},
		{"slash name", func(r *apitypes.CreateProjectRequest) { r.Name = "a/b" }, "name"},
		{"backslash name", func(r *apitypes.CreateProjectRequest) { r.Name = `a\b` }, "name"},
		{"long name", func(r *apitypes.CreateProjectRequest) { r.Name = strings.Repeat("a", 64) }, "name"},
		{"upload-pack url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "--upload-pack=touch /tmp/pwned" }, "repo_url"},
		{"proxycommand url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "-oProxyCommand=calc" }, "repo_url"},
		{"ext url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "ext::sh -c touch% /tmp/pwned" }, "repo_url"},
		{"file url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "file:///etc" }, "repo_url"},
		{"local path", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "/srv/git/repo" }, "repo_url"},
		{"http url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "http://git.example.com/x" }, "repo_url"},
		{"credentials in url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "https://user:pass@host/x" }, "repo_url"},
		{"branch injection", func(r *apitypes.CreateProjectRequest) { r.Branch = "--output=/etc/passwd" }, "branch"},
		{"branch dotdot", func(r *apitypes.CreateProjectRequest) { r.Branch = "a..b" }, "branch"},
		{"missing auth", func(r *apitypes.CreateProjectRequest) { r.Auth = apitypes.ProjectAuth{} }, "auth.type"},
		{"unknown auth", func(r *apitypes.CreateProjectRequest) { r.Auth.Type = "token" }, "auth.type"},
		{"https auth on ssh url", func(r *apitypes.CreateProjectRequest) { r.RepoURL = "git@host:x" }, "auth.type"},
		{"https without username", func(r *apitypes.CreateProjectRequest) { r.Auth.Username = "" }, "auth.username"},
		{"https without password", func(r *apitypes.CreateProjectRequest) { r.Auth.Password = "" }, "auth.password"},
		{"password with newline", func(r *apitypes.CreateProjectRequest) { r.Auth.Password = "a\nb" }, "auth.password"},
		{"https with key", func(r *apitypes.CreateProjectRequest) { r.Auth.PrivateKey = "x" }, "auth.private_key"},
		{"none with password", func(r *apitypes.CreateProjectRequest) {
			r.Auth = apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeNone, Password: "x"}
		}, "auth.password"},
		{"ssh on https url", func(r *apitypes.CreateProjectRequest) {
			r.Auth = apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeSSH, PrivateKey: sshKey(t, "")}
		}, "auth.type"},
		{"ssh encrypted key", func(r *apitypes.CreateProjectRequest) {
			r.RepoURL = "git@host:x"
			r.Auth = apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeSSH, PrivateKey: sshKey(t, "pw")}
		}, "auth.private_key"},
		{"ssh garbage key", func(r *apitypes.CreateProjectRequest) {
			r.RepoURL = "git@host:x"
			r.Auth = apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeSSH, PrivateKey: "garbage"}
		}, "auth.private_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := base
			c.mutate(&req)
			e := errBody(t, ph.do("POST", "/api/v1/projects", ph.admin, req), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
			if e.Field != c.field {
				t.Fatalf("field %q, want %q (%s)", e.Field, c.field, e.Message)
			}
			if strings.Contains(e.Message, plantedProjectPassword) {
				t.Fatal("message echoes the password")
			}
		})
	}
	if n, _ := ph.st.CountProjects(context.Background()); n != 0 {
		t.Fatalf("%d projects created by invalid requests", n)
	}
	// Unknown fields inside auth are rejected by the strict decoder.
	errBody(t, ph.do("POST", "/api/v1/projects", ph.admin, `{"name":"x","repo_url":"https://h/x","auth":{"type":"none","token":"y"}}`),
		http.StatusBadRequest, apitypes.ErrorCodeBadRequest)

	ph.wait(ph.create(base).Job)
	errBody(t, ph.do("POST", "/api/v1/projects", ph.admin, base), http.StatusConflict, apitypes.ErrorCodeNameTaken)

	// Branch switch validation.
	id := strconv.FormatInt(1, 10)
	e := errBody(t, ph.do("POST", "/api/v1/projects/"+id+"/branch", ph.dev, apitypes.SwitchBranchRequest{Branch: "--upload-pack=x"}),
		http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "branch" {
		t.Fatalf("field %q", e.Field)
	}
	errBody(t, ph.do("GET", "/api/v1/projects/abc", ph.dev, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	errBody(t, ph.do("GET", "/api/v1/projects/-1", ph.dev, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
}

func TestProjectUpdate(t *testing.T) {
	ph := newProjectHarness(t)
	res := ph.create(httpsProject("game"))
	ph.wait(res.Job)
	path := fmt.Sprintf("/api/v1/projects/%d", res.Project.ID)
	pull := func() fakeCall {
		t.Helper()
		rr := ph.do("POST", path+"/pull", ph.dev, nil)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("pull %d %s", rr.Code, rr.Body.String())
		}
		ph.wait(decode[apitypes.Job](t, rr))
		return ph.git.lastCall()
	}

	// Empty patch: unchanged.
	p := decode[apitypes.Project](t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{}))
	if p.RepoURL != res.Project.RepoURL || !p.HasSecret {
		t.Fatalf("empty patch %+v", p)
	}
	// New URL, auth omitted: the secret is kept and still decrypts.
	newURL := "https://git.example.com/team/game-renamed.git"
	p = decode[apitypes.Project](t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{RepoURL: &newURL}))
	if p.RepoURL != newURL || !p.HasSecret || p.AuthType != apitypes.ProjectAuthTypeHTTPS {
		t.Fatalf("url patch %+v", p)
	}
	if c := pull(); c.remote.URL != newURL || c.remote.Auth.Password != plantedProjectPassword {
		t.Fatalf("pull after url change %+v", c.remote.URL)
	}
	// Replace credentials.
	p = decode[apitypes.Project](t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{
		Auth: &apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeHTTPS, Username: "bot", Password: "new-token"}}))
	if p.AuthUsername != "bot" {
		t.Fatalf("auth patch %+v", p)
	}
	if c := pull(); c.remote.Auth.Username != "bot" || c.remote.Auth.Password != "new-token" {
		t.Fatal("new credentials not used")
	}
	// Switching to an SSH URL needs SSH auth in the same request.
	sshURL := "git@git.example.com:team/game.git"
	e := errBody(t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{RepoURL: &sshURL}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "repo_url" {
		t.Fatalf("field %q", e.Field)
	}
	p = decode[apitypes.Project](t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{RepoURL: &sshURL,
		Auth: &apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeSSH, PrivateKey: sshKey(t, "")}}))
	if p.AuthType != apitypes.ProjectAuthTypeSSH || p.AuthUsername != "" || !p.HasSecret {
		t.Fatalf("ssh patch %+v", p)
	}
	if c := pull(); c.remote.Auth.Method != git.AuthSSH || len(c.remote.Auth.PrivateKey) == 0 {
		t.Fatal("ssh key not used")
	}
	// Clearing credentials explicitly.
	p = decode[apitypes.Project](t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{Auth: &apitypes.ProjectAuth{Type: apitypes.ProjectAuthTypeNone}}))
	if p.AuthType != apitypes.ProjectAuthTypeNone || p.HasSecret {
		t.Fatalf("clear auth %+v", p)
	}
	if c := pull(); c.remote.Auth.Method != git.AuthNone {
		t.Fatal("cleared auth still used")
	}
	bad := "file:///etc"
	e = errBody(t, ph.do("PATCH", path, ph.admin, apitypes.UpdateProjectRequest{RepoURL: &bad}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "repo_url" {
		t.Fatalf("field %q", e.Field)
	}
	errBody(t, ph.do("PATCH", "/api/v1/projects/9999", ph.admin, apitypes.UpdateProjectRequest{}), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

func TestProjectConcurrencyAndDelete(t *testing.T) {
	ph := newProjectHarness(t)
	res := ph.create(httpsProject("game"))
	ph.wait(res.Job)
	path := fmt.Sprintf("/api/v1/projects/%d", res.Project.ID)

	gate := make(chan struct{})
	started := make(chan string, 4)
	ph.git.set(func(f *fakeGit) { f.gate, f.started = gate, started })
	rr := ph.do("POST", path+"/pull", ph.dev, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("pull %d", rr.Code)
	}
	job := decode[apitypes.Job](t, rr)
	<-started
	if p := ph.project(res.Project.ID); p.Status != apitypes.ProjectStatusPulling {
		t.Fatalf("status while pulling %s", p.Status)
	}
	errBody(t, ph.do("POST", path+"/pull", ph.dev, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	errBody(t, ph.do("POST", path+"/branch", ph.dev, apitypes.SwitchBranchRequest{Branch: "feature"}), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	errBody(t, ph.do("DELETE", path, ph.admin, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	close(gate)
	ph.wait(job)
	ph.git.set(func(f *fakeGit) { f.gate, f.started = nil, nil })

	// Containment: the checkout path is <repoDir>/<id>; a symlink there is
	// removed without following it, and siblings are untouched.
	dir := filepath.Join(ph.repoDir, strconv.FormatInt(res.Project.ID, 10))
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(ph.repoDir, strconv.FormatInt(res.Project.ID+1, 10))
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Logf("symlinks unavailable (%v); testing a plain directory", err)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if rr := ph.do("DELETE", path, ph.admin, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("checkout path survived")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("delete followed the symlink")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatal("sibling removed")
	}
	errBody(t, ph.do("DELETE", path, ph.admin, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

func TestProjectFailureStatuses(t *testing.T) {
	ph := newProjectHarness(t)
	authErr := &git.Error{Op: "clone", Reason: git.ReasonAuth, Unchanged: true, Err: fmt.Errorf("git clone exited with status 128")}
	ph.git.set(func(f *fakeGit) { f.cloneErr = authErr })
	res := ph.create(httpsProject("game"))
	j := ph.wait(res.Job)
	if j.Status != apitypes.JobStatusError || j.Detail != authErr.Message() {
		t.Fatalf("failed clone job %+v", j)
	}
	p := ph.project(res.Project.ID)
	if p.Status != apitypes.ProjectStatusError || p.LastCommit != "" {
		t.Fatalf("after failed clone %+v", p)
	}
	path := fmt.Sprintf("/api/v1/projects/%d", p.ID)

	// Without a checkout, pull starts a clone (and branch switch clones the
	// requested branch).
	ph.git.set(func(f *fakeGit) { f.cloneErr = nil })
	rr := ph.do("POST", path+"/branch", ph.dev, apitypes.SwitchBranchRequest{Branch: "develop"})
	job := decode[apitypes.Job](t, rr)
	if job.Kind != apitypes.JobKindProjectClone {
		t.Fatalf("kind %s", job.Kind)
	}
	ph.wait(job)
	if p = ph.project(p.ID); p.Status != apitypes.ProjectStatusIdle || p.Branch != "develop" {
		t.Fatalf("after clone via branch %+v", p)
	}

	// A pull that fails before touching the checkout keeps the status.
	ph.git.set(func(f *fakeGit) { f.pullErr = &git.Error{Op: "pull", Reason: git.ReasonNetwork, Unchanged: true} })
	job = decode[apitypes.Job](t, ph.do("POST", path+"/pull", ph.dev, nil))
	if j := ph.wait(job); j.Detail != (&git.Error{Reason: git.ReasonNetwork}).Message() {
		t.Fatalf("detail %q", j.Detail)
	}
	if p = ph.project(p.ID); p.Status != apitypes.ProjectStatusIdle {
		t.Fatalf("unchanged failure changed status to %s", p.Status)
	}
	// One that fails after changing it marks the project error.
	ph.git.set(func(f *fakeGit) { f.pullErr = &git.Error{Op: "pull", Reason: git.ReasonLocal} })
	ph.wait(decode[apitypes.Job](t, ph.do("POST", path+"/pull", ph.dev, nil)))
	if p = ph.project(p.ID); p.Status != apitypes.ProjectStatusError {
		t.Fatalf("status %s", p.Status)
	}
	// A failed switch to a missing branch keeps the branch.
	ph.git.set(func(f *fakeGit) {
		f.pullErr = nil
		f.switchErr = &git.Error{Op: "branch", Reason: git.ReasonBranchNotFound, Unchanged: true}
	})
	ph.wait(decode[apitypes.Job](t, ph.do("POST", path+"/pull", ph.dev, nil)))
	ph.wait(decode[apitypes.Job](t, ph.do("POST", path+"/branch", ph.dev, apitypes.SwitchBranchRequest{Branch: "nope"})))
	if p = ph.project(p.ID); p.Status != apitypes.ProjectStatusIdle || p.Branch != "develop" {
		t.Fatalf("after failed switch %+v", p)
	}

	// Cancelling a running job never leaves a transient status behind.
	gate := make(chan struct{})
	started := make(chan string, 1)
	ph.git.set(func(f *fakeGit) { f.gate, f.started = gate, started })
	job = decode[apitypes.Job](t, ph.do("POST", path+"/pull", ph.dev, nil))
	<-started
	if rr := ph.do("POST", fmt.Sprintf("/api/v1/jobs/%d/cancel", job.ID), ph.dev, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("cancel %d", rr.Code)
	}
	if j := ph.wait(job); j.Status != apitypes.JobStatusCancelled {
		t.Fatalf("job %+v", j)
	}
	if p = ph.project(p.ID); p.Status == apitypes.ProjectStatusPulling {
		t.Fatal("status stuck at pulling")
	}

	// Branch listing failures answer 503 with the safe message.
	ph.git.set(func(f *fakeGit) {
		f.gate, f.started = nil, nil
		f.listErr = &git.Error{Op: "ls-remote", Reason: git.ReasonAuth, Err: fmt.Errorf("exit 128")}
	})
	e := errBody(t, ph.do("GET", path+"/branches", ph.dev, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	if !strings.Contains(e.Message, "authentication") {
		t.Fatalf("message %q", e.Message)
	}
}

func TestProjectRestartReconcile(t *testing.T) {
	ph := newProjectHarness(t)
	res := ph.create(httpsProject("game"))
	ph.wait(res.Job)
	ctx := context.Background()
	for _, st := range []string{"cloning", "pulling", "switching_branch"} {
		if err := ph.st.SetProjectStatus(ctx, res.Project.ID, st); err != nil {
			t.Fatal(err)
		}
		r, err := ph.st.Reconcile(ctx)
		if err != nil || r.Projects != 1 {
			t.Fatalf("reconcile %+v %v", r, err)
		}
		if p := ph.project(res.Project.ID); p.Status != apitypes.ProjectStatusError {
			t.Fatalf("%s reconciled to %s", st, p.Status)
		}
	}
	// A pull repairs it.
	ph.wait(decode[apitypes.Job](t, ph.do("POST", fmt.Sprintf("/api/v1/projects/%d/pull", res.Project.ID), ph.dev, nil)))
	if p := ph.project(res.Project.ID); p.Status != apitypes.ProjectStatusIdle {
		t.Fatalf("after repair %s", p.Status)
	}
}

func TestProjectGitUnavailable(t *testing.T) {
	ph := newProjectHarness(t)
	ph.srv.proj.git = nil
	errBody(t, ph.do("POST", "/api/v1/projects", ph.admin, httpsProject("game")), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	if n, _ := ph.st.CountProjects(context.Background()); n != 0 {
		t.Fatal("project created without git")
	}
}
