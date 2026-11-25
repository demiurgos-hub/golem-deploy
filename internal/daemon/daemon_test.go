package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/inspector"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

type noDocker struct{}

func (noDocker) Reachable(context.Context) bool { return false }

func TestRunSelectsManagedTLSForNonLoopback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(t.Context())
	ready, done := make(chan net.Addr, 1), make(chan error, 1)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	go func() {
		defer close(stopped)
		done <- Run(ctx, Options{
			DataDir: dir, Listen: "0.0.0.0:0", docker: noDocker{}, Logger: slog.New(slog.DiscardHandler),
			Ready: func(addr net.Addr) { ready <- addr },
		})
	}()
	var port int
	select {
	case addr := <-ready:
		port = addr.(*net.TCPAddr).Port
	case err := <-done:
		t.Fatalf("daemon stopped during startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("managed TLS listener did not start")
	}
	cert, err := os.ReadFile(filepath.Join((datadir.Layout{Root: dir}).PanelCerts(), "fallback.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		t.Fatal("fallback certificate could not be loaded")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	resp, err := client.Get("https://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/auth/options")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public HTTPS endpoint returned %s", resp.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	if err := Run(context.Background(), Options{DataDir: dir, Listen: "127.0.0.1:0", TLSCert: "c.pem"}); err == nil {
		t.Fatal("cert without key accepted")
	}
	if err := Run(t.Context(), Options{DataDir: dir, Listen: "127.0.0.1:0", HTTPListen: "127.0.0.1:0"}); err == nil {
		t.Fatal("HTTP redirect listener without HTTPS accepted")
	}
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	// A fresh path: t.TempDir() itself may be group/world-readable under the
	// umask, which Prepare refuses on Unix.
	dir := filepath.Join(t.TempDir(), "data")
	// A job left running by a crashed daemon.
	layout, err := datadir.Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := st.CreateJob(context.Background(), store.NewJob{Kind: "project.pull", SubjectType: "project", SubjectID: "1"})
	_ = st.MarkJobRunning(context.Background(), stale.ID)
	_ = st.Close()

	var logs strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	closed := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			DataDir:     dir,
			Listen:      "127.0.0.1:0",
			Version:     "v-test",
			Logger:      slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)),
			Ready:       func(a net.Addr) { addrCh <- a },
			bcryptCost:  bcrypt.MinCost,
			docker:      noDocker{},
			dockerCLI:   dockertest.New(),
			storeClosed: func(err error) { closed <- err },
		})
	}()
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("Run exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}
	resp, err := http.Get("http://" + addr.String() + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"unauthenticated"`) {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	for _, f := range []string{layout.JWTKey(), layout.SecretsKey()} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("key not created: %v", err)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("store close: %v", err)
		}
	default:
		t.Fatal("store was not closed")
	}

	out := logs.String()
	for _, want := range []string{"no approved admin account exists", "interrupted_jobs=1", "golem-deploy stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	st, err = store.Open(context.Background(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	j, _ := st.GetJob(context.Background(), stale.ID)
	if j.Status != store.JobError || j.Detail != store.InterruptedDetail {
		t.Fatalf("stale job %+v", j)
	}
}

func TestAdminCreateAndReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx := context.Background()
	acct, err := createAdmin(ctx, dir, "Root-Admin", "a-very-good-password", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Username != "root-admin" || acct.Role != store.RoleAdmin || !acct.Approved {
		t.Fatalf("account %+v", acct)
	}
	if _, err := createAdmin(ctx, dir, "root-admin", "a-very-good-password", bcrypt.MinCost); !errors.Is(err, ErrUsernameExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := createAdmin(ctx, dir, "x", "a-very-good-password", bcrypt.MinCost); err == nil {
		t.Fatal("bad username accepted")
	}
	if _, err := createAdmin(ctx, dir, "other", "short", bcrypt.MinCost); err == nil {
		t.Fatal("short password accepted")
	}
	if _, err := resetPassword(ctx, dir, "ROOT-ADMIN", "another-good-password", bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	if _, err := resetPassword(ctx, dir, "ghost", "another-good-password", bcrypt.MinCost); !errors.Is(err, ErrNoSuchAccount) {
		t.Fatalf("missing user: %v", err)
	}
	layout, _ := datadir.Prepare(dir)
	st, err := store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, _ := st.GetAccount(ctx, acct.ID)
	if got.TokenVersion != acct.TokenVersion+1 || bcrypt.CompareHashAndPassword([]byte(got.PasswordHash), []byte("another-good-password")) != nil {
		t.Fatalf("reset not applied: %+v", got)
	}
}

// TestF5_CLIResetPasswordDeletesTokens: the local recovery action ends
// sessions and deletes every API token of the account.
func TestF5_CLIResetPasswordDeletesTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx := context.Background()
	acct, err := createAdmin(ctx, dir, "root", "a-very-good-password", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := datadir.Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"h1", "h2"} {
		if _, err := st.CreateAPIToken(ctx, store.NewAPIToken{AccountID: acct.ID, Name: "ci", TokenHash: h, Prefix: "gdp_x"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	n, err := resetPassword(ctx, dir, "root", "another-good-password", bcrypt.MinCost)
	if err != nil || n != 2 {
		t.Fatalf("reset: %d %v", n, err)
	}
	st, err = store.Open(ctx, layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if toks, _ := st.ListAPITokens(ctx, acct.ID); len(toks) != 0 {
		t.Fatalf("tokens survived the reset: %d", len(toks))
	}
}

func TestShutdownDeadlineKeepsStoreAndLockForCertificateJob(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	submitted := make(chan store.Job, 1)
	submitErrors := make(chan error, 1)
	closed, stopped := make(chan error, 1), make(chan error, 1)
	go func() {
		stopped <- Run(ctx, Options{
			DataDir: dir, Listen: "127.0.0.1:0", docker: noDocker{}, dockerCLI: dockertest.New(),
			Logger: slog.New(slog.DiscardHandler), bcryptCost: bcrypt.MinCost, stopTimeout: 50 * time.Millisecond,
			storeClosed: func(err error) { closed <- err },
			readyServices: func(r *jobs.Runner, _ *build.Service, _ *store.Store) {
				j, err := r.Start(ctx, "certificate.renew", jobs.Subject{Type: "certificate", ID: "acme"}, nil,
					func(ctx context.Context, _ io.Writer) error {
						close(started)
						<-release
						return ctx.Err()
					})
				submitted <- j
				submitErrors <- err
			},
		})
	}()
	select {
	case <-started:
	case err := <-stopped:
		t.Fatalf("daemon exited before starting the job: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("job did not start")
	}
	cancel()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown did not report its deadline: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not return while the certificate job was blocked")
	}
	select {
	case err := <-closed:
		t.Fatalf("store closed while job was active: %v", err)
	default:
	}
	layout := datadir.Layout{Root: dir}
	if lock, err := datadir.AcquireLock(layout); !errors.Is(err, datadir.ErrLocked) {
		if err == nil {
			_ = lock.Release()
		}
		t.Fatalf("another daemon could enter while job was active: %v", err)
	}
	finish()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("store did not close after the job drained")
	}
	st, err := store.Open(t.Context(), layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := <-submitErrors; err != nil {
		t.Fatal(err)
	}
	j, err := st.GetJob(t.Context(), (<-submitted).ID)
	if err != nil || j.Status != store.JobCancelled {
		t.Fatalf("job could not save its final status: %+v %v", j, err)
	}
}

func TestShutdownJoinsQueuedBuildWatcherBeforeClosingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan net.Addr, 1)
	stopped, closed := make(chan error, 1), make(chan error, 1)
	created := make(chan int64, 1)
	watching, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	startedBuild := make(chan struct{}, 1)
	go func() {
		stopped <- Run(ctx, Options{
			DataDir: dir, Listen: "127.0.0.1:0", docker: noDocker{}, dockerCLI: dockertest.New(),
			Logger: slog.New(slog.DiscardHandler), bcryptCost: bcrypt.MinCost, stopTimeout: 50 * time.Millisecond,
			storeClosed: func(err error) { closed <- err },
			Ready:       func(addr net.Addr) { ready <- addr },
			readyServices: func(r *jobs.Runner, builds *build.Service, st *store.Store) {
				setup := func() (int64, error) {
					for i := range build.MaxActiveBuilds {
						_, err := r.Start(ctx, "project.build", jobs.Subject{Type: "project", ID: strconv.Itoa(i)}, nil,
							func(ctx context.Context, _ io.Writer) error { <-ctx.Done(); return ctx.Err() }, jobs.InPool(build.JobPool))
						if err != nil {
							return 0, err
						}
					}
					project, err := st.CreateProject(ctx, store.Project{Name: "arena", RepoURL: "https://example.com/g.git"}, nil)
					if err != nil {
						return 0, err
					}
					record, err := st.CreateBuild(ctx, store.NewBuild{ProjectID: project.ID})
					if err != nil {
						return 0, err
					}
					job, err := r.Start(ctx, "project.build", jobs.Subject{Type: "project", ID: "queued"}, nil,
						func(context.Context, io.Writer) error { startedBuild <- struct{}{}; return nil }, jobs.InPool(build.JobPool))
					if err != nil {
						return 0, err
					}
					if err := st.SetBuildJob(ctx, record.ID, job.ID); err != nil {
						return 0, err
					}
					builds.Watch(record.ID, r.Done(job.ID), func() {
						close(watching)
						<-release
					})
					return record.ID, nil
				}
				id, err := setup()
				if err != nil {
					t.Error(err)
				}
				created <- id
			},
		})
	}()
	select {
	case <-ready:
	case err := <-stopped:
		t.Fatalf("daemon exited before readiness: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not report readiness")
	}
	id := <-created
	if id == 0 {
		t.Fatal("queued build was not created")
	}
	cancel()
	select {
	case <-watching:
	case <-time.After(5 * time.Second):
		t.Fatal("queued build watcher did not wake after cancellation")
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("daemon finished before the watcher: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not observe the shutdown deadline")
	}
	select {
	case err := <-closed:
		t.Fatalf("store closed while build watcher was blocked: %v", err)
	default:
	}
	finish()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("store did not close after the watcher finished")
	}
	select {
	case <-startedBuild:
		t.Fatal("the queued build started at shutdown")
	default:
	}
	st, err := store.Open(t.Context(), (datadir.Layout{Root: dir}).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	record, err := st.GetBuild(t.Context(), id)
	if err != nil || record.Status != store.BuildStatusCancelled || record.Detail != jobs.DetailShutdown {
		t.Fatalf("queued build did not save its final status: %+v, %v", record, err)
	}
}

// The Inspector flags are validated before anything is opened; the
// listener port may not collide with the panel, the HTTP listener or the
// game port range.
func TestInspectorConfig(t *testing.T) {
	rng := ports.Range{Lo: 7000, Hi: 7099}
	base := Options{Listen: "127.0.0.1:8000"}
	got, err := inspectorConfig(base, rng)
	if err != nil || got.listen != "127.0.0.1:8444" || got.image != inspector.DefaultImage || got.memoryMB != inspector.DefaultMemoryMB {
		t.Fatalf("%+v %v", got, err)
	}
	off := base
	off.Inspector.Listen = "off"
	if got, err := inspectorConfig(off, rng); err != nil || got.listen != "" {
		t.Fatalf("%+v %v", got, err)
	}
	for name, mut := range map[string]func(*Options){
		"panel port":      func(o *Options) { o.Inspector.Listen = ":8000" },
		"http port":       func(o *Options) { o.HTTPListen = ":8080"; o.Inspector.Listen = "0.0.0.0:8080" },
		"game range":      func(o *Options) { o.Inspector.Listen = ":7050" },
		"bad listen":      func(o *Options) { o.Inspector.Listen = "8444" },
		"bad image":       func(o *Options) { o.Inspector.Image = "-rm" },
		"small memory":    func(o *Options) { o.Inspector.MemoryMB = 16 },
		"bad public url":  func(o *Options) { o.Inspector.PublicURL = "https://x.example/path" },
		"bad agent host":  func(o *Options) { o.Inspector.AgentHost = "h:1" },
		"derived on 8444": func(o *Options) { o.Listen = "127.0.0.1:8000" },
	} {
		o := base
		mut(&o)
		_, err := inspectorConfig(o, rng)
		if name == "derived on 8444" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	collide := base
	if _, err := inspectorConfig(collide, ports.Range{Lo: 8400, Hi: 8500}); err == nil {
		t.Error("derived port inside the game range accepted")
	}
	pub := base
	pub.Inspector.PublicURL = "https://Inspector.Example.com/"
	if got, err := inspectorConfig(pub, rng); err != nil || got.publicURL != "https://inspector.example.com" {
		t.Fatalf("%+v %v", got, err)
	}
}
