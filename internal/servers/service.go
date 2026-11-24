// Package servers is the game server service: server definitions in the
// store (env sealed with secrets.ServerEnvPurpose), each running the image
// of a successful build, their containers through the label-scoped docker
// client (each server's container is the one whose id is recorded in its
// row), the Golem Engine deploy contract injected into every container,
// host port allocation, the health-gated start/stop/restart/delete job
// bodies, each server's data volume with the backup, restore and reset job
// bodies and the backup schedule, the runtime view read from the game's
// admin endpoint,
// export/import, startup reconciliation, the managed image and volume views
// that cross-reference servers and builds, and the sanitising of container
// output shown to users.
package servers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Errors returned by Service. Handlers map them to API errors; docker
// errors (docker.ErrUnavailable in particular) pass through wrapped.
var (
	ErrNotFound      = errors.New("servers: not found")
	ErrNameTaken     = errors.New("servers: name taken")
	ErrPortInUse     = errors.New("servers: port in use")
	ErrNoContainer   = errors.New("servers: server has no container")
	ErrNotRunning    = errors.New("servers: server is not running")
	ErrInUse         = errors.New("servers: object is in use")
	ErrJobInProgress = store.ErrServerJob
	ErrConflict      = errors.New("servers: conflicting change")
	// ErrRunning: the change needs the server to be stopped first (its
	// port cannot change while its container runs).
	ErrRunning = errors.New("servers: server is running")
	// ErrAdminUnavailable: the game's admin endpoint did not answer (or
	// answered outside the contract).
	ErrAdminUnavailable = errors.New("servers: the game's admin endpoint did not answer")
)

// FieldError is a rejected request field; Msg is safe to show to users.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

func fieldErr(field, format string, a ...any) error {
	return &FieldError{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// DefaultStartTimeout is the default of Config.StartTimeout (--start-timeout).
const DefaultStartTimeout = 60 * time.Second

// Config holds daemon-wide server settings.
type Config struct {
	// Ports is the validated host port range.
	Ports ports.Range
	// PanelPort is the panel's own listen port (0 when unknown); a server
	// can never be started on it.
	PanelPort int
	// Panel connection settings supply the browser test origins.
	PanelPublicHosts []string
	PanelTLS         bool
	PanelLoopback    bool
	// BindAddress is the host IP game ports are published on ("" = all).
	BindAddress string
	// MemoryMB and CPUs are per-container limits (0 = unlimited).
	MemoryMB int
	CPUs     float64
	// StopTimeout is the graceful stop timeout (default 30s).
	StopTimeout time.Duration
	// StartTimeout bounds how long a start waits for the game's admin
	// endpoint to report ready (default DefaultStartTimeout); ReadyPoll is
	// the poll interval (default 500ms).
	StartTimeout time.Duration
	ReadyPoll    time.Duration
	// DefaultGameHost is used when the game_host setting is empty (the
	// first --public-host; "" when there is none).
	DefaultGameHost string
	// Certs is the game certificate manager. Without it servers cannot
	// start.
	Certs *certs.Manager
	// ProbePort checks that nothing else listens on a host port before a
	// start (default ports.ProbeFree).
	ProbePort func(bindAddr string, port int) error
	// Backups stores backup archives; without it backups, restores and
	// safety backups fail. BackupMaxBytes is the largest data directory a
	// backup accepts (default DefaultBackupMaxBytes).
	Backups        *backups.Store
	BackupMaxBytes int64
	// SnapshotRetry is the pause between snapshot requests while the game
	// reports one in progress (default 2s); SnapshotDeadline bounds the
	// whole request for a fresh snapshot, retries included (default
	// DefaultSnapshotDeadline).
	SnapshotRetry    time.Duration
	SnapshotDeadline time.Duration
	// Now is the clock of the backup schedule (default time.Now).
	Now func() time.Time
}

// DefaultBackupMaxBytes is the default of Config.BackupMaxBytes
// (--backup-max-mb 1024).
const DefaultBackupMaxBytes = 1024 << 20

// DefaultSnapshotDeadline is the default of Config.SnapshotDeadline.
const DefaultSnapshotDeadline = 3 * time.Minute

// Service implements server operations. It is safe for concurrent use.
type Service struct {
	store  *store.Store
	box    *secrets.Box
	docker *docker.Client
	cfg    Config
	logger *slog.Logger

	// writeMu serialises create, update and import in this process so
	// auto-assigned ports do not race; env updates are additionally
	// compare-and-swapped in the store.
	writeMu sync.Mutex
	// foreign remembers containers already logged as foreign (they carry
	// our labels but are not the container recorded for any server), so
	// each is logged once.
	foreign sync.Map
	// knownHelpers holds the launch ids of helper containers this process
	// created or found recorded (they are neither servers nor foreign);
	// activeHelpers those a running job still uses (cleanup skips them).
	knownHelpers  sync.Map
	activeHelpers sync.Map
	// pendingVolumes holds data volumes a running job created and has not
	// recorded (or switched to) yet; they cannot be deleted meanwhile.
	pendingVolumes sync.Map
}

// New returns a Service.
func New(st *store.Store, box *secrets.Box, dc *docker.Client, cfg Config, logger *slog.Logger) *Service {
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = docker.DefaultStopTimeout
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = DefaultStartTimeout
	}
	if cfg.ReadyPoll <= 0 {
		cfg.ReadyPoll = 500 * time.Millisecond
	}
	if cfg.ProbePort == nil {
		cfg.ProbePort = ports.ProbeFree
	}
	if cfg.BackupMaxBytes <= 0 {
		cfg.BackupMaxBytes = DefaultBackupMaxBytes
	}
	if cfg.SnapshotDeadline <= 0 {
		cfg.SnapshotDeadline = DefaultSnapshotDeadline
	}
	if cfg.SnapshotRetry <= 0 {
		cfg.SnapshotRetry = 2 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: st, box: box, docker: dc, cfg: cfg, logger: logger}
}

// BackupMaxBytes returns the largest data directory a backup accepts.
func (s *Service) BackupMaxBytes() int64 { return s.cfg.BackupMaxBytes }

// PortRange returns the configured host port range.
func (s *Service) PortRange() ports.Range { return s.cfg.Ports }

// SetPanelPort records the bound port when the daemon did not have a numeric
// port at configuration time. Call it before serving requests or reconciling.
func (s *Service) SetPanelPort(port int) { s.cfg.PanelPort = port }

// GameHost returns the effective game host: the game_host setting, or
// Config.DefaultGameHost when it is empty ("" when neither is set).
func (s *Service) GameHost(ctx context.Context) (string, error) {
	st, err := s.store.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if st.GameHost != "" {
		return st.GameHost, nil
	}
	return s.cfg.DefaultGameHost, nil
}

// params reads the installation-wide container inputs.
func (s *Service) params(ctx context.Context) (runtimeParams, error) {
	host, err := s.GameHost(ctx)
	if err != nil {
		return runtimeParams{}, err
	}
	p := runtimeParams{gameHost: host, testOrigins: s.cfg.testOrigins(host)}
	if s.cfg.Certs != nil {
		p.groupAdd = s.cfg.Certs.GroupAdd()
	}
	return p, nil
}

func ref(sv store.Server) docker.ServerRef {
	return docker.ServerRef{ServerID: sv.ID, ContainerID: sv.ContainerID}
}

// logForeign logs, once per container, a container that carries our
// labels but is not the one recorded for its server. It is never stopped,
// removed or read.
func (s *Service) logForeign(ct docker.Container) {
	if _, seen := s.foreign.LoadOrStore(ct.ID, true); seen {
		return
	}
	s.logger.Warn("ignoring a container with golem-deploy labels that is not the container recorded for any server (labels can be inherited from an image)",
		"container", ct.Name, "id", shortID(ct.ID), "server_id_label", ct.Labels[docker.LabelServerID])
}

func (s *Service) logForeignRef(r docker.ServerRef) {
	if _, seen := s.foreign.LoadOrStore(r.ContainerID, true); seen {
		return
	}
	s.logger.Warn("the container recorded for a server no longer carries this installation's labels; ignoring it",
		"server_id", r.ServerID, "id", shortID(r.ContainerID))
}

func (s *Service) openEnv(sv store.Server) (map[string]string, error) {
	env := map[string]string{}
	if len(sv.Env) == 0 {
		return env, nil
	}
	pt, err := s.box.Open(secrets.ServerEnvPurpose(sv.ID), sv.Env)
	if err != nil {
		return nil, fmt.Errorf("open env of server %d: %w", sv.ID, err)
	}
	if err := json.Unmarshal(pt, &env); err != nil {
		return nil, fmt.Errorf("decode env of server %d: %w", sv.ID, err)
	}
	return env, nil
}

func (s *Service) sealEnv(id int64, env map[string]string) ([]byte, error) {
	if len(env) == 0 {
		return nil, nil
	}
	pt, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(secrets.ServerEnvPurpose(id), pt)
}

// newAdminToken returns a fresh per-container admin token (64 hex
// characters) and its ciphertext for the server row.
func (s *Service) newAdminToken(id int64) (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := hex.EncodeToString(b)
	sealed, err := s.box.Seal(secrets.ServerAdminTokenPurpose(id), []byte(tok))
	if err != nil {
		return "", nil, err
	}
	return tok, sealed, nil
}

func (s *Service) openAdminToken(sv store.Server) (string, error) {
	if len(sv.AdminToken) == 0 {
		return "", errors.New("no admin token recorded")
	}
	pt, err := s.box.Open(secrets.ServerAdminTokenPurpose(sv.ID), sv.AdminToken)
	if err != nil {
		return "", fmt.Errorf("open admin token of server %d: %w", sv.ID, err)
	}
	return string(pt), nil
}

func (s *Service) runSpec(sv store.Server, b store.Build, env map[string]string, img docker.Image, p runtimeParams, token string) docker.RunSpec {
	spec := docker.RunSpec{
		ServerID:      sv.ID,
		ServerName:    sv.Name,
		ProjectID:     &b.ProjectID,
		BuildID:       b.ID,
		Commit:        b.Commit,
		EngineVersion: b.EngineVersion,
		Image:         img.ID,
		ImageVolumes:  img.Volumes,
		HostPort:      *sv.Port,
		BindAddress:   s.cfg.BindAddress,
		Env:           env,
		DaemonEnv:     engineEnv(sv, &b, p, token),
		MemoryMB:      s.cfg.MemoryMB,
		CPUs:          s.cfg.CPUs,
		StopTimeout:   s.cfg.StopTimeout,
		ConfigHash:    s.configHash(sv, &b, p),
		GroupAdd:      p.groupAdd,
		AdminPort:     AdminContainerPort,
	}
	if sv.DataVolume != "" {
		spec.DataVolume = &docker.VolumeMount{Name: sv.DataVolume, Target: build.DataDir}
	}
	if p.certDir != "" {
		spec.ReadOnlyBinds = []docker.BindMount{{Source: p.certDir, Target: certs.ContainerDir}}
	}
	return spec
}

func mapState(st string) apitypes.ServerState {
	switch st {
	case "created":
		return apitypes.ServerStateCreated
	case "running":
		return apitypes.ServerStateRunning
	case "restarting":
		return apitypes.ServerStateRestarting
	case "paused":
		return apitypes.ServerStatePaused
	case "dead":
		return apitypes.ServerStateDead
	default: // exited, removing
		return apitypes.ServerStateExited
	}
}

// buildCache loads builds once per request.
type buildCache struct {
	s *Service
	m map[int64]*store.Build
}

func (s *Service) builds() *buildCache { return &buildCache{s: s, m: map[int64]*store.Build{}} }

// get returns the server's build, nil when it has none (or the row is
// gone, which the store's foreign key prevents).
func (c *buildCache) get(ctx context.Context, sv store.Server) (*store.Build, error) {
	if sv.BuildID == nil {
		return nil, nil
	}
	if b, ok := c.m[*sv.BuildID]; ok {
		return b, nil
	}
	b, err := c.s.store.GetBuild(ctx, *sv.BuildID)
	if errors.Is(err, store.ErrNotFound) {
		c.m[*sv.BuildID] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.m[*sv.BuildID] = &b
	return &b, nil
}

// view renders a server. b is its build (nil when it has none), ct its
// recorded container (nil when it has none); dockerOK false means runtime
// state is unknown. lastBackup is when its newest backup was taken (nil
// without one).
func (s *Service) view(ctx context.Context, sv store.Server, b *store.Build, p runtimeParams, ct *docker.Container, dockerOK bool, lastBackup *time.Time) (apitypes.Server, error) {
	env, err := s.openEnv(sv)
	if err != nil {
		return apitypes.Server{}, err
	}
	port := 0
	if sv.Port != nil {
		port = *sv.Port
	}
	out := apitypes.Server{
		ID:             sv.ID,
		Name:           sv.Name,
		BuildID:        sv.BuildID,
		Port:           port,
		PublicURL:      publicURL(p.gameHost, port),
		AllowedOrigins: append([]string{}, sv.AllowedOrigins...),
		EnvKeys:        docker.EnvKeys(env),
		DesiredState:   apitypes.DesiredState(sv.DesiredState),
		State:          apitypes.ServerStateMissing,
		Data: apitypes.ServerData{
			Volume:            sv.DataVolume,
			SchemaFingerprint: sv.DataFingerprint,
			LastBackupAt:      lastBackup,
		},
		SnapshotIntervalSeconds: sv.SnapshotIntervalSeconds,
		BackupSchedule:          apitypes.BackupSchedule{IntervalHours: sv.BackupIntervalHours, Keep: sv.BackupKeep},
		CreatedAt:               sv.CreatedAt.UTC(),
		UpdatedAt:               sv.UpdatedAt.UTC(),
	}
	out.Certificate, err = s.serverCertificate(ctx, sv.ID, &p)
	if err != nil {
		return apitypes.Server{}, err
	}
	if b != nil {
		pid := b.ProjectID
		out.ProjectID = &pid
		out.Image, out.Commit, out.EngineVersion = b.ImageID, b.Commit, b.EngineVersion
		out.Data.SchemaMismatch = schemaMismatch(sv.DataFingerprint, b.SchemaFingerprint)
	}
	switch {
	case sv.ContainerID == "":
		// No container recorded: there is none, whatever Docker says.
	case !dockerOK:
		out.State = apitypes.ServerStateUnknown
	case ct != nil:
		out.State = mapState(ct.State)
		out.ContainerID = ct.ID
		if !ct.StartedAt.IsZero() {
			t := ct.StartedAt
			out.StartedAt = &t
		}
		if !ct.Running && ct.State != "created" {
			code := ct.ExitCode
			out.ExitCode = &code
		}
		out.RestartRequired = s.restartRequired(sv, b, p, ct)
	}
	return out, nil
}

func (s *Service) restartRequired(sv store.Server, b *store.Build, p runtimeParams, ct *docker.Container) bool {
	return ct.Running && ct.Labels[docker.LabelConfigHash] != s.configHash(sv, b, p)
}

// dockerFailed logs a failed runtime query whose result is reported as
// state "unknown". When the failure is only ctx being cancelled by its
// caller it logs at debug level and returns an error, so the request ends
// as cancelled instead of reporting an unknown state to nobody.
func (s *Service) dockerFailed(ctx context.Context, op string, err error) error {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		// The caller (an HTTP client that went away) cancelled the query:
		// nothing failed, and nobody is left to see an unknown state.
		s.logger.Debug("docker query cancelled with its caller", "op", op)
		return fmt.Errorf("%s: %w", op, err)
	}
	if !errors.Is(err, docker.ErrUnavailable) {
		s.logger.Warn("docker query failed; reporting unknown state", "op", op, "err", err)
	}
	return nil
}

// containers indexes managed containers by id and logs the ones that are
// not the recorded container of any row.
func (s *Service) containers(rows []store.Server, cts []docker.Container) map[int64]*docker.Container {
	byID := make(map[string]*docker.Container, len(cts))
	for i := range cts {
		byID[cts[i].ID] = &cts[i]
	}
	out := map[int64]*docker.Container{}
	claimed := map[string]bool{}
	pending := map[int64]store.Server{}
	for _, sv := range rows {
		if sv.PendingLaunch != "" {
			pending[sv.ID] = sv
		}
		if ct := byID[sv.ContainerID]; ct != nil && s.docker.IsServerContainer(*ct, ref(sv)) {
			out[sv.ID] = ct
			claimed[ct.ID] = true
		}
	}
	for i := range cts {
		ct := cts[i]
		sv := pending[ct.ServerID]
		if claimed[ct.ID] || s.docker.IsPendingServerContainer(ct, sv.ID, sv.Name, sv.PendingLaunch) {
			continue
		}
		if _, helper := s.knownHelpers.Load(ct.Labels[docker.LabelHelper]); helper && docker.IsHelper(ct) {
			continue // a recorded helper container
		}
		s.logForeign(ct)
	}
	return out
}

// List returns every server with runtime state from one container query.
// When Docker cannot be queried, servers are returned with state "unknown".
func (s *Service) List(ctx context.Context) ([]apitypes.Server, error) {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.params(ctx)
	if err != nil {
		return nil, err
	}
	cts, derr := s.docker.ManagedContainers(ctx)
	if derr != nil {
		if err := s.dockerFailed(ctx, "list servers", derr); err != nil {
			return nil, err
		}
	}
	byServer := s.containers(rows, cts)
	last, err := s.store.LastBackups(ctx)
	if err != nil {
		return nil, err
	}
	bc := s.builds()
	out := make([]apitypes.Server, 0, len(rows))
	for _, sv := range rows {
		b, err := bc.get(ctx, sv)
		if err != nil {
			return nil, err
		}
		v, err := s.view(ctx, sv, b, p, byServer[sv.ID], derr == nil, timePtr(last, sv.ID))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, id int64) (store.Server, error) {
	sv, err := s.store.GetServer(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Server{}, ErrNotFound
	}
	return sv, err
}

// container returns the server's recorded container, nil when it has none
// (or the recorded id now names a container that is not ours).
func (s *Service) container(ctx context.Context, sv store.Server) (*docker.Container, error) {
	ct, err := s.docker.ServerContainer(ctx, ref(sv))
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return nil, nil
	case errors.Is(err, docker.ErrNotOurs):
		s.logForeignRef(ref(sv))
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &ct, nil
}

// viewOne renders one server with its container ct (dockerOK as in view).
func (s *Service) viewOne(ctx context.Context, sv store.Server, ct *docker.Container, dockerOK bool) (apitypes.Server, error) {
	p, err := s.params(ctx)
	if err != nil {
		return apitypes.Server{}, err
	}
	b, err := s.builds().get(ctx, sv)
	if err != nil {
		return apitypes.Server{}, err
	}
	last, err := s.store.LastBackups(ctx)
	if err != nil {
		return apitypes.Server{}, err
	}
	return s.view(ctx, sv, b, p, ct, dockerOK, timePtr(last, sv.ID))
}

func timePtr(m map[int64]time.Time, id int64) *time.Time {
	t, ok := m[id]
	if !ok {
		return nil
	}
	t = t.UTC()
	return &t
}

// Get returns one server with runtime state.
func (s *Service) Get(ctx context.Context, id int64) (apitypes.Server, error) {
	sv, ct, dockerOK, err := s.inspect(ctx, id)
	if err != nil {
		return apitypes.Server{}, err
	}
	return s.viewOne(ctx, sv, ct, dockerOK)
}

func (s *Service) inspect(ctx context.Context, id int64) (store.Server, *docker.Container, bool, error) {
	sv, err := s.load(ctx, id)
	if err != nil {
		return sv, nil, false, err
	}
	ct, derr := s.container(ctx, sv)
	if derr != nil {
		if err := s.dockerFailed(ctx, "get server", derr); err != nil {
			return sv, nil, false, err
		}
	}
	return sv, ct, derr == nil, nil
}

// resolveBuild returns build id if it is a successful build with an image.
// Docker is not asked here: the image is verified when the server starts.
// errBuildGone: the build passed resolveBuild but was deleted before the
// server row was written (the foreign key refused it).
var errBuildGone = fieldErr("build_id", "the build was deleted meanwhile; choose another build")

func (s *Service) resolveBuild(ctx context.Context, field string, id int64) (store.Build, error) {
	if id <= 0 {
		return store.Build{}, fieldErr(field, "build_id is required (the id of a successful build)")
	}
	b, err := s.store.GetBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Build{}, fieldErr(field, "build %d does not exist", id)
	}
	if err != nil {
		return store.Build{}, err
	}
	if b.Status != store.BuildStatusSuccess || b.ImageID == "" {
		return store.Build{}, fieldErr(field, "build %d has not succeeded; servers run successful builds only", id)
	}
	return b, nil
}

// validateEnv checks env and names the offending field: field for names
// and limits, field.KEY for a rejected value.
func validateEnv(field string, env map[string]string) error {
	if err := docker.ValidateEnv(env); err != nil {
		var ve *docker.ValidationError
		if errors.As(err, &ve) {
			if ve.Key != "" {
				return &FieldError{Field: field + "." + ve.Key, Msg: ve.Msg}
			}
			return &FieldError{Field: field, Msg: ve.Msg}
		}
		return err
	}
	return nil
}

func (s *Service) checkPort(field string, p int) error {
	if !s.portAllowed(p) {
		return fieldErr(field, "port must be inside the configured range %s (browser-unsafe ports excluded)", s.cfg.Ports)
	}
	return nil
}

// portAllowed reports whether a server may use host port p now: inside
// the configured range, not browser-unsafe and not the panel's port.
func (s *Service) portAllowed(p int) bool {
	return s.cfg.Ports.Allowed(p) && p != s.cfg.PanelPort
}

// conflictCause tells a name collision from a port collision after the
// store reported ErrConflict.
func (s *Service) conflictCause(ctx context.Context, name string, port int) error {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Name == name {
			return ErrNameTaken
		}
	}
	for _, r := range rows {
		if r.Port != nil && *r.Port == port {
			return ErrPortInUse
		}
	}
	return ErrConflict
}

func (s *Service) usedPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, r := range rows {
		if r.Port != nil {
			used[*r.Port] = true
		}
	}
	return used, nil
}

// Create validates and stores a new server. Starting it (desired_state
// "running") is up to the caller.
func (s *Service) Create(ctx context.Context, req apitypes.CreateServerRequest) (apitypes.Server, error) {
	if err := docker.ValidateServerName(req.Name); err != nil {
		return apitypes.Server{}, fieldErr("name", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit")
	}
	if err := validateEnv("env", req.Env); err != nil {
		return apitypes.Server{}, err
	}
	origins, err := validateOrigins("allowed_origins", req.AllowedOrigins)
	if err != nil {
		return apitypes.Server{}, err
	}
	desired := apitypes.DesiredStateStopped
	if req.DesiredState != nil {
		desired = *req.DesiredState
		if desired != apitypes.DesiredStateRunning && desired != apitypes.DesiredStateStopped {
			return apitypes.Server{}, fieldErr("desired_state", `desired_state must be "running" or "stopped"`)
		}
	}
	if req.Port != nil {
		if err := s.checkPort("port", *req.Port); err != nil {
			return apitypes.Server{}, err
		}
	}
	snapshot, interval, keep, err := dataSettings("", req.SnapshotIntervalSeconds, req.BackupSchedule, store.DefaultSnapshotIntervalSeconds, 0, store.DefaultBackupKeep)
	if err != nil {
		return apitypes.Server{}, err
	}
	b, err := s.resolveBuild(ctx, "build_id", req.BuildID)
	if err != nil {
		return apitypes.Server{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	seal := func(id int64) ([]byte, error) { return s.sealEnv(id, req.Env) }
	row := store.Server{Name: req.Name, BuildID: &b.ID, AllowedOrigins: origins, DesiredState: string(desired),
		SnapshotIntervalSeconds: snapshot, BackupIntervalHours: interval, BackupKeep: keep}
	for attempt := 0; ; attempt++ {
		port := 0
		if req.Port != nil {
			port = *req.Port
		} else {
			used, err := s.usedPorts(ctx)
			if err != nil {
				return apitypes.Server{}, err
			}
			if s.cfg.PanelPort != 0 {
				used[s.cfg.PanelPort] = true
			}
			if port, err = s.cfg.Ports.Lowest(used); err != nil {
				return apitypes.Server{}, fmt.Errorf("%w: %w", ErrPortInUse, err)
			}
		}
		row.Port = &port
		created, err := s.store.CreateServer(ctx, row, seal)
		if err == nil {
			s.logger.Info("server created", "server_id", created.ID, "name", created.Name, "build_id", b.ID, "port", port, "env_keys", len(req.Env))
			return s.viewOne(ctx, created, nil, true)
		}
		if errors.Is(err, store.ErrMissingReference) {
			return apitypes.Server{}, errBuildGone
		}
		if !errors.Is(err, store.ErrConflict) {
			return apitypes.Server{}, err
		}
		cause := s.conflictCause(ctx, req.Name, port)
		// Another process (e.g. an import) took the auto-assigned port: retry.
		if errors.Is(cause, ErrPortInUse) && req.Port == nil && attempt < 32 {
			continue
		}
		return apitypes.Server{}, cause
	}
}

// Update applies a PATCH. It never restarts the server; the returned
// view's RestartRequired tells whether the running container is stale. It
// never writes desired_state (start/stop jobs own it) and changes the env
// with a compare-and-swap, so concurrent changes are not lost. Changing the
// port of a server whose container runs is ErrRunning.
func (s *Service) Update(ctx context.Context, id int64, req apitypes.UpdateServerRequest) (apitypes.Server, error) {
	if err := validateEnv("env_set", req.EnvSet); err != nil {
		return apitypes.Server{}, err
	}
	for _, k := range req.EnvUnset {
		if err := docker.ValidateEnvKey(k); err != nil {
			return apitypes.Server{}, fieldErr("env_unset", "invalid variable name")
		}
		if _, both := req.EnvSet[k]; both {
			return apitypes.Server{}, fieldErr("env_unset", "a variable cannot be in both env_set and env_unset")
		}
	}
	var origins []string
	if req.AllowedOrigins != nil {
		var err error
		if origins, err = validateOrigins("allowed_origins", *req.AllowedOrigins); err != nil {
			return apitypes.Server{}, err
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var updated store.Server
	for attempt := 0; ; attempt++ {
		sv, err := s.load(ctx, id)
		if err != nil {
			return apitypes.Server{}, err
		}
		u := store.ServerUpdate{SetOrigins: req.AllowedOrigins != nil, AllowedOrigins: origins}
		if req.SnapshotIntervalSeconds != nil || req.BackupSchedule != nil {
			snapshot, interval, keep, err := dataSettings("", req.SnapshotIntervalSeconds, req.BackupSchedule,
				sv.SnapshotIntervalSeconds, sv.BackupIntervalHours, sv.BackupKeep)
			if err != nil {
				return apitypes.Server{}, err
			}
			if req.SnapshotIntervalSeconds != nil {
				u.SnapshotIntervalSeconds = &snapshot
			}
			if req.BackupSchedule != nil {
				u.BackupIntervalHours, u.BackupKeep = &interval, &keep
			}
		}
		portChange := req.Port != nil && (sv.Port == nil || *sv.Port != *req.Port)
		buildChange := req.BuildID != nil && (sv.BuildID == nil || *sv.BuildID != *req.BuildID)
		if portChange || buildChange {
			if err := s.store.CheckServerJob(ctx, id); err != nil {
				return apitypes.Server{}, err
			}
		}
		if buildChange {
			if _, err := s.resolveBuild(ctx, "build_id", *req.BuildID); err != nil {
				return apitypes.Server{}, err
			}
			u.BuildID = req.BuildID
		}
		if portChange {
			if err := s.checkPort("port", *req.Port); err != nil {
				return apitypes.Server{}, err
			}
			if err := s.checkStopped(ctx, sv); err != nil {
				return apitypes.Server{}, err
			}
			p := *req.Port
			u.Port = &p
			u.PrevContainerID = sv.ContainerID
		}
		if len(req.EnvSet) > 0 || len(req.EnvUnset) > 0 {
			env, err := s.openEnv(sv)
			if err != nil {
				return apitypes.Server{}, err
			}
			for _, k := range req.EnvUnset {
				delete(env, k)
			}
			for k, v := range req.EnvSet {
				// Replace a variable that differs only in case.
				for old := range env {
					if old != k && equalFold(old, k) {
						delete(env, old)
					}
				}
				env[k] = v
			}
			if err := validateEnv("env_set", env); err != nil {
				return apitypes.Server{}, err
			}
			if u.Env, err = s.sealEnv(sv.ID, env); err != nil {
				return apitypes.Server{}, err
			}
			u.SetEnv, u.PrevEnv = true, sv.Env
		}
		updated, err = s.store.UpdateServer(ctx, id, u)
		if errors.Is(err, store.ErrStale) && attempt < 8 {
			continue // another process changed the env; apply ours on top
		}
		switch {
		case errors.Is(err, store.ErrStale):
			return apitypes.Server{}, ErrConflict
		case errors.Is(err, store.ErrServerProject):
			return apitypes.Server{}, fieldErr("build_id", "the build must belong to the server's project")
		case errors.Is(err, store.ErrLegacyServerEnv):
			return apitypes.Server{}, fieldErr("build_id", "clear the legacy server's saved environment before assigning its first build")
		case errors.Is(err, store.ErrMissingReference):
			return apitypes.Server{}, errBuildGone
		case errors.Is(err, store.ErrConflict):
			return apitypes.Server{}, ErrPortInUse
		case errors.Is(err, store.ErrNotFound):
			return apitypes.Server{}, ErrNotFound
		case err != nil:
			return apitypes.Server{}, err
		}
		break
	}
	s.logger.Info("server updated", "server_id", id, "build_changed", req.BuildID != nil, "port_changed", req.Port != nil,
		"origins_changed", req.AllowedOrigins != nil, "env_set", len(req.EnvSet), "env_unset", len(req.EnvUnset),
		"snapshot_interval_changed", req.SnapshotIntervalSeconds != nil, "backup_schedule_changed", req.BackupSchedule != nil)
	ct, derr := s.container(ctx, updated)
	if derr != nil {
		// The update is stored; a cancelled caller only loses the view.
		_ = s.dockerFailed(ctx, "update server", derr)
	}
	return s.viewOne(ctx, updated, ct, derr == nil)
}

// checkStopped returns ErrRunning when the server's container is running
// (or restarting or paused), and Docker's error when that cannot be told.
func (s *Service) checkStopped(ctx context.Context, sv store.Server) error {
	if sv.ContainerID == "" {
		return nil
	}
	ct, err := s.container(ctx, sv)
	if err != nil {
		return err
	}
	if ct != nil && (ct.Running || ct.State == "restarting" || ct.State == "paused") {
		return ErrRunning
	}
	return nil
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'a' <= x && x <= 'z' {
			x -= 'a' - 'A'
		}
		if 'a' <= y && y <= 'z' {
			y -= 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// Stats samples the server's running container.
func (s *Service) Stats(ctx context.Context, id int64, now time.Time) (apitypes.ServerStats, error) {
	sv, err := s.load(ctx, id)
	if err != nil {
		return apitypes.ServerStats{}, err
	}
	st, err := s.docker.ServerStats(ctx, ref(sv))
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return apitypes.ServerStats{}, ErrNoContainer
	case errors.Is(err, docker.ErrNotOurs):
		s.logForeignRef(ref(sv))
		return apitypes.ServerStats{}, ErrNoContainer
	case errors.Is(err, docker.ErrNotRunning):
		return apitypes.ServerStats{}, ErrNotRunning
	case err != nil:
		return apitypes.ServerStats{}, err
	}
	return apitypes.ServerStats{
		CPUPercent:       st.CPUPercent,
		MemoryBytes:      st.MemoryBytes,
		MemoryLimitBytes: st.MemoryLimitBytes,
		NetworkRxBytes:   st.NetworkRxBytes,
		NetworkTxBytes:   st.NetworkTxBytes,
		PIDs:             st.PIDs,
		SampledAt:        now.UTC(),
	}, nil
}

// CheckLogs verifies the server and its container exist before a log
// response starts.
func (s *Service) CheckLogs(ctx context.Context, id int64) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	ct, err := s.container(ctx, sv)
	if err != nil {
		return err
	}
	if ct == nil {
		return ErrNoContainer
	}
	return nil
}

// Logs writes the server's container output to w, sanitised (control
// characters escaped, invalid UTF-8 replaced, lines capped; see
// internal/logsafe) but not redacted: it is the game's own output.
func (s *Service) Logs(ctx context.Context, id int64, o docker.LogOptions, w io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	san := logsafe.NewWriter(w, "")
	err = s.docker.ServerLogs(ctx, ref(sv), o, san)
	if cerr := san.Close(); err == nil && ctx.Err() == nil {
		err = cerr
	}
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return ErrNoContainer
	case errors.Is(err, docker.ErrNotOurs):
		s.logForeignRef(ref(sv))
		return ErrNoContainer
	}
	return err
}
