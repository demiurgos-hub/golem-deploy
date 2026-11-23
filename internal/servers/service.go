// Package servers is the game server service: server definitions in the
// store (env sealed with secrets.ServerEnvPurpose), their containers through
// the label-scoped docker client, host port allocation, the start/stop/
// restart/delete job bodies, export/import, startup reconciliation, and the
// managed image and volume views that cross-reference servers.
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

	// writeMu serialises read-modify-write changes of server rows (create,
	// update, import) so concurrent PATCHes cannot lose env changes and
	// auto-assigned ports do not race within this process.
	writeMu sync.Mutex
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

func (s *Service) runSpec(sv store.Server, env map[string]string, imageID string) docker.RunSpec {
	return docker.RunSpec{
		ServerID:    sv.ID,
		ServerName:  sv.Name,
		ProjectID:   sv.ProjectID,
		Image:       imageID,
		HostPort:    *sv.Port,
		BindAddress: s.cfg.BindAddress,
		Env:         env,
		MemoryMB:    s.cfg.MemoryMB,
		CPUs:        s.cfg.CPUs,
		StopTimeout: s.cfg.StopTimeout,
		ConfigHash:  s.configHash(sv),
	}
}

// pickContainer chooses the container representing a server: a running
// one, else the newest.
func pickContainer(list []docker.Container) *docker.Container {
	var best *docker.Container
	for i := range list {
		c := &list[i]
		switch {
		case best == nil:
			best = c
		case c.Running && !best.Running:
			best = c
		case c.Running == best.Running && c.Created.After(best.Created):
			best = c
		}
	}
	return best
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

// view renders a server. dockerOK false means runtime state is unknown.
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
	byServer := map[int64][]docker.Container{}
	for _, c := range cts {
		byServer[c.ServerID] = append(byServer[c.ServerID], c)
	}
	out := make([]apitypes.Server, 0, len(rows))
	for _, sv := range rows {
		v, err := s.view(sv, pickContainer(byServer[sv.ID]), derr == nil)
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

// Get returns one server with runtime state.
func (s *Service) Get(ctx context.Context, id int64) (apitypes.Server, error) {
	sv, err := s.load(ctx, id)
	if err != nil {
		return apitypes.Server{}, err
	}
	cts, derr := s.docker.ServerContainers(ctx, id)
	if derr != nil {
		s.dockerFailed("get server", derr)
	}
	return s.view(sv, pickContainer(cts), derr == nil)
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
	return img.ID, nil
}

func validateEnv(field string, env map[string]string) error {
	if err := docker.ValidateEnv(env); err != nil {
		var ve *docker.ValidationError
		if errors.As(err, &ve) {
			return &FieldError{Field: field, Msg: ve.Msg}
		}
		return err
	}
	return nil
}

func (s *Service) checkPort(field string, p int) error {
	if !s.cfg.Ports.Allowed(p) {
		return fieldErr(field, "port must be inside the configured range %s (browser-unsafe ports excluded)", s.cfg.Ports)
	}
	return nil
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
// view's RestartRequired tells whether the running container is stale.
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
	sv, err := s.load(ctx, id)
	if err != nil {
		return apitypes.Server{}, err
	}
	if req.Image != nil {
		sv.Image = imageID
	}
	if req.Port != nil {
		p := *req.Port
		sv.Port = &p
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
		if sv.Env, err = s.sealEnv(sv.ID, env); err != nil {
			return apitypes.Server{}, err
		}
	}
	updated, err := s.store.UpdateServer(ctx, sv)
	switch {
	case errors.Is(err, store.ErrConflict):
		return apitypes.Server{}, ErrPortInUse
	case errors.Is(err, store.ErrNotFound):
		return apitypes.Server{}, ErrNotFound
	case err != nil:
		return apitypes.Server{}, err
	}
	s.logger.Info("server updated", "server_id", id, "image_changed", req.Image != nil, "port_changed", req.Port != nil,
		"env_set", len(req.EnvSet), "env_unset", len(req.EnvUnset))
	cts, derr := s.docker.ServerContainers(ctx, id)
	if derr != nil {
		s.dockerFailed("update server", derr)
	}
	return s.view(updated, pickContainer(cts), derr == nil)
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
	if _, err := s.load(ctx, id); err != nil {
		return apitypes.ServerStats{}, err
	}
	st, err := s.docker.ServerStats(ctx, id)
	switch {
	case errors.Is(err, docker.ErrNotFound):
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
	if _, err := s.load(ctx, id); err != nil {
		return err
	}
	if _, err := s.docker.ServerContainer(ctx, id); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return ErrNoContainer
		}
		return err
	}
	return nil
}

// Logs writes the server's container output to w (see docker.ServerLogs).
func (s *Service) Logs(ctx context.Context, id int64, o docker.LogOptions, w io.Writer) error {
	err := s.docker.ServerLogs(ctx, id, o, w)
	if errors.Is(err, docker.ErrNotFound) {
		return ErrNoContainer
	}
	return err
}
