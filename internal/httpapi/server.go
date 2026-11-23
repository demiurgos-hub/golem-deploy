// Package httpapi is the daemon's HTTP layer: the http.Server, the /api/v1
// route table with per-route access levels, middleware, handlers and the
// fallback to the embedded web client.
package httpapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Config holds listener settings.
type Config struct {
	// Listen is the TCP address, e.g. "127.0.0.1:8000".
	Listen  string
	TLSCert string
	TLSKey  string
	// InsecureHTTP allows plaintext HTTP on a non-loopback address.
	InsecureHTTP bool
	// Version is reported by GET /api/v1/status.
	Version string
	// PublicHosts are the host names (without port, case-insensitive)
	// accepted in the Host header besides IP literals and "localhost".
	// Anything else is refused with 421 bad_host (DNS rebinding defence).
	PublicHosts []string
	// TrustedProxies are the networks of reverse proxies whose
	// X-Forwarded-For header is believed. Empty means forwarding headers are
	// ignored and the TCP peer is the client.
	TrustedProxies []netip.Prefix
}

func (c Config) tls() bool { return c.TLSCert != "" && c.TLSKey != "" }

// DockerProbe reports whether Docker is reachable.
type DockerProbe interface {
	Reachable(ctx context.Context) bool
}

// Deps are the server's collaborators. Store, Sessions, Hasher, Secrets and
// Jobs are required.
type Deps struct {
	Store    *store.Store
	Sessions *auth.Sessions
	Hasher   *auth.Hasher
	Secrets  *secrets.Box
	Jobs     *jobs.Runner
	Logger   *slog.Logger
	Docker   DockerProbe
	// Git runs project checkouts; nil makes project git operations answer
	// 503. RepoDir holds the checkouts (<RepoDir>/<project id>) and TempDir
	// is scratch space on the same filesystem.
	Git     GitClient
	RepoDir string
	TempDir string
	// SPA serves non-API paths (the embedded web client). Nil serves 404.
	SPA http.Handler
	// Now overrides the clock (tests).
	Now func() time.Time
	// HashSlots bounds concurrent bcrypt operations (0: auth.DefaultSlotCount).
	HashSlots int
	// HashSlotWait is how long a request waits for a bcrypt slot before it
	// gets 503 (0: hashSlotWait).
	HashSlotWait time.Duration
}

// Server holds the HTTP layer's state. Create it with New.
type Server struct {
	cfg      Config
	store    *store.Store
	sessions *auth.Sessions
	hasher   *auth.Hasher
	secrets  *secrets.Box
	jobs     *jobs.Runner
	logger   *slog.Logger
	docker   DockerProbe
	spa      http.Handler
	now      func() time.Time
	proj     *projectState

	// Password attempt limits (login, password change, token creation):
	// failures per client IP and per submitted username, a known-good-IP
	// exemption from the username limit, and in-flight caps.
	loginIP       *auth.Limiter
	loginUser     *auth.Limiter
	registerIP    *auth.Limiter
	knownIPs      *auth.KnownIPs
	loginFlightIP *auth.InFlight
	loginFlightU  *auth.InFlight
	// hashSlots bounds concurrent bcrypt work reachable from HTTP.
	hashSlots *auth.Slots
	// logStreams caps concurrent following log streams per account.
	logStreams *auth.InFlight

	publicHosts    map[string]bool
	trustedProxies []netip.Prefix
	badHostLog     sampledLog

	api     *http.ServeMux
	handler http.Handler

	shutdownOnce sync.Once
	shutdown     chan struct{}
}

// Rate limits (in-memory, per process).
const (
	// loginFailuresPerWindow is the per-client-IP password failure limit.
	loginFailuresPerWindow = 5
	// usernameFailuresPerWindow is the per-username failure limit, counted
	// for every submitted username whether it exists or not. Addresses that
	// logged in successfully as that user are exempt.
	usernameFailuresPerWindow = 20
	loginWindow               = 15 * time.Minute
	registrationsPerWindow    = 5
	registerWindow            = time.Hour
	limiterMaxKeys            = 50000
	// knownIPsPerUser and knownIPUsers bound the known-good-IP memory.
	knownIPsPerUser = 8
	knownIPUsers    = 10000
	// loginsInFlightPerIP and loginsInFlightPerUser cap concurrent logins.
	loginsInFlightPerIP   = 4
	loginsInFlightPerUser = 1
	// hashSlotWait is how long a request waits for a bcrypt slot.
	hashSlotWait = 2 * time.Second
	// logStreamsPerAccount caps concurrent ?follow=1 log streams.
	logStreamsPerAccount = 4
)

// Body size limits.
const (
	defaultBodyLimit = 1 << 20 // 1 MiB for JSON routes
	// StaticUploadLimit is the multipart limit of POST /api/v1/static.
	StaticUploadLimit = 1 << 30 // 1 GiB
)

// New builds a Server.
func New(cfg Config, d Deps) (*Server, error) {
	if d.Store == nil || d.Sessions == nil || d.Hasher == nil || d.Secrets == nil || d.Jobs == nil {
		return nil, errors.New("httpapi: Store, Sessions, Hasher, Secrets and Jobs are required")
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.SPA == nil {
		d.SPA = http.NotFoundHandler()
	}
	if d.HashSlots <= 0 {
		d.HashSlots = auth.DefaultSlotCount()
	}
	if d.HashSlotWait <= 0 {
		d.HashSlotWait = hashSlotWait
	}
	hosts, err := NormalizePublicHosts(cfg.PublicHosts)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:        cfg,
		store:      d.Store,
		sessions:   d.Sessions,
		hasher:     d.Hasher,
		secrets:    d.Secrets,
		jobs:       d.Jobs,
		logger:     d.Logger,
		docker:     d.Docker,
		spa:        d.SPA,
		now:        d.Now,
		proj:       newProjectState(d),
		loginIP:    auth.NewLimiter(loginFailuresPerWindow, loginWindow, limiterMaxKeys, d.Now),
		loginUser:  auth.NewLimiter(usernameFailuresPerWindow, loginWindow, limiterMaxKeys, d.Now),
		registerIP: auth.NewLimiter(registrationsPerWindow, registerWindow, limiterMaxKeys, d.Now),
		knownIPs:   auth.NewKnownIPs(knownIPsPerUser, knownIPUsers),

		loginFlightIP: auth.NewInFlight(loginsInFlightPerIP),
		loginFlightU:  auth.NewInFlight(loginsInFlightPerUser),
		hashSlots:     auth.NewSlots(d.HashSlots, d.HashSlotWait),
		logStreams:    auth.NewInFlight(logStreamsPerAccount),

		publicHosts:    map[string]bool{},
		trustedProxies: append([]netip.Prefix(nil), cfg.TrustedProxies...),
		shutdown:       make(chan struct{}),
	}
	for _, h := range hosts {
		s.publicHosts[h] = true
	}
	s.api = http.NewServeMux()
	for _, rt := range s.routes() {
		s.register(rt)
	}
	s.handler = s.outer(http.HandlerFunc(s.dispatch))
	return s, nil
}

// handlerFunc is an HTTP handler that reports failures by returning an
// error, which is rendered as the error envelope.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// route is one entry of the route table.
type route struct {
	method string
	// path is relative to /api/v1 and may contain {wildcards}.
	path    string
	access  Access
	handler handlerFunc
	// maxBody overrides the default request body limit.
	maxBody int64
}

func (rt route) pattern() string { return rt.method + " /api/v1" + rt.path }

func (s *Server) register(rt route) {
	if rt.access < AccessPublic || rt.access > AccessAdmin {
		panic("httpapi: route " + rt.pattern() + " has no access level")
	}
	limit := rt.maxBody
	if limit == 0 {
		limit = defaultBodyLimit
	}
	h := s.authorize(rt.access, rt.handler)
	access := rt.access
	s.api.Handle(rt.pattern(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		r = r.WithContext(context.WithValue(r.Context(), routeAccessKey, access))
		if err := h(w, r); err != nil {
			s.writeError(w, r, err)
		}
	}))
}

// dispatch sends /api paths to the API mux, rendering its 404 and 405 as
// JSON envelopes, and everything else to the web client.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if !isAPIPath(r.URL.Path) {
		s.spa.ServeHTTP(w, r)
		return
	}
	h, pattern := s.api.Handler(r)
	if pattern != "" {
		s.api.ServeHTTP(w, r)
		return
	}
	// No route matched. The mux's own handler distinguishes 404 from 405 and
	// sets Allow; run it against a recorder and translate.
	rec := &headerRecorder{header: http.Header{}}
	h.ServeHTTP(rec, r)
	if rec.code == http.StatusMethodNotAllowed {
		e := newError(http.StatusMethodNotAllowed, apitypes.ErrorCodeMethodNotAllowed, "method not allowed for this path")
		e.header = http.Header{"Allow": rec.header.Values("Allow")}
		s.writeError(w, r, e)
		return
	}
	s.writeError(w, r, notFound("route"))
}

// headerRecorder captures the status and headers of the mux's fallback
// handlers and discards their plain-text bodies.
type headerRecorder struct {
	header http.Header
	code   int
}

func (h *headerRecorder) Header() http.Header         { return h.header }
func (h *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (h *headerRecorder) WriteHeader(code int)        { h.code = code }

// Handler returns the complete HTTP handler (middleware, API and SPA).
func (s *Server) Handler() http.Handler { return s.handler }

// RoutePatterns lists every registered pattern with its access level.
func (s *Server) RoutePatterns() map[string]Access {
	out := map[string]Access{}
	for _, rt := range s.routes() {
		out[rt.pattern()] = rt.access
	}
	return out
}

// ErrPlaintextNonLoopback is returned when plaintext HTTP would be served on
// a non-loopback address without an explicit opt-in.
var ErrPlaintextNonLoopback = errors.New("refusing to serve plaintext HTTP on a non-loopback address: configure --tls-cert and --tls-key, or pass --insecure-http if TLS is terminated elsewhere")

// CheckListen enforces the bind rule: without TLS, only loopback addresses
// are allowed unless insecure is set. A missing host (":8000") means all
// interfaces and is not loopback; "localhost" is resolved and every address
// it resolves to must be loopback; other host names are not resolved and
// count as non-loopback.
func CheckListen(addr string, tlsEnabled, insecure bool) error {
	return checkListen(addr, tlsEnabled, insecure, lookupHost)
}

func lookupHost(host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func checkListen(addr string, tlsEnabled, insecure bool, lookup func(string) ([]netip.Addr, error)) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid listen address %q: missing port", addr)
	}
	if tlsEnabled || insecure {
		return nil
	}
	if strings.EqualFold(host, "localhost") {
		addrs, err := lookup(host)
		if err != nil || len(addrs) == 0 {
			return fmt.Errorf("%w (could not resolve %q to check it)", ErrPlaintextNonLoopback, host)
		}
		for _, a := range addrs {
			if !a.Unmap().IsLoopback() {
				return fmt.Errorf("%w (%q resolves to %s)", ErrPlaintextNonLoopback, host, a)
			}
		}
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return ErrPlaintextNonLoopback
}

// Timeouts of the http.Server. Streaming handlers extend their own write
// deadline with http.ResponseController; upload handlers extend the read
// deadline the same way.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	shutdownGrace     = 15 * time.Second
)

// Listen validates the configuration and opens the listener.
func (s *Server) Listen() (net.Listener, error) {
	if (s.cfg.TLSCert == "") != (s.cfg.TLSKey == "") {
		return nil, errors.New("--tls-cert and --tls-key must be set together")
	}
	if err := CheckListen(s.cfg.Listen, s.cfg.tls(), s.cfg.InsecureHTTP); err != nil {
		return nil, err
	}
	return net.Listen("tcp", s.cfg.Listen)
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully:
// it stops accepting connections, ends streaming responses and waits up to
// a grace period for in-flight requests.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}
	if s.cfg.tls() {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}

	errCh := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errCh <- srv.ServeTLS(ln, "", "") // certificates are in TLSConfig
			return
		}
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		s.beginShutdown()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	s.beginShutdown()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

func (s *Server) beginShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}
