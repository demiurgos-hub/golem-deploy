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
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
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
	// unlimited; the serve flags default to 2048 MiB and 2 CPUs).
	ServerMemoryMB int
	ServerCPUs     float64
	// MaxRepoSizeMB bounds the size of each project checkout in MiB
	// (--max-repo-size-mb); 0 means unlimited.
	MaxRepoSizeMB int
	// StaticQuotaMB caps the static file store (0 means unlimited; the
	// serve flag defaults to 5120).
	StaticQuotaMB int
	// StartTimeout bounds how long a server start waits for the game to
	// report ready (--start-timeout; 0 means servers.DefaultStartTimeout).
	StartTimeout time.Duration
	// BuildTimeout bounds one build (--build-timeout; 0 means
	// build.DefaultTimeout) and BuildRetention is how many successful
	// builds are kept per project (--build-retention; 0 means
	// build.DefaultRetention).
	BuildTimeout   time.Duration
	BuildRetention int
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
	if o.StaticQuotaMB < 0 || o.StaticQuotaMB > 1<<30 {
		return servers.Config{}, errors.New("--static-quota-mb must be 0 (unlimited) or a positive number of MiB")
	}
	if o.StartTimeout != 0 && (o.StartTimeout < time.Second || o.StartTimeout > 30*time.Minute) {
		return servers.Config{}, errors.New("--start-timeout must be between 1s and 30m")
	}
	return servers.Config{Ports: rng, PanelPort: panel, BindAddress: o.PortBindAddress, MemoryMB: o.ServerMemoryMB, CPUs: o.ServerCPUs,
		StartTimeout: o.StartTimeout}, nil
}

// defaultGameHost is the first --public-host usable as a game host.
func defaultGameHost(publicHosts []string) string {
	for _, h := range publicHosts {
		if v, err := certs.ValidateHost(h); err == nil && v != "" {
			return v
		}
	}
	return ""
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
	if runtime.GOOS == "windows" {
		logger.Warn("Windows is a development platform only. Data directory permissions are not checked or restricted here. Keys and temporary environment files inherit the data directory ACL. Use Linux for production.")
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
	srvCfg.DefaultGameHost = defaultGameHost(publicHosts)
	srvCfg.PanelPublicHosts = publicHosts
	srvCfg.PanelLoopback = httpapi.CheckListen(o.Listen, false, false) == nil
	srvCfg.PanelTLS = panelUsesTLS(o, srvCfg.PanelLoopback)
	srvCfg.PanelBehindProxy = o.InsecureHTTP
	if o.MaxRepoSizeMB < 0 {
		return errors.New("--max-repo-size-mb must be 0 (unlimited) or positive")
	}
	if o.BuildTimeout == 0 {
		o.BuildTimeout = build.DefaultTimeout
	}
	if o.BuildTimeout < time.Minute || o.BuildTimeout > 24*time.Hour {
		return errors.New("--build-timeout must be between 1m and 24h")
	}
	if o.BuildRetention == 0 {
		o.BuildRetention = build.DefaultRetention
	}
	if o.BuildRetention < 1 || o.BuildRetention > 100 {
		return errors.New("--build-retention must be between 1 and 100")
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
	if n, err := st.ReconcileBuilds(ctx); err != nil {
		return err
	} else if n > 0 {
		logger.Warn("builds interrupted by the previous run were marked as failed", "builds", n)
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

	static, err := staticfiles.Open(layout.Static(), staticfiles.Options{QuotaBytes: int64(o.StaticQuotaMB) << 20})
	if err != nil {
		return err
	}
	// Builds run on their own pool: a waiting build holds a worker of that
	// pool (build.MaxActiveBuilds at most), so builds never delay server
	// starts and stops or other project jobs on the default pool.
	runner := jobs.New(st, layout.JobLogs(), jobs.Options{Logger: logger, Workers: 4, Pools: map[string]int{build.JobPool: build.MaxActiveBuilds}})
	instanceID, err := st.InstanceID(ctx)
	if err != nil {
		return err
	}
	if err := docker.SweepEnvFiles(layout.Tmp()); err != nil {
		logger.Warn("could not remove env files left by a previous run", "err", err)
	}
	if err := build.SweepTemp(layout.Tmp()); err != nil {
		logger.Warn("could not remove build directories left by a previous run", "err", err)
	}
	dockerClient := docker.New(docker.Options{Runner: o.dockerCLI, Logger: logger, InstanceID: instanceID,
		TempDir: layout.Tmp(), BuildConfigDir: layout.BuildDockerConfig()})
	if err := dockerClient.PrepareBuildConfig(); err != nil {
		return fmt.Errorf("prepare dedicated Docker build configuration: %w", err)
	}
	if err := docker.ValidateBindSource(layout.GameCerts()); err != nil {
		return fmt.Errorf("the data directory %q cannot be bind-mounted into server containers (its path must not contain , \" ' or =): %w", layout.Root, err)
	}
	certMgr, err := certs.Open(certs.Options{Dir: layout.GameCerts(), Settings: st, Logger: logger})
	if err != nil {
		return err
	}
	srvCfg.Certs = certMgr
	if eph, overlap, ok := ports.EphemeralOverlap(srvCfg.Ports, os.ReadFile); ok && overlap {
		logger.Warn("the game server port range overlaps the host's ephemeral port range; outgoing connections and Docker can take those ports at any time: "+
			"choose another --port-range or raise net.ipv4.ip_local_port_range", "port_range", srvCfg.Ports.String(), "ephemeral_range", eph.String())
	}
	var headCommit func(context.Context, string) (string, error)
	if gitRunner, ok := gitClient.(*git.Runner); ok {
		headCommit = gitRunner.HeadCommit
	}
	builds := build.New(build.Options{
		Store:      st,
		Docker:     dockerClient,
		InstanceID: instanceID,
		RepoDir:    layout.Repositories(),
		TempDir:    layout.Tmp(),
		StaticDir:  static.Dir(),
		HeadCommit: headCommit,
		Timeout:    o.BuildTimeout,
		Retention:  o.BuildRetention,
		Logger:     logger,
	})
	svc := servers.New(st, box, dockerClient, srvCfg, logger)
	certMgr.Check(ctx, svc.GameHost)
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
		Certs:    certMgr,
		Builds:   builds,
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
	if srvCfg.PanelPort == 0 {
		svc.SetPanelPort(ln.Addr().(*net.TCPAddr).Port)
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
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			cctx, cancel := context.WithTimeout(recCtx, 2*time.Minute)
			builds.CleanupImages(cctx)
			cancel()
			select {
			case <-recCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() {
		cancelRec()
		<-cleanupDone
	}()
	// The certificate check runs hourly until shutdown, before the store
	// closes.
	certDone := make(chan struct{})
	go func() {
		defer close(certDone)
		certMgr.Maintain(recCtx, svc.GameHost)
	}()
	defer func() {
		cancelRec()
		<-certDone
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

// panelUsesTLS mirrors the panel TLS policy for the origins handed to games.
func panelUsesTLS(o Options, loopback bool) bool {
	return o.TLSCert != "" || (!o.InsecureHTTP && !loopback)
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

// reconcileServers submits startup verification and recovery as server jobs.
func reconcileServers(ctx context.Context, svc *servers.Service, runner *jobs.Runner, logger *slog.Logger) {
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	err := svc.Reconcile(rctx, func(id int64) error {
		jobID := make(chan int64, 1)
		job, err := runner.Start(rctx, string(apitypes.JobKindServerStart), jobs.Subject{Type: string(apitypes.SubjectTypeServer), ID: jobs.SubjectID(id)}, nil,
			func(jctx context.Context, log io.Writer) error {
				var jid int64
				select {
				case jid = <-jobID:
				case <-jctx.Done():
				}
				return svc.Recover(servers.WithJobID(jctx, jid), id, log)
			})
		if err == nil {
			jobID <- job.ID
		}
		return err
	})
	if err != nil && ctx.Err() == nil {
		logger.Warn("server reconciliation skipped", "err", err)
	}
}
