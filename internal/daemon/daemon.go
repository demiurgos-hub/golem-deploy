// Package daemon assembles and runs the golem-deploy server process: data
// directory, keys, store, startup reconciliation, job runner and HTTP
// server, with graceful shutdown in that order reversed.
package daemon

import (
	"context"
	"crypto/tls"
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
	"sync/atomic"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/httpapi"
	"github.com/demiurgos-hub/golem-deploy/internal/inspector"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/staticfiles"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Options configures Run.
type Options struct {
	DataDir       string
	Listen        string
	TLSCert       string
	TLSKey        string
	HTTPListen    string
	ACMEDirectory string
	ACMERootCA    string
	InsecureHTTP  bool
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
	// BackupMaxMB is the largest data directory a backup accepts, in MiB
	// (--backup-max-mb; 0 means 1024).
	BackupMaxMB int
	// Inspector configures the Inspector (the --inspector-* flags).
	Inspector InspectorOptions
	// Ready, if set, is called with the bound address once serving.
	Ready func(addr net.Addr)

	bcryptCost    int                                              // tests only; 0 means auth.DefaultBcryptCost
	docker        httpapi.DockerProbe                              // tests only
	dockerCLI     docker.Runner                                    // tests only; nil runs the docker CLI
	storeClosed   func(error)                                      // tests only
	readyServices func(*jobs.Runner, *build.Service, *store.Store) // tests only
	stopTimeout   time.Duration                                    // tests only
}

// InspectorOptions are the Inspector serve flags.
type InspectorOptions struct {
	// Listen is --inspector-listen ("" derives it from --listen, "off"
	// makes the Inspector unavailable).
	Listen string
	// Image is --inspector-image ("" means inspector.DefaultImage) and
	// MemoryMB --inspector-memory-mb (0 means inspector.DefaultMemoryMB).
	Image    string
	MemoryMB int
	// PublicURL is --inspector-public-url and AgentHost
	// --inspector-agent-host.
	PublicURL string
	AgentHost string
}

// inspectorSettings are the validated Inspector flags.
type inspectorSettings struct {
	listen, image, publicURL, agentHost string
	memoryMB                            int
}

// inspectorConfig validates the Inspector flags. The listener port must
// differ from the panel and HTTP listener ports and lie outside the game
// port range.
func inspectorConfig(o Options, rng ports.Range) (inspectorSettings, error) {
	var out inspectorSettings
	var err error
	if out.listen, err = inspector.ResolveListen(o.Inspector.Listen, o.Listen); err != nil {
		return out, err
	}
	out.image = o.Inspector.Image
	if out.image == "" {
		out.image = inspector.DefaultImage
	}
	if err := inspector.ValidateImage(out.image); err != nil {
		return out, err
	}
	out.memoryMB = o.Inspector.MemoryMB
	if out.memoryMB == 0 {
		out.memoryMB = inspector.DefaultMemoryMB
	}
	if out.memoryMB < 32 || out.memoryMB > 1<<20 {
		return out, errors.New("--inspector-memory-mb must be between 32 and 1048576")
	}
	if out.publicURL, err = inspector.ValidatePublicURL(o.Inspector.PublicURL); err != nil {
		return out, err
	}
	if out.agentHost, err = inspector.ValidateAgentHost(o.Inspector.AgentHost); err != nil {
		return out, err
	}
	port := inspector.ListenPort(out.listen)
	if out.listen == "" || port == 0 {
		return out, nil
	}
	portOf := func(addr string) int {
		if _, p, err := net.SplitHostPort(addr); err == nil {
			n, _ := strconv.Atoi(p)
			return n
		}
		return 0
	}
	switch {
	case port == portOf(o.Listen):
		return out, fmt.Errorf("--inspector-listen port %d is the panel's port (--listen); choose another port or \"off\"", port)
	case o.HTTPListen != "" && port == portOf(o.HTTPListen):
		return out, fmt.Errorf("--inspector-listen port %d is the HTTP listener's port (--http-listen)", port)
	case rng.Contains(port):
		return out, fmt.Errorf("--inspector-listen port %d is inside the game server port range %s", port, rng)
	}
	return out, nil
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
	if _, p, err := net.SplitHostPort(o.HTTPListen); err == nil {
		if port, err := strconv.Atoi(p); err == nil && rng.Contains(port) {
			return servers.Config{}, fmt.Errorf("port range %s contains the HTTP redirect listener port %d", rng, port)
		}
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
	backupMax := o.BackupMaxMB
	if backupMax == 0 {
		backupMax = servers.DefaultBackupMaxBytes >> 20
	}
	if backupMax < 1 || backupMax > 1<<20 {
		return servers.Config{}, errors.New("--backup-max-mb must be between 1 and 1048576")
	}
	return servers.Config{Ports: rng, PanelPort: panel, BindAddress: o.PortBindAddress, MemoryMB: o.ServerMemoryMB, CPUs: o.ServerCPUs,
		StartTimeout: o.StartTimeout, BackupMaxBytes: int64(backupMax) << 20}, nil
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

// shutdownGrace covers HTTP shutdown, jobs, build watchers and maintenance together.
const shutdownGrace = 20 * time.Second

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
	panelMode := apitypes.PanelCertificateModeNone
	var fileCert *tls.Certificate
	switch {
	case o.TLSCert != "":
		panelMode = apitypes.PanelCertificateModeFiles
		pair, err := tls.LoadX509KeyPair(o.TLSCert, o.TLSKey)
		if err != nil {
			return fmt.Errorf("load panel TLS files: %w", err)
		}
		fileCert = &pair
	default:
		if err := httpapi.CheckListen(o.Listen, false, o.InsecureHTTP); errors.Is(err, httpapi.ErrPlaintextNonLoopback) {
			panelMode = apitypes.PanelCertificateModeSelfSigned
		} else if err != nil {
			return err
		}
	}
	panelTLS := panelMode != apitypes.PanelCertificateModeNone
	if err := httpapi.CheckListen(o.Listen, panelTLS, o.InsecureHTTP); err != nil {
		return err
	}
	if o.HTTPListen != "" {
		if !panelTLS {
			return errors.New("--http-listen requires the panel to serve HTTPS")
		}
		if err := httpapi.CheckListen(o.HTTPListen, false, true); err != nil {
			return fmt.Errorf("--http-listen: %w", err)
		}
	}
	if err := certs.ValidateDirectory(o.ACMEDirectory); err != nil {
		return err
	}
	roots, err := certs.LoadACMERoots(o.ACMERootCA)
	if err != nil {
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
	inspCfg, err := inspectorConfig(o, srvCfg.Ports)
	if err != nil {
		return err
	}
	srvCfg.DefaultGameHost = defaultGameHost(publicHosts)
	srvCfg.PanelPublicHosts = publicHosts
	srvCfg.PanelLoopback = httpapi.CheckListen(o.Listen, false, false) == nil
	srvCfg.PanelTLS = panelTLS
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
	// Held until all store users have returned, including on a shutdown timeout.
	lock, err := datadir.AcquireLock(layout)
	if err != nil {
		return err
	}
	var drain <-chan struct{}
	var closeStore func() error
	release := func() error {
		defer func() { _ = lock.Release() }()
		if closeStore != nil {
			return closeStore()
		}
		return nil
	}
	defer func() {
		if drain != nil {
			// A stuck publication may still be using both the files and the
			// store. In the CLI, process exit releases the OS lock and the
			// next start recovers the durable stage. If embedded, retain both
			// until the remaining work returns.
			go func() {
				<-drain
				if err := release(); err != nil {
					logger.Warn("close store after shutdown timeout", "err", err)
				}
			}()
			return
		}
		if cerr := release(); cerr != nil && err == nil {
			err = fmt.Errorf("close store: %w", cerr)
		}
	}()
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
	closeStore = func() error {
		cerr := st.Close()
		if o.storeClosed != nil {
			o.storeClosed(cerr)
		}
		return cerr
	}

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
	backupStore, err := backups.Open(layout.Backups(), backups.Options{})
	if err != nil {
		return err
	}
	// No job runs yet: archives being written when the daemon stopped and
	// archives without a record are leftovers of an interrupted backup.
	if files, err := st.BackupFiles(ctx); err != nil {
		return err
	} else if removed, err := backupStore.Sweep(files); err != nil {
		logger.Warn("could not remove all backup files left by a previous run", "err", err)
	} else if len(removed) > 0 {
		logger.Warn("removed backup files left by an interrupted backup", "files", removed)
	}
	srvCfg.Backups = backupStore
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
	// The panel's Host policy is shared with the Inspector listener. Panel
	// host names derived from the game host come from the certificate
	// coordinator, created below.
	var automationRef atomic.Pointer[certs.Automation]
	policy := netpolicy.New(publicHosts, trusted, func(h string) bool {
		a := automationRef.Load()
		return a != nil && a.IsPanelHost(h)
	})
	var inspTLS *tls.Config
	if panelTLS {
		inspTLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			a := automationRef.Load()
			if a == nil {
				return nil, errors.New("no panel TLS certificate is available yet")
			}
			return a.GetCertificate(hello)
		}}
	}
	insp, err := inspector.New(ctx, inspector.Config{
		Store: st, Secrets: box, Docker: dockerClient, Jobs: runner, Policy: policy, Logger: logger,
		Image: inspCfg.image, MemoryMB: inspCfg.memoryMB, Listen: inspCfg.listen, PublicURL: inspCfg.publicURL,
		AgentHost: inspCfg.agentHost, TLSConfig: inspTLS, GOOS: runtime.GOOS,
	})
	if err != nil {
		return err
	}
	srvCfg.Inspector = insp
	svc := servers.New(st, box, dockerClient, srvCfg, logger)
	certMgr.Check(ctx, svc.GameHost)
	ln, err := httpapi.OpenListener(o.Listen, "--listen")
	if err != nil {
		return err
	}
	defer ln.Close()
	if srvCfg.PanelPort == 0 {
		svc.SetPanelPort(ln.Addr().(*net.TCPAddr).Port)
	}
	var httpLn net.Listener
	var listeners certs.ACMEListeners
	if o.HTTPListen != "" {
		httpLn, err = httpapi.OpenListener(o.HTTPListen, "--http-listen")
		if err != nil {
			return err
		}
		defer httpLn.Close()
		listeners.HTTP = httpLn.Addr().String()
	}
	if panelTLS && ln.Addr().(*net.TCPAddr).Port == 443 {
		listeners.ALPN = ln.Addr().String()
	}
	automation := certs.NewAutomation(certs.AutomationOptions{
		Game: certMgr, GameHost: svc.GameHost, PublicHosts: publicHosts,
		PanelMode: panelMode, PanelDir: layout.PanelCerts(), FileCertificate: fileCert,
		Client:    certs.NewACMEClient(layout.ACME(), roots, logger, listeners),
		Directory: o.ACMEDirectory, HTTP01: listeners.HTTP != "", TLSALPN01: listeners.ALPN != "",
		Jobs: runner,
	})
	if err := automation.Initialize(ctx); err != nil {
		return fmt.Errorf("initialize certificates: %w", err)
	}
	automationRef.Store(automation)
	var probe httpapi.DockerProbe = dockerClient
	if o.docker != nil {
		probe = o.docker
	}
	srv, err := httpapi.New(httpapi.Config{
		Listen:         o.Listen,
		TLSCert:        o.TLSCert,
		TLSKey:         o.TLSKey,
		ManagedTLS:     panelTLS && o.TLSCert == "",
		InsecureHTTP:   o.InsecureHTTP,
		Version:        o.Version,
		PublicHosts:    publicHosts,
		TrustedProxies: trusted,
		Policy:         policy,
	}, httpapi.Deps{
		Store:      st,
		Sessions:   sessions,
		Hasher:     hasher,
		Secrets:    box,
		Jobs:       runner,
		Logger:     logger,
		Docker:     probe,
		Git:        gitClient,
		RepoDir:    layout.Repositories(),
		TempDir:    layout.Tmp(),
		Servers:    svc,
		Static:     static,
		Certs:      certMgr,
		Automation: automation,
		Builds:     builds,
		Inspector:  insp,
		SPA:        o.SPA,
	})
	if err != nil {
		return err
	}
	scheme := "http"
	if panelTLS {
		scheme = "https"
	}
	logger.Info("golem-deploy listening", "addr", ln.Addr().String(), "scheme", scheme, "data_dir", layout.Root, "version", o.Version,
		"port_range", srvCfg.Ports.String())
	if o.Ready != nil {
		o.Ready(ln.Addr())
	}
	if o.readyServices != nil {
		o.readyServices(runner, builds, st)
	}

	// Cancel all background work with HTTP shutdown. Keep their completion
	// signals, since cancelling a context does not join the goroutine.
	recCtx, cancelRec := context.WithCancel(ctx)
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		cleanupHelpers(recCtx, svc, logger)
		reconcileServers(recCtx, svc, runner, logger)
	}()
	scheduleDone := make(chan struct{})
	go func() {
		defer close(scheduleDone)
		<-recDone
		runBackupSchedule(recCtx, svc, runner, logger)
	}()
	defer cancelRec()
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
	// The Inspector repairs its hub and sweeps its sessions until
	// shutdown, then closes its listener (the hub container keeps running).
	inspDone := make(chan struct{})
	go func() {
		defer close(inspDone)
		insp.Run(recCtx)
	}()
	// Certificate maintenance runs until shutdown, including mount retries.
	certDone := make(chan struct{})
	go func() {
		defer close(certDone)
		automation.Maintain(recCtx)
	}()
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	finished := make(chan struct{})
	deadline := make(chan time.Time, 1)
	go func() {
		<-runCtx.Done()
		grace := shutdownGrace
		if o.stopTimeout > 0 {
			grace = o.stopTimeout
		}
		deadline <- time.Now().Add(grace)
		jobsDone := runner.Stop()
		for _, done := range []<-chan struct{}{srv.Drained(), jobsDone, certDone, recDone, cleanupDone, scheduleDone, inspDone} {
			<-done
		}
		builds.WaitWatchers()
		close(finished)
	}()
	serveErr := srv.Serve(runCtx, ln, httpLn)
	stopRun()
	cancelRec()
	drain, err = awaitShutdown(<-deadline, finished)
	logger.Info("golem-deploy stopped")
	return errors.Join(serveErr, err)
}

func awaitShutdown(deadline time.Time, finished <-chan struct{}) (<-chan struct{}, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-finished:
		return nil, nil
	case <-timer.C:
		return finished, fmt.Errorf("daemon work did not finish before the shutdown deadline: %w", context.DeadlineExceeded)
	}
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

// cleanupHelpers removes helper containers a previous run left behind.
func cleanupHelpers(ctx context.Context, svc *servers.Service, logger *slog.Logger) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := svc.CleanupHelpers(cctx); err != nil && ctx.Err() == nil {
		logger.Warn("could not remove all helper containers left by a previous run; retrying later", "err", err)
	}
}

// runBackupSchedule checks the backup schedules every
// servers.ScheduleInterval until ctx ends, and retries the removal of
// leftover helper containers.
func runBackupSchedule(ctx context.Context, svc *servers.Service, runner *jobs.Runner, logger *slog.Logger) {
	start := func(id int64, body func(context.Context, io.Writer) error) error {
		jobID := make(chan int64, 1)
		job, err := runner.Start(ctx, string(apitypes.JobKindServerBackup), jobs.Subject{Type: string(apitypes.SubjectTypeServer), ID: jobs.SubjectID(id)}, nil,
			func(jctx context.Context, log io.Writer) error {
				var jid int64
				select {
				case jid = <-jobID:
				case <-jctx.Done():
				}
				return body(servers.WithJobID(jctx, jid), log)
			})
		if err == nil {
			jobID <- job.ID
			logger.Info("scheduled backup started", "server_id", id, "job_id", job.ID)
		}
		return err
	}
	ticker := time.NewTicker(servers.ScheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := svc.CleanupHelpers(cctx); err != nil && ctx.Err() == nil {
			logger.Debug("helper container cleanup failed", "err", err)
		}
		if _, err := svc.ScheduleBackups(cctx, start); err != nil && ctx.Err() == nil && !errors.Is(err, docker.ErrUnavailable) {
			logger.Warn("could not check the backup schedules", "err", err)
		}
		cancel()
	}
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
