package servers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
)

// Errors of Profile besides ErrNotFound, ErrNoContainer, ErrNotRunning and
// ErrAdminUnavailable.
var (
	// ErrProfilingOff: the server's Profiling switch is off, its running
	// container was started without it (a restart applies the switch), or
	// the game's engine does not serve profiles.
	ErrProfilingOff = errors.New("servers: profiling is not available for this server")
	// ErrProfileInProgress: a profile of the server is already being
	// captured, through this daemon or otherwise.
	ErrProfileInProgress = errors.New("servers: a profile is already being captured")
)

// Profile is a captured profile file and the name to save it under.
type Profile struct {
	Data     []byte
	Filename string
}

// Capture lengths of the timed profile kinds in whole seconds: the default
// when none is asked for and the largest accepted (the smallest is 1). They
// match golem-engine's admin endpoint (golem/host admin_profile.go) and the
// ProfileKind documentation.
const (
	ProfileCPUDefaultSeconds   = 30
	ProfileCPUMaxSeconds       = 60
	ProfileTraceDefaultSeconds = 5
	ProfileTraceMaxSeconds     = 15
)

// profileLimits returns a kind's default and maximum capture length in
// seconds (both 0 for kinds without a duration) and whether kind is known.
func profileLimits(kind apitypes.ProfileKind) (def, max int, ok bool) {
	switch kind {
	case apitypes.ProfileKindCPU:
		return ProfileCPUDefaultSeconds, ProfileCPUMaxSeconds, true
	case apitypes.ProfileKindTrace:
		return ProfileTraceDefaultSeconds, ProfileTraceMaxSeconds, true
	case apitypes.ProfileKindHeap, apitypes.ProfileKindAllocs, apitypes.ProfileKindGoroutine:
		return 0, 0, true
	}
	return 0, 0, false
}

// ProfileSeconds validates a profile request and returns the capture length
// in seconds (0 for kinds without one). seconds is nil when absent.
func ProfileSeconds(kind apitypes.ProfileKind, seconds *int) (int, error) {
	def, max, ok := profileLimits(kind)
	switch {
	case !ok:
		return 0, fieldErr("kind", "kind must be cpu, trace, heap, allocs or goroutine")
	case max == 0 && seconds != nil:
		return 0, fieldErr("seconds", "seconds is only accepted for cpu and trace")
	case max == 0:
		return 0, nil
	case seconds == nil:
		return def, nil
	case *seconds < 1 || *seconds > max:
		return 0, fieldErr("seconds", "seconds must be from 1 to %d for %s", max, kind)
	}
	return *seconds, nil
}

// Profile captures a profile of server id's running game through its admin
// endpoint and returns the file. A timed capture (cpu, trace) takes its
// whole length before it returns. One capture per server runs at a time;
// the game also refuses a second timed capture of its own.
func (s *Service) Profile(ctx context.Context, id int64, kind apitypes.ProfileKind, seconds *int, now time.Time) (Profile, error) {
	n, err := ProfileSeconds(kind, seconds)
	if err != nil {
		return Profile{}, err
	}
	sv, err := s.load(ctx, id)
	if err != nil {
		return Profile{}, err
	}
	if !sv.Profiling {
		return Profile{}, ErrProfilingOff
	}
	ct, err := s.container(ctx, sv)
	if err != nil {
		return Profile{}, err
	}
	if ct == nil {
		return Profile{}, ErrNoContainer
	}
	if !adminRunning(*ct) {
		return Profile{}, ErrNotRunning
	}
	if ct.Labels[docker.LabelProfiling] != docker.ManagedValue {
		return Profile{}, ErrProfilingOff
	}
	if _, busy := s.profiling.LoadOrStore(id, struct{}{}); busy {
		return Profile{}, ErrProfileInProgress
	}
	defer s.profiling.Delete(id)
	port := ct.LoopbackPort(AdminContainerPort)
	if port == 0 {
		return Profile{}, fmt.Errorf("%w: the container publishes no admin port", ErrAdminUnavailable)
	}
	token, err := s.openAdminToken(sv)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	c, err := gameadmin.New(port, token)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	data, err := c.Profile(ctx, string(kind), n)
	switch {
	case errors.Is(err, gameadmin.ErrProfilingUnsupported):
		return Profile{}, ErrProfilingOff
	case errors.Is(err, gameadmin.ErrProfileInProgress):
		return Profile{}, ErrProfileInProgress
	case err != nil && ctx.Err() != nil:
		return Profile{}, ctx.Err()
	case err != nil:
		return Profile{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	ext := ".pb.gz"
	if kind == apitypes.ProfileKindTrace {
		ext = ".trace"
	}
	name := strings.Join([]string{sv.Name, string(kind), now.UTC().Format("20060102T150405Z")}, "-") + ext
	return Profile{Data: data, Filename: name}, nil
}
