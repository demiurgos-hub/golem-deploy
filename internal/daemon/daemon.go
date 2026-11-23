// Package daemon assembles and runs the golem-deploy server process: data
// directory, keys, store, startup reconciliation, job runner and HTTP
// server, with graceful shutdown in that order reversed.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/httpapi"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Options configures Run.
type Options struct {
	DataDir      string
	Listen       string
	TLSCert      string
	TLSKey       string
	InsecureHTTP bool
	// PublicHosts are host names accepted in the Host header besides IP
	// literals and localhost (--public-host).
	PublicHosts []string
	// TrustedProxies are CIDRs (or addresses) of reverse proxies whose
	// X-Forwarded-For is believed (--trusted-proxy).
	TrustedProxies []string
	Version        string
	Logger         *slog.Logger
	SPA            http.Handler
	// PortRange is the host port range for game servers ("LO-HI"; default
	// ports.DefaultRange).
	PortRange string
	// PortBindAddress is the host IP game ports are published on; empty
	// means all interfaces.
	PortBindAddress string
	// ServerMemoryMB and ServerCPUs limit every server container (0 means
	// unlimited).
	ServerMemoryMB int
	ServerCPUs     float64
	// MaxRepoSizeMB bounds the size of each project checkout in MiB
	// (--max-repo-size-mb); 0 means unlimited.
	MaxRepoSizeMB int
	// Ready, if set, is called with the bound address once serving.
	Ready func(addr net.Addr)

	bcryptCost  int                 // tests only; 0 means auth.DefaultBcryptCost
	docker      httpapi.DockerProbe // tests only
	dockerCLI   docker.Runner       // tests only; nil runs the docker CLI
	storeClosed func(error)         // tests only
}

// serverConfig validates the game server settings.
func serverConfig(o Options) (servers.Config, error) {
	spec := o.PortRange
	if spec == "" {
		spec = ports.DefaultRange
	}
	rng, err := ports.Parse(spec)
	if err != nil {
		return servers.Config{}, err
	}
	panel := 0
	if _, p, err := net.SplitHostPort(o.Listen); err == nil {
		panel, _ = strconv.Atoi(p)
	}
	if err := rng.Validate(panel); err != nil {
		return servers.Config{}, err
	}
	if o.PortBindAddress != "" {
		if ip, err := netip.ParseAddr(o.PortBindAddress); err != nil || ip.Zone() != "" {
			return servers.Config{}, fmt.Errorf("--port-bind-address %q is not an IP address", o.PortBindAddress)
		}
	}
	if o.ServerMemoryMB < 0 || (o.ServerMemoryMB > 0 && o.ServerMemoryMB < 8) {
		return servers.Config{}, errors.New("--server-memory-mb must be 0 (unlimited) or at least 8")
	}
	if o.ServerCPUs < 0 || o.ServerCPUs > 1024 {
		return servers.Config{}, errors.New("--server-cpus must be between 0 (unlimited) and 1024")
	}
	return servers.Config{Ports: rng, BindAddress: o.PortBindAddress, MemoryMB: o.ServerMemoryMB, CPUs: o.ServerCPUs}, nil
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
	publicHosts, err := httpapi.NormalizePublicHosts(o.PublicHosts)
	if err != nil {
		return err
	}
	trusted, err := httpapi.ParseTrustedProxies(o.TrustedProxies)
	if err != nil {
		return err
	}
	srvCfg, err := serverConfig(o)
	if err != nil {
		return err
	}
	if o.MaxRepoSizeMB < 0 {
		return errors.New("--max-repo-size-mb must be 0 (unlimited) or positive")
	}
	if err := checkDataDirPath(o.DataDir); err != nil {
		return err
	}

	layout, err := datadir.Prepare(o.DataDir)
	if err != nil {
		return err
	}
	// Held until Run returns (released last, after the store is closed);
	// taken before reconciliation and the startup sweeps, which would
	// otherwise fail or delete the live state of another daemon.
	lock, err := datadir.AcquireLock(layout)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
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

	gitClient, err := prepareGit(ctx, st, layout, int64(o.MaxRepoSizeMB)<<20, logger)
	if err != nil {
		return err
	}

	static, err := staticfiles.Open(layout.Static())
	if err != nil {
		return err
	}
	runner := jobs.New(st, layout.JobLogs(), jobs.Options{Logger: logger})
	dockerClient := docker.New(docker.Options{Runner: o.dockerCLI, Logger: logger})
	svc := servers.New(st, box, dockerClient, srvCfg, logger)
	var probe httpapi.DockerProbe = dockerClient
	if o.docker != nil {
		probe = o.docker
	}
	srv, err := httpapi.New(httpapi.Config{
		Listen:         o.Listen,
		TLSCert:        o.TLSCert,
		TLSKey:         o.TLSKey,
		InsecureHTTP:   o.InsecureHTTP,
		Version:        o.Version,
		PublicHosts:    publicHosts,
		TrustedProxies: trusted,
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
		Servers:  svc,
		Static:   static,
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
	logger.Info("golem-deploy listening", "addr", ln.Addr().String(), "scheme", scheme, "data_dir", layout.Root, "version", o.Version,
		"port_range", srvCfg.Ports.String())
	if o.Ready != nil {
		o.Ready(ln.Addr())
	}

	// On cancellation, HTTP shutdown and job cancellation proceed together;
	// the store is closed (deferred above) only after both have finished.
	var wg sync.WaitGroup
	var jobErr error
	// Startup reconciliation uses the store; it is cancelled and awaited
	// before the store closes on every exit path.
	recCtx, cancelRec := context.WithCancel(ctx)
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		reconcileServers(recCtx, svc, runner, logger)
	}()
	defer func() {
		cancelRec()
		<-recDone
	}()
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
func prepareGit(ctx context.Context, st *store.Store, layout datadir.Layout, maxRepoSize int64, logger *slog.Logger) (httpapi.GitClient, error) {
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
	r, err := git.New(git.Options{TempDir: layout.Tmp(), KnownHostsFile: layout.KnownHosts(), MaxRepoSize: maxRepoSize})
	if err != nil {
		logger.Warn("git is not available; project clone, pull and branch operations are disabled", "err", err)
		return nil, nil
	}
	return r, nil
}

// checkDataDirPath refuses a data directory whose absolute path cannot be
// written safely into the per-operation ssh_config of git operations (the
// SSH key and known_hosts paths live below it).
func checkDataDirPath(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	if err := git.CheckSSHConfigPath(abs); err != nil {
		return fmt.Errorf("data directory %q cannot be used: %w (it is written into the SSH configuration of git operations); choose another --data-dir", abs, err)
	}
	return nil
}

// reconcileServers starts, as jobs, the servers whose desired state is
// running but whose container is not (the job runner bounds concurrency).
func reconcileServers(ctx context.Context, svc *servers.Service, runner *jobs.Runner, logger *slog.Logger) {
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	err := svc.Reconcile(rctx, func(id int64) error {
		_, err := runner.Start(rctx, string(apitypes.JobKindServerStart), jobs.Subject{Type: string(apitypes.SubjectTypeServer), ID: jobs.SubjectID(id)}, nil,
			func(jctx context.Context, log io.Writer) error { return svc.Start(jctx, id, log) })
		return err
	})
	if err != nil && ctx.Err() == nil {
		logger.Warn("server reconciliation skipped", "err", err)
	}
}
