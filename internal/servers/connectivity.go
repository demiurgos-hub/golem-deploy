package servers

import (
	"context"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

func (s *Service) serverCertificate(ctx context.Context, id int64, p *runtimeParams) (*apitypes.CertificateInfo, error) {
	if s.cfg.Certs == nil {
		return nil, nil
	}
	info, err := s.cfg.Certs.Current(ctx, id)
	if err != nil {
		return nil, err
	}
	p.certMode, p.certDir = info.Mode, s.cfg.Certs.ServerDir(id)
	return info.APIInfo(p.gameHost), nil
}

// Connectivity describes the public game endpoints a browser can probe.
func (s *Service) Connectivity(ctx context.Context, id int64) (apitypes.ServerConnectivity, error) {
	sv, ct, dockerOK, err := s.inspect(ctx, id)
	if err != nil {
		return apitypes.ServerConnectivity{}, err
	}
	if !dockerOK {
		return apitypes.ServerConnectivity{}, docker.ErrUnavailable
	}
	p, err := s.params(ctx)
	if err != nil {
		return apitypes.ServerConnectivity{}, err
	}
	b, err := s.builds().get(ctx, sv)
	if err != nil {
		return apitypes.ServerConnectivity{}, err
	}
	cert, err := s.serverCertificate(ctx, id, &p)
	if err != nil {
		return apitypes.ServerConnectivity{}, err
	}
	port := 0
	if sv.Port != nil {
		port = *sv.Port
	}
	wt, ws := transportURLs(b, p.gameHost, port)
	running := ct != nil && ct.Running && ct.State == "running"
	return apitypes.ServerConnectivity{
		Running:           running,
		PageURL:           publicURL(p.gameHost, port),
		WebTransportURL:   wt,
		WebSocketURL:      ws,
		Pinned:            p.certMode == certs.ModeSelfSigned,
		Certificate:       cert,
		TestOrigins:       p.testOrigins,
		TestOriginsActive: running && !s.restartRequired(sv, b, p, ct),
	}, nil
}
