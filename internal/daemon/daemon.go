// Package daemon assembles and runs the golem-deploy server process: data
// directory, keys, store, startup reconciliation, job runner and HTTP
// server, with graceful shutdown in that order reversed.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/httpapi"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Options configures Run.
type Options struct {
	DataDir      string
	Listen       string
	TLSCert      string
	TLSKey       string
	InsecureHTTP bool
	Version      string
	Logger       *slog.Logger
	SPA          http.Handler
	// Ready, if set, is called with the bound address once serving.
	Ready func(addr net.Addr)

	bcryptCost  int                 // tests only; 0 means auth.DefaultBcryptCost
	docker      httpapi.DockerProbe // tests only
	storeClosed func(error)         // tests only
}

// jobShutdownGrace bounds how long running jobs get to stop.
const jobShutdownGrace = 10 * time.Second

// Run serves until ctx is cancelled, then shuts down gracefully and closes
// the store. It returns configuration and startup errors immediately.
func Run(ctx context.Context, o Options) (err error) {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return errors.New("--tls-cert and --tls-key must be set together")
	}
	if err := httpapi.CheckListen(o.Listen, o.TLSCert != "", o.InsecureHTTP); err != nil {
		return err
	}

	layout, err := datadir.Prepare(o.DataDir)
	if err != nil {
		return err
	}
	jwtKey, err := datadir.LoadOrCreateKey(layout.JWTKey(), auth.JWTKeySize)
	if err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(layout.SecretsKey())
	if err != nil {
		return err
	}
	sessions, err := auth.NewSessions(jwtKey, nil)
	if err != nil {
		return err
	}
	cost := o.bcryptCost
	if cost == 0 {
		cost = auth.DefaultBcryptCost
	}
	hasher, err := auth.NewHasher(cost)
	if err != nil {
		return err
	}
	hasher.Dummy() // precompute the dummy hash so the first login is not slower

	st, err := store.Open(ctx, layout.DB())
	if err != nil {
		return err
	}
	defer func() {
		cerr := st.Close()
		if o.storeClosed != nil {
			o.storeClosed(cerr)
		}
		if cerr != nil && err == nil {
			err = fmt.Errorf("close store: %w", cerr)
		}
	}()

	rec, err := st.Reconcile(ctx)
	if err != nil {
		return err
	}
	if rec.Jobs > 0 || rec.Projects > 0 {
		logger.Warn("reconciled state from previous run", "interrupted_jobs", rec.Jobs, "reset_projects", rec.Projects)
	}
	counts, err := st.CountAccounts(ctx)
	if err != nil {
		return err
	}
	if counts.ApprovedAdmins == 0 {
		logger.Warn("no approved admin account exists; create one with: golem-deploy admin create --username <name> --data-dir " + layout.Root)
	}

	gitClient, err := prepareGit(ctx, st, layout, logger)
	if err != nil {
		return err
	}

	runner := jobs.New(st, layout.JobLogs(), jobs.Options{Logger: logger})
	probe := o.docker
	if probe == nil {
		probe = &dockerCLIProbe{}
	}
	srv, err := httpapi.New(httpapi.Config{
		Listen:       o.Listen,
		TLSCert:      o.TLSCert,
		TLSKey:       o.TLSKey,
		InsecureHTTP: o.InsecureHTTP,
		Version:      o.Version,
	}, httpapi.Deps{
		Store:    st,
		Sessions: sessions,
		Hasher:   hasher,
		Secrets:  box,
		Jobs:     runner,
		Logger:   logger,
		Docker:   probe,
		Git:      gitClient,
		RepoDir:  layout.Repositories(),
		TempDir:  layout.Tmp(),
		SPA:      o.SPA,
	})
	if err != nil {
		return err
	}
	ln, err := srv.Listen()
	if err != nil {
		_ = runner.Shutdown(context.Background())
		return err
	}
	scheme := "http"
	if o.TLSCert != "" {
		scheme = "https"
	}
	logger.Info("golem-deploy listening", "addr", ln.Addr().String(), "scheme", scheme, "data_dir", layout.Root, "version", o.Version)
	if o.Ready != nil {
		o.Ready(ln.Addr())
	}

	// On cancellation, HTTP shutdown and job cancellation proceed together;
	// the store is closed (deferred above) only after both have finished.
	var wg sync.WaitGroup
	var jobErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		jctx, cancel := context.WithTimeout(context.Background(), jobShutdownGrace)
		defer cancel()
		jobErr = runner.Shutdown(jctx)
	}()
	serveErr := srv.Serve(ctx, ln)
	if serveErr != nil && ctx.Err() == nil {
		// Serving failed on its own; stop the job goroutine too.
		_ = runner.Shutdown(context.Background())
		return serveErr
	}
	wg.Wait()
	logger.Info("golem-deploy stopped")
	if serveErr != nil {
		return serveErr
	}
	return jobErr
}

// prepareGit sweeps git scratch files and orphaned checkouts left by a
// previous run and returns the git runner, or nil (project git operations
// then answer 503) when git is not installed.
func prepareGit(ctx context.Context, st *store.Store, layout datadir.Layout, logger *slog.Logger) (httpapi.GitClient, error) {
	if err := git.SweepTemp(layout.Tmp()); err != nil {
		logger.Warn("could not remove git scratch files from a previous run", "err", err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]bool, len(projects))
	for _, p := range projects {
		ids[p.ID] = true
	}
	removed, err := git.SweepCheckouts(layout.Repositories(), layout.Tmp(), func(id int64) bool { return ids[id] })
	if len(removed) > 0 {
		logger.Warn("removed checkouts of deleted projects", "project_ids", removed)
	}
	if err != nil {
		logger.Warn("could not remove all orphaned checkouts", "err", err)
	}
	r, err := git.New(git.Options{TempDir: layout.Tmp(), KnownHostsFile: layout.KnownHosts()})
	if err != nil {
		logger.Warn("git is not available; project clone, pull and branch operations are disabled", "err", err)
		return nil, nil
	}
	return r, nil
}

// dockerCLIProbe checks Docker with the docker CLI, caching the answer.
type dockerCLIProbe struct {
	mu      sync.Mutex
	checked time.Time
	ok      bool
}

const dockerProbeTTL = 15 * time.Second

func (p *dockerCLIProbe) Reachable(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.checked) < dockerProbeTTL {
		return p.ok
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p.ok = exec.CommandContext(cctx, "docker", "version", "--format", "{{.Server.Version}}").Run() == nil
	p.checked = time.Now()
	return p.ok
}
