// Package servers is the game server service: server definitions in the
// store (env sealed with secrets.ServerEnvPurpose), their containers through
// the label-scoped docker client (each server's container is the one whose
// id is recorded in its row), host port allocation, the start/stop/
// restart/delete job bodies, export/import, startup reconciliation, the
// managed image and volume views that cross-reference servers, and the
// sanitising of container output shown to users.
package servers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/ports"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Errors returned by Service. Handlers map them to API errors; docker
// errors (docker.ErrUnavailable in particular) pass through wrapped.
var (
	ErrNotFound    = errors.New("servers: not found")
	ErrNameTaken   = errors.New("servers: name taken")
	ErrPortInUse   = errors.New("servers: port in use")
	ErrNoContainer = errors.New("servers: server has no container")
	ErrNotRunning  = errors.New("servers: server is not running")
	ErrInUse       = errors.New("servers: object is in use")
	ErrConflict    = errors.New("servers: conflicting change")
	// ErrRunning: the change needs the server to be stopped first (its
	// port cannot change while its container runs).
	ErrRunning = errors.New("servers: server is running")
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

// Config holds daemon-wide server settings.
type Config struct {
	// Ports is the validated host port range.
	Ports ports.Range
	// PanelPort is the panel's own listen port (0 when unknown); a server
	// can never be started on it.
	PanelPort int
	// BindAddress is the host IP game ports are published on ("" = all).
	BindAddress string
	// MemoryMB and CPUs are per-container limits (0 = unlimited).
	MemoryMB int
	CPUs     float64
	// StopTimeout is the graceful stop timeout (default 30s).
	StopTimeout time.Duration
	// SettleWindow is how long a started container must keep running for
	// the start job to succeed (default 3s); SettlePoll is the poll interval.
	SettleWindow time.Duration
	SettlePoll   time.Duration
	// ProbePort checks that nothing else listens on a host port before a
	// start (default ports.ProbeFree).
	ProbePort func(bindAddr string, port int) error
}

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
}

// New returns a Service.
func New(st *store.Store, box *secrets.Box, dc *docker.Client, cfg Config, logger *slog.Logger) *Service {
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = docker.DefaultStopTimeout
	}
	if cfg.SettleWindow <= 0 {
		cfg.SettleWindow = 3 * time.Second
	}
	if cfg.SettlePoll <= 0 {
		cfg.SettlePoll = 250 * time.Millisecond
	}
	if cfg.ProbePort == nil {
		cfg.ProbePort = ports.ProbeFree
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: st, box: box, docker: dc, cfg: cfg, logger: logger}
}

// PortRange returns the configured host port range.
func (s *Service) PortRange() ports.Range { return s.cfg.Ports }

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

// configHash fingerprints everything a container is created from. Env is
// represented by a hash of its ciphertext, which is re-sealed (with a fresh
// nonce) on every env change and reveals nothing about the values.
func (s *Service) configHash(sv store.Server) string {
	h := sha256.New()
	port := 0
	if sv.Port != nil {
		port = *sv.Port
	}
	envSum := sha256.Sum256(sv.Env)
	fmt.Fprintf(h, "v1\x00%s\x00%d\x00%s\x00%d\x00%g\x00%x", sv.Image, port, s.cfg.BindAddress, s.cfg.MemoryMB, s.cfg.CPUs, envSum)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (s *Service) runSpec(sv store.Server, env map[string]string, img docker.Image) docker.RunSpec {
	return docker.RunSpec{
		ServerID:     sv.ID,
		ServerName:   sv.Name,
		ProjectID:    sv.ProjectID,
		Image:        img.ID,
		ImageVolumes: img.Volumes,
		HostPort:     *sv.Port,
		BindAddress:  s.cfg.BindAddress,
		Env:          env,
		MemoryMB:     s.cfg.MemoryMB,
		CPUs:         s.cfg.CPUs,
		StopTimeout:  s.cfg.StopTimeout,
		ConfigHash:   s.configHash(sv),
	}
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

// view renders a server. ct is its recorded container (nil when it has
// none); dockerOK false means runtime state is unknown.
func (s *Service) view(sv store.Server, ct *docker.Container, dockerOK bool) (apitypes.Server, error) {
	env, err := s.openEnv(sv)
	if err != nil {
		return apitypes.Server{}, err
	}
	port := 0
	if sv.Port != nil {
		port = *sv.Port
	}
	out := apitypes.Server{
		ID:           sv.ID,
		Name:         sv.Name,
		ProjectID:    sv.ProjectID,
		Image:        sv.Image,
		Port:         port,
		EnvKeys:      docker.EnvKeys(env),
		DesiredState: apitypes.DesiredState(sv.DesiredState),
		State:        apitypes.ServerStateMissing,
		CreatedAt:    sv.CreatedAt.UTC(),
		UpdatedAt:    sv.UpdatedAt.UTC(),
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
		out.RestartRequired = ct.Running && ct.Labels[docker.LabelConfigHash] != s.configHash(sv)
	}
	return out, nil
}

func (s *Service) dockerFailed(op string, err error) {
	if !errors.Is(err, docker.ErrUnavailable) {
		s.logger.Warn("docker query failed; reporting unknown state", "op", op, "err", err)
	}
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
	for _, sv := range rows {
		if ct := byID[sv.ContainerID]; ct != nil && s.docker.IsServerContainer(*ct, ref(sv)) {
			out[sv.ID] = ct
			claimed[ct.ID] = true
		}
	}
	for i := range cts {
		if !claimed[cts[i].ID] {
			s.logForeign(cts[i])
		}
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
	cts, derr := s.docker.ManagedContainers(ctx)
	if derr != nil {
		s.dockerFailed("list servers", derr)
	}
	byServer := s.containers(rows, cts)
	out := make([]apitypes.Server, 0, len(rows))
	for _, sv := range rows {
		v, err := s.view(sv, byServer[sv.ID], derr == nil)
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

// Get returns one server with runtime state.
func (s *Service) Get(ctx context.Context, id int64) (apitypes.Server, error) {
	sv, err := s.load(ctx, id)
	if err != nil {
		return apitypes.Server{}, err
	}
	ct, derr := s.container(ctx, sv)
	if derr != nil {
		s.dockerFailed("get server", derr)
	}
	return s.view(sv, ct, derr == nil)
}

// resolveImage returns the immutable id of a managed image reference.
func (s *Service) resolveImage(ctx context.Context, field, ref string) (string, error) {
	if err := docker.ValidateImageRef(ref); err != nil {
		return "", fieldErr(field, "image must be an image id or a name[:tag]")
	}
	img, err := s.docker.ManagedImage(ctx, ref)
	switch {
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotManaged):
		return "", fieldErr(field, "image must be a managed image (built and labelled by golem-deploy)")
	case err != nil:
		return "", err
	}
	if len(img.Volumes) > docker.MaxImageVolumes {
		return "", fieldErr(field, "the image declares more than %d volumes", docker.MaxImageVolumes)
	}
	return img.ID, nil
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
	if req.ProjectID != nil {
		if _, err := s.store.GetProject(ctx, *req.ProjectID); errors.Is(err, store.ErrNotFound) {
			return apitypes.Server{}, fieldErr("project_id", "project does not exist")
		} else if err != nil {
			return apitypes.Server{}, err
		}
	}
	if err := validateEnv("env", req.Env); err != nil {
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
	imageID, err := s.resolveImage(ctx, "image", req.Image)
	if err != nil {
		return apitypes.Server{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	seal := func(id int64) ([]byte, error) { return s.sealEnv(id, req.Env) }
	row := store.Server{Name: req.Name, ProjectID: req.ProjectID, Image: imageID, DesiredState: string(desired)}
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
			s.logger.Info("server created", "server_id", created.ID, "name", created.Name, "port", port, "env_keys", len(req.Env))
			return s.view(created, nil, true)
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
	if req.Port != nil {
		if err := s.checkPort("port", *req.Port); err != nil {
			return apitypes.Server{}, err
		}
	}
	imageID := ""
	if req.Image != nil {
		var err error
		if imageID, err = s.resolveImage(ctx, "image", *req.Image); err != nil {
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
		u := store.ServerUpdate{}
		if req.Image != nil {
			u.Image = &imageID
		}
		if req.Port != nil && (sv.Port == nil || *sv.Port != *req.Port) {
			if err := s.checkStopped(ctx, sv); err != nil {
				return apitypes.Server{}, err
			}
			p := *req.Port
			u.Port = &p
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
		case errors.Is(err, store.ErrConflict):
			return apitypes.Server{}, ErrPortInUse
		case errors.Is(err, store.ErrNotFound):
			return apitypes.Server{}, ErrNotFound
		case err != nil:
			return apitypes.Server{}, err
		}
		break
	}
	s.logger.Info("server updated", "server_id", id, "image_changed", req.Image != nil, "port_changed", req.Port != nil,
		"env_set", len(req.EnvSet), "env_unset", len(req.EnvUnset))
	ct, derr := s.container(ctx, updated)
	if derr != nil {
		s.dockerFailed("update server", derr)
	}
	return s.view(updated, ct, derr == nil)
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
// sanitizer) but not redacted: it is the game's own output.
func (s *Service) Logs(ctx context.Context, id int64, o docker.LogOptions, w io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	san := newSanitizer(w, "")
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
