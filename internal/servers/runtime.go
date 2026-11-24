package servers

import (
	"context"
	"fmt"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
)

// Runtime reads what the running game reports through its admin endpoint
// (GET /ready, /stats and /info). It is ErrNoContainer or ErrNotRunning
// when there is no running container, and ErrAdminUnavailable when the
// endpoint does not answer within the contract. Every value is bounded and
// sanitised by gameadmin.
func (s *Service) Runtime(ctx context.Context, id int64, now time.Time) (apitypes.ServerRuntime, error) {
	sv, err := s.load(ctx, id)
	if err != nil {
		return apitypes.ServerRuntime{}, err
	}
	ct, err := s.container(ctx, sv)
	if err != nil {
		return apitypes.ServerRuntime{}, err
	}
	if ct == nil {
		return apitypes.ServerRuntime{}, ErrNoContainer
	}
	if !adminRunning(*ct) {
		return apitypes.ServerRuntime{}, ErrNotRunning
	}
	port := ct.LoopbackPort(AdminContainerPort)
	if port == 0 {
		return apitypes.ServerRuntime{}, fmt.Errorf("%w: the container publishes no admin port", ErrAdminUnavailable)
	}
	token, err := s.openAdminToken(sv)
	if err != nil {
		return apitypes.ServerRuntime{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	c, err := gameadmin.NewSession(port, token)
	if err != nil {
		return apitypes.ServerRuntime{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	defer c.Close()
	r, err := c.Ready(ctx)
	if err != nil {
		return apitypes.ServerRuntime{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	st, err := c.Stats(ctx)
	if err != nil {
		return apitypes.ServerRuntime{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		return apitypes.ServerRuntime{}, fmt.Errorf("%w: %w", ErrAdminUnavailable, err)
	}
	return apitypes.ServerRuntime{
		Ready:          r.Ready,
		NotReadyReason: r.Reason,
		Sessions:       st.Sessions,
		Entities:       st.Entities,
		UptimeSeconds:  st.UptimeSeconds,
		Tick: apitypes.ServerTickStats{
			Tick: st.Tick, Rate: st.TickRate, BudgetMs: st.BudgetMs, LastMs: st.LastMs, MaxMs: st.MaxMs, Overruns: st.Overruns,
		},
		GameName:                   info.Name,
		EngineVersion:              info.EngineVersion,
		Transport:                  info.Transport,
		SchemaFingerprint:          info.SchemaFingerprint,
		CertificateHashesPublished: info.CertificateHashesPublished,
		SampledAt:                  now.UTC(),
	}, nil
}

// Docker may set Running for paused or restarting containers as well.
func adminRunning(c docker.Container) bool { return c.Running && c.State == "running" }
