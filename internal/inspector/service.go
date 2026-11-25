// Package inspector runs the Golem Inspector of the installation: the hub
// as a managed container that only the daemon talks to, the Inspector
// listener (browsers through one-time tickets and daemon-held sessions
// that are reverse-proxied to the hub; games' Inspector agents relayed to
// the hub with per-server tokens), the inspector.apply job that brings the
// hub in line with its setting, and the per-server agent settings the
// servers service injects. It has no package-level mutable state.
package inspector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Defaults of the serve flags.
const (
	DefaultImage    = "ghcr.io/demiurgos-hub/golem-inspector:0.4.0-rc.1"
	DefaultMemoryMB = 512
	DefaultPort     = 8444
	// ListenOff as --inspector-listen makes the Inspector unavailable.
	ListenOff = "off"
)

// Timing.
const (
	// DefaultReadyTimeout bounds the wait for a new hub to answer ready.
	DefaultReadyTimeout = 60 * time.Second
	// RepairInterval is the least time between an automatic repair and the
	// failed attempt before it, and between two looks for a hub container
	// left behind while the Inspector is off.
	RepairInterval = 15 * time.Minute
	// cleanupRetry is the least time between two attempts to remove what a
	// turn-off left (the listener, the hub container).
	cleanupRetry = time.Minute
	// targetsTTL caches the hub's target list.
	targetsTTL     = 2 * time.Second
	targetsTimeout = 3 * time.Second
	// hubSpecVersion changes whenever the hub's container definition
	// changes meaning, so the next apply replaces the container.
	hubSpecVersion = "inspector-hub-1"
	// hubLogPrefix starts every line of hub output copied into a job log.
	hubLogPrefix = "hub | "
)

// Errors returned to the HTTP layer.
var (
	// ErrUnavailable: the daemon runs with --inspector-listen off.
	ErrUnavailable = errors.New("inspector: the Inspector is unavailable")
	// ErrNotRunning: the Inspector is not serving.
	ErrNotRunning = errors.New("inspector: the Inspector is not running")
	// ErrServerNotFound: the server of a session request does not exist.
	ErrServerNotFound = errors.New("inspector: server not found")
	// ErrTooManyTickets: too many unconsumed entry links.
	ErrTooManyTickets = errTooManyTickets
)

// FieldError is a rejected request field; Msg is safe to show to users.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Config configures the Service.
type Config struct {
	Store   *store.Store
	Secrets *secrets.Box
	Docker  *docker.Client
	Jobs    *jobs.Runner
	// Policy is the panel's Host and client address policy.
	Policy *netpolicy.Policy
	Logger *slog.Logger
	// Image is --inspector-image (validated) and MemoryMB the hub's memory
	// limit (--inspector-memory-mb).
	Image    string
	MemoryMB int
	// Listen is the listener address (ResolveListen); "" makes the
	// Inspector unavailable.
	Listen string
	// PublicURL is --inspector-public-url (ValidatePublicURL); "" when no
	// reverse proxy fronts the listener.
	PublicURL string
	// AgentHost overrides the address games' agents connect to
	// (--inspector-agent-host).
	AgentHost string
	// TLSConfig, when set, makes the listener serve TLS to browsers (the
	// panel's certificate) and plaintext to agents on the same port.
	TLSConfig *tls.Config
	// GOOS is the daemon's operating system (default runtime.GOOS by the
	// daemon); it selects the agent address.
	GOOS string
	// Now overrides the clock (tests).
	Now func() time.Time
	// ReadyTimeout bounds the wait for a new hub (DefaultReadyTimeout).
	ReadyTimeout time.Duration
	// BcryptCost is the cost of the hub password hash (HubBcryptCost).
	BcryptCost int
}

// / Service is the installation's Inspector. It is safe for concurrent use.
type Service struct {
	cfg        Config
	logger     *slog.Logger
	proxyLog   *log.Logger
	hub        *hubClient
	proxy      *httputil.ReverseProxy
	agentProxy *httputil.ReverseProxy
	tickets    *ticketStore
	sessions   *sessionStore
	agents     *agentRegistry
	entryLimit *auth.Limiter
	guard      loginGuard
	// loginSem serialises hub logins.
	loginSem chan struct{}
	agentLog sampled
	bg       sync.WaitGroup
	// sniffTimeout bounds the wait for a connection's first byte,
	// reqTimeout a request that is not a relayed WebSocket, and maxConns
	// the open connections of the listener (tests lower them).
	sniffTimeout time.Duration
	reqTimeout   time.Duration
	maxConns     int
	// publicOrigin is --inspector-public-url in canonical form.
	publicOrigin string
	// inspectHub reads the hub container, listHubs lists the hub
	// containers and netQuery the managed network's addresses (Docker;
	// tests replace them).
	inspectHub func(ctx context.Context, id string) (docker.Container, error)
	listHubs   func(ctx context.Context) ([]docker.Container, error)
	netQuery   func(ctx context.Context) (docker.NetworkAddresses, error)
	// stepTimeout bounds each step of a sweep and handshakeTimeout a
	// relayed WebSocket upgrade until the hub's valid 101 (tests lower
	// them).
	stepTimeout      time.Duration
	handshakeTimeout time.Duration
	// pages are the running relays of the hub's own pages.
	pages *cancelSet
	// hooks are test hooks; all nil outside tests.
	hooks testHooks
	// settingMu orders writes and reads of the setting with the copy in
	// enabled, so memory never runs ahead of the database.
	settingMu sync.Mutex

	// mu guards the fields below. It is never held across a call to
	// Docker or the hub.
	mu      sync.Mutex
	enabled bool
	sec     *hubSecrets
	// hubContainer and hubPort are the hub container and its loopback port.
	// The port is valid only for that container while it runs: it is
	// cleared as soon as the daemon learns that the container stopped,
	// went or changed, when Docker does not answer, and on any transport
	// error towards it; hubVerified is when Docker last answered that it
	// runs and publishes the port. hubSeq changes with every change of
	// these fields (and of hubSick), so the result of a check that started
	// before a change is discarded (observeHub).
	hubContainer string
	hubPort      int
	hubVerified  time.Time
	hubSeq       uint64
	// hubSick is the hub container that failed hubSickAfter readiness
	// checks in a row and hubFailures the failures so far: the port of a
	// sick hub is not restored from Docker's metadata, and the repair
	// replaces the container.
	hubSick     string
	hubFailures int
	detail      string
	ln          *listener
	// admitting and gen gate the registration of tickets, sessions and
	// agent relays: a registration needs admission open and the generation
	// it started with. gen changes whenever admission closes or the
	// secrets change, before what was registered is drained.
	admitting bool
	gen       uint64
	// lastAttempt is when the last attempt to start the hub failed (zero
	// after a success). cleanupDue is set while a listener or a hub may be
	// left although the Inspector is off; lastCleanup is the last attempt
	// to remove them and lastStray the last look for a hub left behind.
	lastAttempt time.Time
	cleanupDue  bool
	lastCleanup time.Time
	lastStray   time.Time
	closed      bool
	bgClosed    bool

	// netGen changes whenever the cached addresses are dropped or
	// forgotten, so a query that started before cannot store its answer.
	netMu     sync.Mutex
	netAddrs  docker.NetworkAddresses
	netErr    error
	netAt     time.Time
	netGen    uint64
	netFlight flight

	statusMu      sync.Mutex
	statusSID     string
	targetsAt     time.Time
	targets       []hubTarget
	targetsOK     bool
	targetsFlight flight

	hubFlight flight
}

// testHooks let tests act at fixed points of a request.
type testHooks struct {
	// afterEntryLogin runs in a ticket exchange after the hub login and
	// before the session is registered.
	afterEntryLogin func()
	// afterAgentCheck runs in an agent relay after the token and server
	// checks and before the slot is taken.
	afterAgentCheck func()
	// beforeTicket runs in CreateSession before the ticket is issued.
	beforeTicket func()
	// afterKeyCheck runs in a proxied request after the key was found and
	// before the body is read.
	afterKeyCheck func()
	// beforeHubLogin runs in a hub login after the guard and Docker checks.
	beforeHubLogin func()
	// beforeStoreEnabled runs in Update before the setting is written.
	beforeStoreEnabled func()
	// afterAgentHost runs in Agent after the agent address is known.
	afterAgentHost func()
	// afterSettingRead runs in Update after the setting was read.
	afterSettingRead func()
	// beforePublish runs in enable before the hub is published as serving.
	beforePublish func()
	// afterTicketConsumed runs in a ticket exchange after the ticket was
	// consumed and the account checked.
	afterTicketConsumed func()
	// afterReadyCheck runs in checkHub after the hub answered (or not) and
	// before the result is recorded.
	afterReadyCheck func()
}

// The hub port. The password is sent only after a Docker check made for
// that login, on a connection of its own. A hub session or the hub's agent
// token is sent only while Docker confirmed the port within
// hubCheckInterval of its answer; an older confirmation is renewed first,
// and traffic causes at most one Docker check per hubCheckInterval. The
// 30-second sweep asks the hub's readiness endpoint (hubReadyTimeout);
// hubSickAfter failures in a row make the hub "not answering". Each Docker
// call that checks the hub gets hubDockerTimeout, each step of the sweep
// sweepStepTimeout.
const (
	hubCheckInterval = 10 * time.Second
	hubReadyTimeout  = 3 * time.Second
	hubSickAfter     = 3
	hubDockerTimeout = 10 * time.Second
	sweepStepTimeout = 15 * time.Second
	// sickDetail is the state's detail for a hub that does not answer.
	sickDetail = "the Inspector hub is running but does not answer; it is repaired automatically or when the Inspector is applied again"
)

// errHubUnverified: Docker did not confirm the hub port recently enough to
// send a hub session or the hub's agent token to it.
var errHubUnverified = errors.New("inspector: the hub port is not confirmed by Docker")

// New loads the Inspector setting and secrets. It opens nothing; Run
// repairs a hub that should be on and removes one that should not exist.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Secrets == nil || cfg.Policy == nil {
		return nil, errors.New("inspector: Store, Secrets and Policy are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.BcryptCost == 0 {
		cfg.BcryptCost = HubBcryptCost
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = DefaultMemoryMB
	}
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	s := &Service{
		cfg:        cfg,
		logger:     cfg.Logger,
		proxyLog:   slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelDebug),
		hub:        newHubClient(),
		tickets:    newTicketStore(cfg.Now),
		sessions:   newSessionStore(cfg.Now),
		agents:     newAgentRegistry(),
		entryLimit: auth.NewLimiter(entryFailuresPerWindow, entryWindow, 50000, cfg.Now),
		loginSem:   make(chan struct{}, 1),
		agentLog:   sampled{every: time.Minute, now: cfg.Now},

		sniffTimeout: firstByteTimeout,
		reqTimeout:   requestTimeout,
		maxConns:     maxConns,

		stepTimeout:      sweepStepTimeout,
		handshakeTimeout: wsHandshakeTimeout,
		pages:            newCancelSet(),
	}
	if cfg.PublicURL != "" {
		o, err := canonicalOrigin(cfg.PublicURL)
		if err != nil {
			return nil, err
		}
		s.publicOrigin = o
	}
	if cfg.Docker != nil {
		s.inspectHub = cfg.Docker.InspectorHub
		s.listHubs = cfg.Docker.InspectorHubs
		s.netQuery = cfg.Docker.ManagedNetworkAddresses
	}
	s.proxy = s.newBrowserProxy()
	s.agentProxy = s.newAgentProxy()
	v, _, err := cfg.Store.GetSetting(ctx, store.SettingInspectorEnabled)
	if err != nil {
		return nil, err
	}
	s.enabled, _ = strconv.ParseBool(v)
	sec, err := loadSecrets(ctx, cfg.Store, cfg.Secrets)
	if err != nil {
		// New secrets are generated by the next apply; the hub is
		// replaced then (a new generation).
		cfg.Logger.Error("could not read the Inspector secrets; they are replaced when the Inspector is applied again", "err", err)
	}
	s.sec = sec
	return s, nil
}

// hubView is one consistent snapshot of what a request needs to talk to
// the hub: the admission generation, the secrets, the hub container and
// its loopback port.
type hubView struct {
	gen       uint64
	sec       *hubSecrets
	container string
	port      int
}

// view returns the current snapshot and whether the Inspector serves
// (setting on, listener open, admission open, a verified hub port). A
// recorded hub whose port was dropped is checked again in the background.
func (s *Service) view() (hubView, bool) {
	s.mu.Lock()
	v := hubView{gen: s.gen, sec: s.sec, container: s.hubContainer, port: s.hubPort}
	ok := s.enabled && s.ln != nil && s.admitting && v.sec != nil && v.port != 0
	kick := s.enabled && s.ln != nil && v.container != "" && v.port == 0
	s.mu.Unlock()
	if kick {
		s.kickRefresh()
	}
	return v, ok
}

// hubNow returns the snapshot without the serving checks.
func (s *Service) hubNow() hubView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hubView{gen: s.gen, sec: s.sec, container: s.hubContainer, port: s.hubPort}
}

// admit runs fn (which registers a ticket, a session or a relay) under the
// admission lock, only while admission is open with generation gen. fn
// must not take s.mu.
func (s *Service) admit(gen uint64, fn func() bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.admitting || s.gen != gen || !s.enabled || s.closed {
		return false
	}
	return fn()
}

// goBG runs fn in the background unless the Service finished closing;
// Close waits for it.
func (s *Service) goBG(fn func()) {
	s.mu.Lock()
	if s.bgClosed {
		s.mu.Unlock()
		return
	}
	s.bg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

// kickRefresh checks the hub container's port again in the background, at
// most once per hubCheckInterval.
func (s *Service) kickRefresh() {
	s.goBG(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.refreshHub(ctx, hubCheckInterval)
	})
}

// hubSeen is what one Docker check learned about a hub container.
type hubSeen int

const (
	// seenRunning: it runs and publishes the hub port on the loopback
	// port given.
	seenRunning hubSeen = iota
	// seenStopped: it exists but does not run, does not publish the hub
	// port, or is not the current hub container any more.
	seenStopped
	// seenGone: it does not exist, or it is not this installation's hub.
	seenGone
	// seenNoAnswer: Docker did not answer (an error, a timeout, a
	// cancelled context). This says nothing about the hub.
	seenNoAnswer
)

// hubCheck is the start of one check of hub container id: the change
// counter when it started.
type hubCheck struct {
	id  string
	seq uint64
}

// startCheck starts a check of hub container id.
func (s *Service) startCheck(id string) hubCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hubCheck{id: id, seq: s.hubSeq}
}

// seenOf classifies a Docker answer about the hub container.
func seenOf(ct docker.Container, err error) (hubSeen, int) {
	switch {
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotOurs):
		return seenGone, 0
	case err != nil:
		return seenNoAnswer, 0
	case !ct.Running:
		return seenStopped, 0
	}
	if p := ct.LoopbackPort(docker.InspectorHubPort); p != 0 {
		return seenRunning, p
	}
	return seenStopped, 0
}

// observeHub records what check c learned about the recorded hub; at is
// when Docker answered. It is the only place that records a Docker check.
// A result is discarded when the recorded hub is another container or
// anything about it changed since c started (hubSeq). Positive evidence
// that the hub is gone or wrong drops the port (seenGone also forgets the
// container); no answer leaves the port unverified (dropped until a later
// check confirms it) and is no evidence about the hub; a confirmation
// keeps or restores the port Docker reports, except for a hub that does
// not answer (hubSick), and is valid for hubCheckInterval from at. It
// reports whether the result was recorded.
func (s *Service) observeHub(c hubCheck, seen hubSeen, port int, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.id == "" || s.hubContainer != c.id || s.hubSeq != c.seq {
		return false
	}
	if seen == seenRunning && s.hubSick != c.id {
		if s.hubPort != port {
			s.hubPort = port
			s.hubSeq++
		}
		if at.After(s.hubVerified) {
			s.hubVerified = at
		}
		return true
	}
	if s.hubPort != 0 || !s.hubVerified.IsZero() || seen == seenGone {
		s.hubPort, s.hubVerified = 0, time.Time{}
		if seen == seenGone {
			s.hubContainer = ""
		}
		s.hubSeq++
	}
	return true
}

// recentLocked reports whether Docker confirmed the recorded hub's port
// within hubCheckInterval of now. s.mu is held.
func (s *Service) recentLocked(now time.Time) bool {
	age := now.Sub(s.hubVerified)
	return s.hubPort != 0 && s.hubContainer != "" && !s.hubVerified.IsZero() && age >= 0 && age < hubCheckInterval
}

// confirmedRecently reports whether Docker confirmed v's container and port
// (still the recorded ones) within hubCheckInterval.
func (s *Service) confirmedRecently(v hubView) bool {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return v.port != 0 && v.container != "" && s.hubContainer == v.container && s.hubPort == v.port && s.recentLocked(now)
}

// freshHub reports whether a hub session, the hub's agent token or a relayed
// request may go to v's port now: Docker confirmed v's container and port
// within hubCheckInterval. Every send that carries a secret asks it right
// before. An older confirmation is renewed first; concurrent callers share
// one Docker call, which is skipped when a confirmation arrived meanwhile.
func (s *Service) freshHub(ctx context.Context, v hubView) bool {
	if s.confirmedRecently(v) {
		return true
	}
	s.refreshHub(ctx, 0)
	return s.confirmedRecently(v)
}

// hubUnreachable handles err from a call to the hub at port: unless ctx
// ended (the caller left), the port is dropped until Docker confirms it
// again. It returns err marked as a transport error.
func (s *Service) hubUnreachable(ctx context.Context, port int, err error) error {
	if ctx.Err() != nil {
		return err
	}
	dropped := false
	s.mu.Lock()
	if port != 0 && s.hubPort == port {
		s.hubPort, s.hubVerified = 0, time.Time{}
		s.hubSeq++
		dropped = true
	}
	s.mu.Unlock()
	if dropped {
		s.logger.Warn("the Inspector hub did not answer; its port is checked again through Docker", "err", err)
		s.kickRefresh()
	}
	if isTransport(err) {
		return err
	}
	return &transportError{err}
}

// verifyHub confirms through Docker, every time, that container id runs and
// publishes the hub port on exactly port (a hub login sends the password
// only after it). What Docker answers is recorded (observeHub). The Docker
// call has its own time budget and does not end when the caller leaves.
func (s *Service) verifyHub(ctx context.Context, id string, port int) error {
	if s.inspectHub == nil {
		return docker.ErrUnavailable
	}
	c := s.startCheck(id)
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubDockerTimeout)
	defer cancel()
	ct, err := s.inspectHub(qctx, id)
	seen, got := seenOf(ct, err)
	s.observeHub(c, seen, got, s.cfg.Now())
	switch {
	case err != nil:
		return err
	case seen != seenRunning:
		return errors.New("inspector: the hub container does not run")
	case got != port:
		return errors.New("inspector: the hub container does not publish the recorded port")
	}
	return nil
}

// Available reports whether the Inspector can be turned on.
func (s *Service) Available() bool { return s.cfg.Listen != "" }

// configuredPort is the port of --inspector-listen (0 when unavailable).
func (s *Service) configuredPort() int {
	_, p, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// port is the bound port while the listener is open, else the configured
// port.
func (s *Service) port() int {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		return ln.port()
	}
	return s.configuredPort()
}

// agentHost returns the host games' agents connect to. reachable is false
// when containers cannot reach the listener at all (a loopback listener on
// Linux); known is false while the address cannot be told (the managed
// network does not exist yet).
func (s *Service) agentHost(ctx context.Context) (host string, reachable, known bool) {
	if !s.Available() {
		return "", false, false
	}
	if s.cfg.AgentHost != "" {
		return s.cfg.AgentHost, true, true
	}
	lh, _, _ := net.SplitHostPort(s.cfg.Listen)
	ip, ipErr := netip.ParseAddr(lh)
	wildcard := lh == "" || (ipErr == nil && ip.IsUnspecified())
	loopback := strings.EqualFold(lh, "localhost") || (ipErr == nil && ip.IsLoopback())
	if s.cfg.GOOS != "linux" {
		// Docker Desktop: containers reach the host by this name.
		if wildcard || loopback {
			return "host.docker.internal", true, true
		}
		return lh, true, true
	}
	switch {
	case loopback:
		return "", false, false
	case wildcard:
		addrs, err := s.networkAddrs(ctx, false)
		if err != nil {
			return "", true, false
		}
		var v6 netip.Addr
		for _, g := range addrs.Gateways {
			if g.Is4() {
				return g.String(), true, true
			}
			if !v6.IsValid() {
				v6 = g
			}
		}
		if v6.IsValid() {
			return v6.String(), true, true
		}
		return "", true, false
	}
	return lh, true, true
}

// / Agent implements servers.Inspector: the agent endpoint, the token key
// generation and the token function of that same generation, in one
// snapshot. It follows the desired configuration (setting on, secrets, an
// address containers can reach), not whether the hub runs right now: a
// server started while the hub is down still gets the variables, and its
// agent connects once the hub is repaired. The address can need Docker:
// after it, the setting and the secrets are checked again and the port is
// taken in one step under the lock, so a turn-off or a replacement of the
// secrets meanwhile gives nothing rather than stale or mixed values.
func (s *Service) Agent(ctx context.Context) (servers.InspectorAgent, bool) {
	s.mu.Lock()
	enabled, sec := s.enabled, s.sec
	s.mu.Unlock()
	if !enabled || sec == nil || !s.Available() {
		return servers.InspectorAgent{}, false
	}
	host, reachable, known := s.agentHost(ctx)
	if !reachable || !known {
		return servers.InspectorAgent{}, false
	}
	if s.hooks.afterAgentHost != nil {
		s.hooks.afterAgentHost()
	}
	s.mu.Lock()
	same := s.enabled && s.sec == sec
	port := s.configuredPort()
	if s.ln != nil {
		port = s.ln.port()
	}
	s.mu.Unlock()
	if !same || port == 0 {
		return servers.InspectorAgent{}, false
	}
	return servers.InspectorAgent{
		HubURL:     "ws://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/ws/agent",
		Generation: sec.Generation,
		Token:      func(id int64) (string, error) { return agentToken(sec.TokenKey, id) },
	}, true
}

// AgentToken returns the agent token of server id under the current
// secrets.
func (s *Service) AgentToken(id int64) (string, error) {
	s.mu.Lock()
	sec := s.sec
	s.mu.Unlock()
	if sec == nil {
		return "", errors.New("inspector: no secrets")
	}
	return agentToken(sec.TokenKey, id)
}

// Status reports the Inspector (GET /api/v1/inspector).
func (s *Service) Status(ctx context.Context) (apitypes.Inspector, error) {
	s.mu.Lock()
	enabled, detail, ln, hubID, hubPort, sick := s.enabled, s.detail, s.ln, s.hubContainer, s.hubPort, s.hubSick
	s.mu.Unlock()
	out := apitypes.Inspector{Available: s.Available(), Enabled: enabled, State: apitypes.InspectorStateOff, Image: s.cfg.Image,
		PublicURL: s.cfg.PublicURL}
	if out.Available {
		out.Port = s.port()
		_, out.AgentsReachable, _ = s.agentHost(ctx)
	}
	if !enabled {
		return out, nil
	}
	if a, ok := s.Agent(ctx); ok {
		if u, err := url.Parse(a.HubURL); err == nil {
			out.AgentAddress = u.Host
		}
	}
	if s.listHubs == nil {
		out.State = apitypes.InspectorStateUnknown
		return out, nil
	}
	// The listing counts as a check of the recorded hub: what it shows is
	// recorded like any other Docker answer, a confirmation included.
	c := s.startCheck(hubID)
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubDockerTimeout)
	hubs, err := s.listHubs(qctx)
	cancel()
	at := s.cfg.Now()
	if err != nil {
		s.observeHub(c, seenNoAnswer, 0, at)
		if ctx.Err() != nil {
			return apitypes.Inspector{}, ctx.Err()
		}
		if !errors.Is(err, docker.ErrUnavailable) {
			s.logger.Warn("docker query failed; reporting the Inspector as unknown", "err", err)
		}
		out.State = apitypes.InspectorStateUnknown
		return out, nil
	}
	var hub, rec *docker.Container
	if len(hubs) > 0 {
		hub = &hubs[len(hubs)-1]
		out.ImageID = hub.ImageID
		if hub.Running {
			t := hub.StartedAt
			out.StartedAt = &t
		}
	}
	for i := range hubs {
		if hubs[i].ID == hubID {
			rec = &hubs[i]
		}
	}
	recorded := rec != nil && rec.Running && hub.ID == hubID
	if hubID != "" {
		switch {
		case rec == nil:
			// The recorded hub container is gone: never use its port again.
			s.observeHub(c, seenGone, 0, at)
		case !recorded:
			// It stopped, or another hub container is the current one.
			s.observeHub(c, seenStopped, 0, at)
		default:
			seen, port := seenOf(*rec, nil)
			s.observeHub(c, seen, port, at)
		}
		now := s.hubNow()
		recorded = recorded && now.container == hubID
		hubPort = now.port
	}
	switch {
	case recorded && ln != nil && hubPort != 0:
		out.State = apitypes.InspectorStateRunning
		out.Targets, out.OtherTargets = s.targetView(ctx)
	default:
		out.State = apitypes.InspectorStateStopped
		out.Detail = detail
		if out.Detail == "" {
			switch {
			case recorded && sick == hubID:
				out.Detail = sickDetail
			case s.applying():
				out.Detail = "the Inspector is being applied"
			case hub == nil:
				out.Detail = "the Inspector hub container does not exist"
			case !hub.Running:
				out.Detail = "the Inspector hub container is not running"
			case ln == nil:
				out.Detail = "the Inspector listener is not open"
			default:
				out.Detail = "the daemon cannot reach the Inspector hub; it is repaired automatically or when the Inspector is applied again"
			}
		}
	}
	return out, nil
}

func (s *Service) applying() bool {
	return s.cfg.Jobs != nil && s.cfg.Jobs.Busy(subject())
}

func subject() jobs.Subject {
	return jobs.Subject{Type: string(apitypes.SubjectTypeInspector), ID: "hub"}
}

// targetView reads the hub's targets (cached) and matches them to servers
// with the Inspector switch on, by name.
func (s *Service) targetView(ctx context.Context) ([]apitypes.InspectorTarget, int) {
	list, ok := s.hubTargets(ctx)
	if !ok {
		return nil, 0
	}
	rows, err := s.cfg.Store.ListServers(ctx)
	if err != nil {
		return nil, 0
	}
	byName := map[string]int64{}
	for _, r := range rows {
		if r.Inspector {
			byName[r.Name] = r.ID
		}
	}
	out := []apitypes.InspectorTarget{}
	seen := map[int64]bool{}
	other := 0
	for _, t := range list {
		id, ok := byName[t.TargetID]
		if !ok || seen[id] {
			other++
			continue
		}
		seen[id] = true
		v := apitypes.InspectorTarget{ServerID: id, Connected: t.Connected, Entities: max(t.EntityCount, 0)}
		if t.Connected && t.ConnectedAt != nil && !t.ConnectedAt.IsZero() {
			at := t.ConnectedAt.UTC()
			v.ConnectedAt = &at
		}
		out = append(out, v)
	}
	return out, other
}

// hubTargets returns the hub's target list through the daemon's own hub
// session, cached for targetsTTL. Concurrent callers share one fetch; no
// lock is held while it talks to the hub.
func (s *Service) hubTargets(ctx context.Context) ([]hubTarget, bool) {
	fresh := func() bool {
		return !s.targetsAt.IsZero() && s.cfg.Now().Sub(s.targetsAt) < targetsTTL
	}
	s.statusMu.Lock()
	if fresh() {
		defer s.statusMu.Unlock()
		return s.targets, s.targetsOK
	}
	s.statusMu.Unlock()
	v, serving := s.view()
	if !serving {
		return nil, false
	}
	s.targetsFlight.do(ctx, s.cfg.Now, 0, func() {
		s.statusMu.Lock()
		done := fresh()
		sid := s.statusSID
		s.statusMu.Unlock()
		if done {
			return
		}
		// The Docker checks have their own budget; each request to the
		// hub gets targetsTimeout from when it is sent.
		base := context.WithoutCancel(ctx)
		fetch := func() ([]hubTarget, error) {
			if sid == "" {
				lctx, cancel := context.WithTimeout(base, 15*time.Second)
				n, err := s.hubLoginFor(lctx, v, targetsTimeout)
				cancel()
				if err != nil {
					return nil, err
				}
				sid = n
				s.statusMu.Lock()
				s.statusSID = n
				s.statusMu.Unlock()
			}
			if !s.freshHub(base, v) {
				return nil, errHubUnverified
			}
			tctx, cancel := context.WithTimeout(base, targetsTimeout)
			defer cancel()
			list, err := s.hub.targets(tctx, v.port, sid)
			if isTransport(err) {
				_ = s.hubUnreachable(tctx, v.port, err)
			}
			return list, err
		}
		list, err := fetch()
		if errors.Is(err, errHubAuth) {
			sid = ""
			list, err = fetch()
		}
		now := s.cfg.Now()
		s.statusMu.Lock()
		s.targets, s.targetsOK, s.targetsAt = list, err == nil, now
		s.statusMu.Unlock()
	})
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.targets, s.targetsOK
}

// CreateSession issues a one-time entry link for acct (POST
// /api/v1/inspector/sessions). The ticket is registered only while the
// generation the request started with is current.
func (s *Service) CreateSession(ctx context.Context, acct store.Account, serverID *int64) (apitypes.InspectorSession, error) {
	target := ""
	if serverID != nil {
		sv, err := s.cfg.Store.GetServer(ctx, *serverID)
		if errors.Is(err, store.ErrNotFound) {
			return apitypes.InspectorSession{}, ErrServerNotFound
		}
		if err != nil {
			return apitypes.InspectorSession{}, err
		}
		if !sv.Inspector {
			return apitypes.InspectorSession{}, &FieldError{Field: "server_id", Msg: "the server's Inspector setting is off; turn it on and restart the server"}
		}
		target = sv.Name
	}
	v, serving := s.view()
	if !serving {
		return apitypes.InspectorSession{}, ErrNotRunning
	}
	if s.hooks.beforeTicket != nil {
		s.hooks.beforeTicket()
	}
	var tok string
	var exp time.Time
	var err error
	if !s.admit(v.gen, func() bool {
		tok, exp, err = s.tickets.issue(acct.ID, acct.TokenVersion, target, v.gen)
		return true
	}) {
		return apitypes.InspectorSession{}, ErrNotRunning
	}
	if err != nil {
		return apitypes.InspectorSession{}, err
	}
	// The ticket travels in the fragment: never in a request line, a
	// Referer or a log. The target stays bound to the ticket.
	return apitypes.InspectorSession{Path: EnterPath + "#t=" + tok, Port: s.port(), PublicURL: s.cfg.PublicURL, ExpiresAt: exp.UTC()}, nil
}

// applyRequest is what one inspector.apply job does besides following the
// setting, which the job reads when it runs.
type applyRequest struct {
	pull   bool
	rotate bool
	// auto marks the automatic repair: it never downloads an image.
	auto bool
}

// Update validates a PATCH, stores the setting and starts the restricted
// inspector.apply job (PATCH /api/v1/inspector). The setting is written
// first, and only when the request has enabled; a failed write fails the
// request and starts nothing. Turning it off takes effect before the
// answer (storeEnabled). When the job cannot start after the write
// (another job started meanwhile), the repair cycle brings the hub in line
// with the stored setting later.
func (s *Service) Update(ctx context.Context, req apitypes.UpdateInspectorRequest, by *int64) (store.Job, error) {
	if !s.Available() {
		return store.Job{}, ErrUnavailable
	}
	cur, err := s.loadEnabled(ctx)
	if err != nil {
		return store.Job{}, err
	}
	if s.hooks.afterSettingRead != nil {
		s.hooks.afterSettingRead()
	}
	want := cur
	if req.Enabled != nil {
		want = *req.Enabled
	}
	if !want && req.PullImage {
		return store.Job{}, &FieldError{Field: "pull_image", Msg: "pull_image needs the Inspector to be on"}
	}
	if !want && req.RotateSecrets {
		return store.Job{}, &FieldError{Field: "rotate_secrets", Msg: "rotate_secrets needs the Inspector to be on"}
	}
	if s.cfg.Jobs == nil {
		return store.Job{}, jobs.ErrShuttingDown
	}
	if s.applying() {
		return store.Job{}, jobs.ErrSubjectBusy
	}
	if req.Enabled != nil {
		if s.hooks.beforeStoreEnabled != nil {
			s.hooks.beforeStoreEnabled()
		}
		if err := s.storeEnabled(context.WithoutCancel(ctx), want); err != nil {
			return store.Job{}, err
		}
	}
	return s.startApply(ctx, applyRequest{pull: req.PullImage, rotate: req.RotateSecrets}, by)
}

// storeEnabled writes the setting, then its copy in memory. Off takes
// effect at once: admission closes (a new generation) and every ticket,
// session, agent relay and stream ends before it returns; the job then
// closes the listener and removes the hub container (cleanupDue until it
// did).
func (s *Service) storeEnabled(ctx context.Context, on bool) error {
	s.settingMu.Lock()
	defer s.settingMu.Unlock()
	if err := s.cfg.Store.SetSetting(ctx, store.SettingInspectorEnabled, strconv.FormatBool(on)); err != nil {
		return err
	}
	s.mu.Lock()
	s.enabled = on
	if !on {
		s.detail = ""
		s.admitting = false
		s.gen++
		s.cleanupDue = true
	}
	s.mu.Unlock()
	if !on {
		s.drain()
	}
	return nil
}

// loadEnabled reads the setting and updates its copy in memory.
func (s *Service) loadEnabled(ctx context.Context) (bool, error) {
	s.settingMu.Lock()
	defer s.settingMu.Unlock()
	v, _, err := s.cfg.Store.GetSetting(ctx, store.SettingInspectorEnabled)
	if err != nil {
		return false, err
	}
	on, _ := strconv.ParseBool(v)
	s.mu.Lock()
	s.enabled = on
	s.mu.Unlock()
	return on, nil
}

// startApply starts an inspector.apply job (restricted).
func (s *Service) startApply(ctx context.Context, want applyRequest, by *int64) (store.Job, error) {
	if s.cfg.Jobs == nil {
		return store.Job{}, jobs.ErrShuttingDown
	}
	return s.cfg.Jobs.Start(ctx, string(apitypes.JobKindInspectorApply), subject(), by, func(jctx context.Context, w io.Writer) error {
		return s.apply(jctx, w, want)
	}, jobs.Restricted(true))
}

// apply is the body of inspector.apply. It reads the setting when it runs:
// with the Inspector on (and available) it brings the hub up, otherwise it
// removes everything. After a failed start the Inspector is "stopped" with
// a detail and nothing talks to the hub until an apply succeeds.
func (s *Service) apply(ctx context.Context, w io.Writer, want applyRequest) error {
	on, err := s.loadEnabled(ctx)
	if err != nil {
		s.noteAttempt(false)
		return jobs.Fail("could not read the Inspector setting", err)
	}
	if !on || !s.Available() {
		return s.cleanup(ctx, w)
	}
	err = s.enable(ctx, w, want)
	if errors.Is(err, errTurnedOff) {
		fmt.Fprintln(w, "the Inspector was turned off while it was being applied")
		return s.cleanup(ctx, w)
	}
	defer func() { s.noteAttempt(err == nil) }()
	var early unchangedError
	if errors.As(err, &early) {
		err = early.err
		if _, serving := s.view(); serving {
			// Nothing was replaced yet (the image could not be downloaded):
			// a hub that serves keeps serving.
			fmt.Fprintln(w, "the running Inspector was left as it is")
			return err
		}
	}
	if err != nil {
		msg := "the Inspector could not be started"
		var se *jobs.SafeError
		if errors.As(err, &se) {
			msg = se.Msg
		} else if ctx.Err() != nil {
			msg = "the last attempt to start the Inspector was cancelled"
		}
		s.stopServing(msg)
	}
	return err
}

// errTurnedOff: the setting was turned off while enable ran; the hub was
// not published as serving.
var errTurnedOff = errors.New("inspector: the Inspector was turned off meanwhile")

// noteAttempt records the outcome of an attempt to start the hub: the
// automatic repair waits RepairInterval after a failed one.
func (s *Service) noteAttempt(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.lastAttempt = time.Time{}
	} else {
		s.lastAttempt = s.cfg.Now()
	}
}

// cleanup runs disable and records the attempt: a failed removal stays due
// (cleanupDue) and is tried again after cleanupRetry.
func (s *Service) cleanup(ctx context.Context, w io.Writer) error {
	s.mu.Lock()
	s.lastCleanup = s.cfg.Now()
	s.mu.Unlock()
	err := s.disable(ctx, w)
	s.mu.Lock()
	s.cleanupDue = err != nil
	s.mu.Unlock()
	return err
}

// unchangedError marks a failure of enable that happened before anything
// of the current hub was rotated, replaced or removed.
type unchangedError struct{ err error }

func (e unchangedError) Error() string { return e.err.Error() }
func (e unchangedError) Unwrap() error { return e.err }

// stopServing records a failed start: admission closes (a new generation),
// the hub is forgotten (requests get 503, the repair cycle tries again),
// every ticket, session, agent relay and stream ends, and the state is
// "stopped" with msg.
func (s *Service) stopServing(msg string) {
	s.mu.Lock()
	s.hubContainer, s.hubPort, s.hubVerified = "", 0, time.Time{}
	s.hubFailures = 0
	s.hubSeq++
	s.detail = msg
	s.admitting = false
	s.gen++
	s.mu.Unlock()
	s.drain()
}

// failDocker logs a Docker failure to the daemon log and returns a job
// failure with a fixed message; nothing from Docker reaches the job log.
func (s *Service) failDocker(action string, err error) error {
	s.logger.Warn("docker command failed", "subject", "inspector", "action", action, "err", err)
	switch {
	case errors.Is(err, docker.ErrUnavailable):
		return jobs.Fail("Docker is unreachable", docker.ErrUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		return jobs.Fail("Docker did not answer in time while trying to "+action, docker.ErrUnavailable)
	case errors.Is(err, docker.ErrConflict):
		return jobs.Fail("a container named "+docker.InspectorHubName+" already exists and this installation does not manage it; "+
			"check what it is, then remove it and apply the Inspector again", docker.ErrConflict)
	case errors.Is(err, context.Canceled):
		return err
	}
	return jobs.Fail("Docker could not "+action+"; see the daemon log", nil)
}

// disable forgets the hub, closes the listener, ends every session and
// agent connection and removes the hub container. A removal that fails is
// tried again by the repair cycle.
func (s *Service) disable(ctx context.Context, w io.Writer) error {
	s.setHub("", 0)
	s.closeListener()
	fmt.Fprintln(w, "the Inspector listener is closed; Inspector sessions and agent connections ended")
	if err := s.removeHubs(ctx, w, "the Inspector is off"); err != nil {
		return err
	}
	fmt.Fprintln(w, "the Inspector is off")
	return nil
}

// removeHubs removes every hub container of this installation.
func (s *Service) removeHubs(ctx context.Context, w io.Writer, why string) error {
	if s.cfg.Docker == nil {
		return nil
	}
	hubs, err := s.cfg.Docker.InspectorHubs(ctx)
	if err != nil {
		return s.failDocker("list the Inspector hub container", err)
	}
	for _, h := range hubs {
		if err := s.cfg.Docker.RemoveInspectorHub(ctx, h.ID); err != nil {
			return s.failDocker("remove the Inspector hub container", err)
		}
		fmt.Fprintf(w, "removed the Inspector hub container %s (%s)\n", shortID(h.ID), why)
	}
	return nil
}

// notIsolatedMsg says how to get a network without traffic between
// containers.
const notIsolatedMsg = "the " + docker.NetworkName + " network still allows traffic between containers, so game servers could reach the Inspector hub; " +
	"stop every server, then restart the daemon (the network is then recreated without that traffic) and apply the Inspector again"

// enable brings the hub up and opens the listener. The image is resolved
// (or downloaded) before anything is rotated or replaced.
func (s *Service) enable(ctx context.Context, w io.Writer, want applyRequest) error {
	if !s.Available() {
		return jobs.Fail("the Inspector is unavailable: the daemon runs with --inspector-listen off", nil)
	}
	if s.cfg.Docker == nil {
		return jobs.Fail("Docker is not configured", nil)
	}
	img, err := s.ensureImage(ctx, w, want)
	if err != nil {
		return unchangedError{err}
	}
	isolated, err := s.cfg.Docker.EnsureNetwork(ctx)
	switch {
	case errors.Is(err, docker.ErrNotManaged):
		s.dropNetwork(err)
		return jobs.Fail("a Docker network named "+docker.NetworkName+" exists but was not created by golem-deploy", nil)
	case errors.Is(err, docker.ErrNotOurs):
		s.dropNetwork(err)
		return jobs.Fail("the Docker network "+docker.NetworkName+" belongs to another golem-deploy installation (or to an older version and still has containers attached)", nil)
	case err != nil:
		return s.failDocker("prepare the "+docker.NetworkName+" network", err)
	}
	s.forgetNetwork()
	if !isolated {
		// Never run the hub where games can reach its port; a hub that
		// runs there now goes too, so isolation can apply.
		s.setHub("", 0)
		if err := s.removeHubs(ctx, w, "its network allows traffic between containers"); err != nil {
			return err
		}
		return jobs.Fail(notIsolatedMsg, nil)
	}
	sec, err := s.ensureSecrets(ctx, w, want.rotate)
	if err != nil {
		return err
	}
	hash := hubHash(img.ID, s.cfg.MemoryMB, sec.Generation)
	id, port, seenAt, err := s.ensureHub(ctx, w, img, hash, sec)
	if err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	sid, err := s.hubLogin(lctx, hubView{sec: sec, container: id, port: port})
	cancel()
	if err != nil {
		s.logger.Warn("the Inspector hub refused the daemon's login", "err", err)
		switch {
		case errors.Is(err, errHubCredentials):
			return jobs.Fail("the Inspector hub does not accept the daemon's password; rotate the Inspector's secrets to replace the hub", nil)
		case errors.Is(err, errHubRateLimited):
			return jobs.Fail("the Inspector hub limits the daemon's logins for now; apply the Inspector again later", nil)
		}
		return jobs.Fail("the Inspector hub is running but did not accept the daemon's login; apply the Inspector again", nil)
	}
	fmt.Fprintln(w, "the daemon is logged in at the hub")
	if err := s.openListener(); err != nil {
		return err
	}
	if s.hooks.beforePublish != nil {
		s.hooks.beforePublish()
	}
	// The setting is checked again in the step that publishes the hub as
	// serving: a turn-off stored meanwhile (it closed admission) wins.
	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return errTurnedOff
	}
	// The confirmation is as old as Docker's answer, not this step.
	s.hubContainer, s.hubPort, s.hubVerified = id, port, seenAt
	s.hubSick, s.hubFailures = "", 0
	s.hubSeq++
	s.admitting = true
	s.detail = ""
	s.mu.Unlock()
	s.statusMu.Lock()
	old := s.statusSID
	s.statusSID, s.targetsAt = sid, time.Time{}
	s.statusMu.Unlock()
	s.logoutHub(old)
	if host, reachable, known := s.agentHost(ctx); !reachable {
		fmt.Fprintln(w, "warning: the Inspector listener is bound to a loopback address; game servers cannot reach it and will not appear in the Inspector")
	} else if known {
		fmt.Fprintf(w, "games' Inspector agents connect to %s\n", net.JoinHostPort(host, strconv.Itoa(s.port())))
	}
	fmt.Fprintf(w, "the Inspector is running on port %d\n", s.port())
	return nil
}

// ensureSecrets generates the hub's secrets on first use or when rotating.
// New secrets are stored durably before anything uses them; then the old
// hub is forgotten and everything admitted under the old secrets is
// drained.
func (s *Service) ensureSecrets(ctx context.Context, w io.Writer, rotate bool) (*hubSecrets, error) {
	s.mu.Lock()
	sec := s.sec
	s.mu.Unlock()
	if sec != nil && !rotate {
		return sec, nil
	}
	gen := s.cfg.Now().Unix()
	if sec != nil && sec.Generation >= gen {
		gen = sec.Generation + 1
	}
	fresh, err := newHubSecrets(gen, s.cfg.BcryptCost)
	if err != nil {
		return nil, jobs.Fail("could not generate the Inspector secrets", err)
	}
	if err := saveSecrets(context.WithoutCancel(ctx), s.cfg.Store, s.cfg.Secrets, fresh); err != nil {
		return nil, jobs.Fail("could not store the Inspector secrets", err)
	}
	s.mu.Lock()
	s.sec = fresh
	s.gen++
	s.hubContainer, s.hubPort, s.hubVerified = "", 0, time.Time{}
	s.hubSeq++
	s.mu.Unlock()
	if rotate {
		// The tokens changed: agents must reconnect with new ones (after a
		// restart of their server) and browser sessions start over.
		n := s.drain()
		fmt.Fprintf(w, "replaced the Inspector secrets; ended %d Inspector sessions; servers with the Inspector on need a restart\n", n)
	} else {
		s.drain()
		fmt.Fprintln(w, "generated the Inspector secrets")
	}
	return fresh, nil
}

// ensureImage resolves the configured image, downloading it when the host
// does not have it (or when asked to). The automatic repair never
// downloads.
func (s *Service) ensureImage(ctx context.Context, w io.Writer, want applyRequest) (docker.Image, error) {
	img, err := s.cfg.Docker.ResolveImage(ctx, s.cfg.Image)
	missing := errors.Is(err, docker.ErrNotFound)
	if err != nil && !missing {
		return docker.Image{}, s.failDocker("inspect the Inspector image", err)
	}
	if !missing && !want.pull {
		fmt.Fprintf(w, "using the Inspector image %s (%s)\n", s.cfg.Image, shortID(img.ID))
		return img, nil
	}
	if want.auto {
		return docker.Image{}, jobs.Fail("the Inspector image "+s.cfg.Image+" is not on this host; apply the Inspector again to download it", nil)
	}
	fmt.Fprintf(w, "downloading the Inspector image %s\n", s.cfg.Image)
	if err := s.cfg.Docker.PullImage(ctx, s.cfg.Image); err != nil {
		s.logger.Warn("docker pull failed", "image", s.cfg.Image, "err", err)
		switch {
		case errors.Is(err, docker.ErrUnavailable):
			return docker.Image{}, jobs.Fail("Docker is unreachable", docker.ErrUnavailable)
		case errors.Is(err, docker.ErrBuildConfig):
			return docker.Image{}, jobs.Fail(docker.ErrBuildConfig.Error(), nil)
		case errors.Is(err, context.Canceled):
			return docker.Image{}, err
		}
		return docker.Image{}, jobs.Fail("the Inspector image "+s.cfg.Image+" could not be downloaded; check the reference and the host's network, see the daemon log", nil)
	}
	img, err = s.cfg.Docker.ResolveImage(ctx, s.cfg.Image)
	if err != nil {
		return docker.Image{}, s.failDocker("inspect the downloaded Inspector image", err)
	}
	fmt.Fprintf(w, "downloaded the Inspector image %s (%s)\n", s.cfg.Image, shortID(img.ID))
	return img, nil
}

// hubHash fingerprints the hub container definition without secret
// material: the secrets' generation stands in for them.
func hubHash(imageID string, memoryMB int, generation int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d\x00%d\x00%s\x00%s", hubSpecVersion, imageID, memoryMB, generation, HubOrigin, HubSessionMaxAge))
	return hex.EncodeToString(sum[:])[:32]
}

// hubEnv is the hub's environment.
func hubEnv(sec *hubSecrets) map[string]string {
	return map[string]string{
		"GOLEM_INSPECTOR_ADDR":            ":" + strconv.Itoa(docker.InspectorHubPort),
		"GOLEM_INSPECTOR_PRODUCTION":      "true",
		"GOLEM_INSPECTOR_BEHIND_PROXY":    "true",
		"GOLEM_INSPECTOR_PUBLIC_ORIGINS":  HubOrigin,
		"GOLEM_INSPECTOR_AGENT_TOKEN":     sec.AgentToken,
		"GOLEM_INSPECTOR_PASSWORD_HASH":   sec.PasswordHash,
		"GOLEM_INSPECTOR_SESSION_MAX_AGE": HubSessionMaxAge.String(),
	}
}

// ensureHub keeps a running hub that matches hash or replaces it, and
// waits until it answers ready. It returns the container id, its loopback
// port and when Docker last confirmed that port.
func (s *Service) ensureHub(ctx context.Context, w io.Writer, img docker.Image, hash string, sec *hubSecrets) (string, int, time.Time, error) {
	hubs, err := s.cfg.Docker.InspectorHubs(ctx)
	if err != nil {
		return "", 0, time.Time{}, s.failDocker("list the Inspector hub container", err)
	}
	s.mu.Lock()
	sick := s.hubSick
	s.mu.Unlock()
	var keep *docker.Container
	for i := len(hubs) - 1; i >= 0; i-- {
		h := hubs[i]
		// A hub that stopped answering is replaced, never kept.
		if keep == nil && h.Running && h.ID != sick && h.Labels[docker.LabelConfigHash] == hash && h.ImageID == img.ID {
			keep = &hubs[i]
			continue
		}
		if err := s.cfg.Docker.RemoveInspectorHub(ctx, h.ID); err != nil {
			return "", 0, time.Time{}, s.failDocker("remove the Inspector hub container", err)
		}
		fmt.Fprintf(w, "removed the Inspector hub container %s (it did not match the settings, was not running or did not answer)\n", shortID(h.ID))
	}
	if keep != nil {
		fmt.Fprintf(w, "the Inspector hub container %s is running with the current settings\n", shortID(keep.ID))
		port, at, err := s.waitHubReady(ctx, w, keep.ID, sec)
		var nr notReadyError
		if errors.As(err, &nr) && ctx.Err() == nil {
			// It runs but does not answer: the next apply replaces it. A
			// Docker failure says nothing about the hub and leaves it.
			s.mu.Lock()
			s.hubSick = keep.ID
			s.hubSeq++
			s.mu.Unlock()
		}
		return keep.ID, port, at, err
	}
	s.setHub("", 0)
	spec := docker.HubSpec{Image: img.ID, ImageVolumes: img.Volumes, Env: hubEnv(sec), MemoryMB: s.cfg.MemoryMB, ConfigHash: hash}
	id, err := s.cfg.Docker.CreateInspectorHub(ctx, spec)
	if err != nil {
		return "", 0, time.Time{}, s.failDocker("create the Inspector hub container", err)
	}
	if err := s.cfg.Docker.StartInspectorHub(ctx, id); err != nil {
		return "", 0, time.Time{}, s.failDocker("start the Inspector hub container", err)
	}
	fmt.Fprintf(w, "started the Inspector hub container %s, waiting up to %s for it to be ready\n", shortID(id), s.cfg.ReadyTimeout)
	port, at, err := s.waitHubReady(ctx, w, id, sec)
	return id, port, at, err
}

// notReadyError marks a failure of the hub itself in waitHubReady: it
// exited, or Docker reported it running and it did not answer ready in
// time. A Docker failure is not one.
type notReadyError struct{ err error }

func (e notReadyError) Error() string { return e.err.Error() }
func (e notReadyError) Unwrap() error { return e.err }

// waitHubReady polls the container and the hub's /ready. It returns the
// port and when Docker answered that the container publishes it. When
// Docker fails or does not answer in time the job fails with the Docker
// message (no notReadyError).
func (s *Service) waitHubReady(ctx context.Context, w io.Writer, id string, sec *hubSecrets) (int, time.Time, error) {
	wctx, cancel := context.WithTimeout(ctx, s.cfg.ReadyTimeout)
	defer cancel()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		ct, err := s.inspectHub(wctx, id)
		at := s.cfg.Now()
		if err != nil {
			if ctx.Err() != nil {
				return 0, time.Time{}, ctx.Err()
			}
			if wctx.Err() != nil {
				err = context.DeadlineExceeded
			}
			return 0, time.Time{}, s.failDocker("inspect the Inspector hub container", err)
		}
		if !ct.Running && ct.State != "restarting" && ct.State != "created" {
			fmt.Fprintf(w, "the Inspector hub container is %s (exit code %d); last output:\n", ct.State, ct.ExitCode)
			s.writeExcerpt(ctx, w, id, sec)
			return 0, time.Time{}, notReadyError{jobs.Fail(fmt.Sprintf("the Inspector hub exited during start (exit code %d); see the job log", ct.ExitCode), nil)}
		}
		if port := ct.LoopbackPort(docker.InspectorHubPort); ct.Running && port != 0 {
			if err := s.hub.ready(wctx, port); err == nil {
				fmt.Fprintln(w, "the Inspector hub is ready")
				return port, at, nil
			}
		}
		select {
		case <-wctx.Done():
		case <-t.C:
		}
		if wctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return 0, time.Time{}, ctx.Err()
	}
	fmt.Fprintln(w, "the Inspector hub did not become ready in time; last output:")
	s.writeExcerpt(ctx, w, id, sec)
	return 0, time.Time{}, notReadyError{jobs.Fail(fmt.Sprintf("the Inspector hub did not become ready within %s; see the job log", s.cfg.ReadyTimeout), nil)}
}

// writeExcerpt copies the hub's last lines to the job log: secrets
// redacted, sanitised and prefixed so nothing the hub prints poses as a
// daemon message.
func (s *Service) writeExcerpt(ctx context.Context, w io.Writer, id string, sec *hubSecrets) {
	out, err := s.cfg.Docker.InspectorHubLogTail(context.WithoutCancel(ctx), id, 50)
	if err != nil {
		s.logger.Warn("could not read the Inspector hub logs for the job log", "err", err)
		fmt.Fprintln(w, "(could not read the hub's output)")
		return
	}
	redacted := docker.RedactValues(string(out), map[string]string{"p": sec.Password, "h": sec.PasswordHash, "t": sec.AgentToken, "k": sec.TokenKey})
	san := logsafe.NewWriter(w, hubLogPrefix)
	_, _ = io.Copy(san, bytes.NewReader([]byte(redacted)))
	_ = san.Close()
}

func (s *Service) setHub(id string, port int) {
	s.mu.Lock()
	s.hubContainer, s.hubPort, s.hubVerified = id, port, time.Time{}
	s.hubSeq++
	s.mu.Unlock()
}

// openListener binds the listener when it is not open.
func (s *Service) openListener() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return jobs.Fail("the daemon is stopping", nil)
	}
	if s.ln != nil {
		return nil
	}
	ln, err := openListener(s.cfg.Listen, s.cfg.TLSConfig, s.browserHandler(), s.agentHandler(), s.sniffTimeout, s.reqTimeout, s.maxConns, s.logger)
	if err != nil {
		s.logger.Warn("could not open the Inspector listener", "addr", s.cfg.Listen, "err", err)
		return jobs.Fail(bindError(s.cfg.Listen, err), nil)
	}
	s.ln = ln
	scheme := "http"
	if s.cfg.TLSConfig != nil {
		scheme = "https"
	}
	s.logger.Info("inspector listening", "addr", ln.addr.String(), "scheme", scheme)
	return nil
}

// closeListener stops admission, then ends every session, agent connection
// and ticket and closes the listener (which waits for the relayed streams
// within its grace).
func (s *Service) closeListener() {
	s.endAdmission()
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	s.mu.Unlock()
	if ln != nil {
		ln.close()
		s.logger.Info("inspector listener closed")
	}
	s.statusMu.Lock()
	sid := s.statusSID
	s.statusSID, s.targetsAt = "", time.Time{}
	s.statusMu.Unlock()
	s.logoutHub(sid)
}

// endAdmission closes admission (a new generation), then drains what was
// admitted: nothing that started before can register after the drain.
func (s *Service) endAdmission() {
	s.mu.Lock()
	s.admitting = false
	s.gen++
	s.mu.Unlock()
	s.drain()
}

// drain drops every ticket and ends every session and agent connection. It
// returns the number of sessions ended. Callers change the generation
// first, so nothing admitted before survives.
func (s *Service) drain() int {
	s.tickets.clear()
	n := s.endAllSessions()
	s.agents.closeAll()
	s.pages.closeAll()
	return n
}

// endAllSessions ends every session and returns how many there were.
func (s *Service) endAllSessions() int {
	sids := s.sessions.drain()
	for _, sid := range sids {
		s.logoutHub(sid)
	}
	return len(sids)
}

// sweepSessions ends sessions whose account no longer exists, lost its
// approval or changed its token version, and expired sessions.
func (s *Service) sweepSessions(ctx context.Context) {
	now := s.cfg.Now()
	for _, sess := range s.sessions.all() {
		if !now.Before(sess.expires) {
			s.endSession(sess)
			continue
		}
		acct, err := s.cfg.Store.GetAccount(ctx, sess.accountID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			continue // try again on the next sweep
		}
		if err != nil || !acct.Approved || acct.TokenVersion != sess.tokenVersion {
			s.logger.Info("inspector session ended: the account changed", "account_id", sess.accountID)
			s.endSession(sess)
		}
	}
}

// refreshHub re-reads the recorded hub container through Docker (it can
// assign a new port when it restarts the container) and records the answer
// (observeHub). Concurrent refreshes share one Docker call; none runs
// within minAge of the last one, or while the recorded port was confirmed
// within hubCheckInterval (checked inside the shared call, so a caller
// that arrives just after a confirmation does not ask again). A caller
// whose context is done does nothing.
func (s *Service) refreshHub(ctx context.Context, minAge time.Duration) {
	if ctx.Err() != nil {
		return
	}
	s.hubFlight.do(ctx, s.cfg.Now, minAge, func() {
		now := s.cfg.Now()
		s.mu.Lock()
		id, recent := s.hubContainer, s.recentLocked(now)
		c := hubCheck{id: id, seq: s.hubSeq}
		s.mu.Unlock()
		if id == "" || recent || s.inspectHub == nil {
			return
		}
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubDockerTimeout)
		defer cancel()
		ct, err := s.inspectHub(qctx, id)
		seen, port := seenOf(ct, err)
		s.observeHub(c, seen, port, s.cfg.Now())
	})
}

// checkHub asks the recorded hub's readiness endpoint (once per sweep, no
// credentials). After hubSickAfter failures in a row (transport errors or
// a readiness answer other than 200) the hub counts as not answering: its
// port is dropped and not restored from Docker's metadata alone, the state
// is "stopped" with sickDetail and the repair is due (it replaces the
// container). A result counts only while the container, its port and the
// apply that published them are the ones the check started with (hubSeq).
func (s *Service) checkHub(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	id, port, enabled, seq := s.hubContainer, s.hubPort, s.enabled, s.hubSeq
	s.mu.Unlock()
	if !enabled || id == "" || port == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, hubReadyTimeout)
	err := s.hub.ready(rctx, port)
	cancel()
	if err != nil && ctx.Err() != nil {
		return // the sweep ended, not the hub
	}
	if s.hooks.afterReadyCheck != nil {
		s.hooks.afterReadyCheck()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hubContainer != id || s.hubPort != port || s.hubSeq != seq {
		return
	}
	if err == nil {
		s.hubFailures = 0
		return
	}
	s.hubFailures++
	if isTransport(err) {
		s.hubPort, s.hubVerified = 0, time.Time{}
		s.hubSeq++
	}
	if s.hubFailures >= hubSickAfter && s.hubSick != id {
		s.hubSick = id
		s.hubPort, s.hubVerified = 0, time.Time{}
		s.hubSeq++
		s.logger.Warn("the Inspector hub does not answer its readiness check; it is repaired", "failures", s.hubFailures, "err", err)
	}
}

// needsRepair reports whether the repair job should run.
//
// With the Inspector on: the hub is not serving (no listener, no hub, a
// hub that does not answer, or Docker reports it gone, stopped or without
// its port), at most every RepairInterval after a failed attempt. When
// Docker does not answer (an error, a timeout, ctx done meanwhile) that is
// recorded as no answer and starts no repair.
//
// With it off or unavailable: a listener or a hub is left after a turn-off
// whose job did not run or failed (cleanupDue). That is checked on every
// sweep without asking Docker and is not paced by failed attempts to start
// the hub; a failed removal is retried after cleanupRetry. Otherwise a hub
// container left behind is looked for at most every RepairInterval.
//
// A caller whose context is done gets false without any check.
func (s *Service) needsRepair(ctx context.Context) bool {
	if ctx.Err() != nil || s.applying() {
		return false
	}
	now := s.cfg.Now()
	s.mu.Lock()
	enabled, ln, id, last, sick := s.enabled, s.ln, s.hubContainer, s.lastAttempt, s.hubSick
	cleanup, lastCleanup, lastStray := s.cleanupDue, s.lastCleanup, s.lastStray
	c := hubCheck{id: id, seq: s.hubSeq}
	s.mu.Unlock()
	if !enabled || !s.Available() {
		if ln != nil || id != "" || cleanup {
			return lastCleanup.IsZero() || now.Sub(lastCleanup) >= cleanupRetry
		}
		if !lastStray.IsZero() && now.Sub(lastStray) < RepairInterval {
			return false
		}
		return s.hubLeft(ctx)
	}
	if !last.IsZero() && now.Sub(last) < RepairInterval {
		return false
	}
	if ln == nil || id == "" || sick == id || s.inspectHub == nil {
		return true
	}
	qctx, cancel := context.WithTimeout(ctx, hubDockerTimeout)
	ct, err := s.inspectHub(qctx, id)
	cancel()
	seen, port := seenOf(ct, err)
	recorded := s.observeHub(c, seen, port, s.cfg.Now())
	return recorded && (seen == seenGone || seen == seenStopped)
}

// hubLeft reports whether a hub container exists while the Inspector is
// off or unavailable. A listing that completed counts as a look for
// RepairInterval; a failed one does not.
func (s *Service) hubLeft(ctx context.Context) bool {
	if s.listHubs == nil || ctx.Err() != nil {
		return false
	}
	hubs, err := s.listHubs(ctx)
	if err != nil {
		return false
	}
	s.mu.Lock()
	s.lastStray = s.cfg.Now()
	s.mu.Unlock()
	return len(hubs) > 0
}

// Run repairs the hub at start and when it stops serving, removes a hub
// that should not exist (at most every RepairInterval), sweeps sessions
// and agent relays every SweepInterval and keeps the hub's port current,
// until ctx ends. It then closes the listener and ends every session and
// relayed connection; the hub container keeps running.
func (s *Service) Run(ctx context.Context) {
	defer s.Close()
	s.mu.Lock()
	enabled := s.enabled
	s.mu.Unlock()
	if enabled && s.Available() {
		s.startRepair(ctx)
	} else {
		cctx, cancel := context.WithTimeout(ctx, s.stepTimeout)
		if s.hubLeft(cctx) {
			s.startRepair(ctx)
		}
		cancel()
	}
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sweep(ctx)
	}
}

// sweep is one round of Run. Each step has its own time budget
// (stepTimeout), so a slow step never leaves a later one an expired
// context; nothing runs once ctx is done.
func (s *Service) sweep(ctx context.Context) {
	step := func(fn func(context.Context)) {
		if ctx.Err() != nil {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, s.stepTimeout)
		defer cancel()
		fn(sctx)
	}
	step(s.sweepSessions)
	step(s.sweepAgents)
	step(func(c context.Context) { s.refreshHub(c, hubCheckInterval) })
	step(s.checkHub)
	repair := false
	step(func(c context.Context) { repair = s.needsRepair(c) })
	if repair {
		s.startRepair(ctx)
	}
}

// startRepair starts the automatic repair. Starting it counts as an
// attempt at once (a start of the hub while the setting is on, a removal
// while it is off), before the job runs: a job that fails before its body
// runs (its log cannot be created, for example) is paced like one that
// ran. The body records the outcome when it runs.
func (s *Service) startRepair(ctx context.Context) {
	s.mu.Lock()
	if s.enabled && s.Available() {
		s.lastAttempt = s.cfg.Now()
	} else {
		s.lastCleanup = s.cfg.Now()
	}
	s.mu.Unlock()
	job, err := s.startApply(ctx, applyRequest{auto: true}, nil)
	if err != nil {
		if !errors.Is(err, jobs.ErrSubjectBusy) && !errors.Is(err, jobs.ErrShuttingDown) {
			s.logger.Warn("could not start the Inspector repair", "err", err)
		}
		return
	}
	s.logger.Info("inspector repair started", "job_id", job.ID)
}

// Close stops admission, closes the listener and ends every session and
// relayed agent connection, then waits for background hub calls. The hub
// container keeps running.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.closeListener()
	s.mu.Lock()
	s.bgClosed = true
	s.mu.Unlock()
	s.bg.Wait()
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// sampled runs a function at most once per interval.
type sampled struct {
	mu    sync.Mutex
	every time.Duration
	now   func() time.Time
	last  time.Time
}

func (l *sampled) Do(fn func()) {
	l.mu.Lock()
	now := l.now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.mu.Unlock()
		return
	}
	l.last = now
	l.mu.Unlock()
	fn()
}
