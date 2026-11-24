package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
)

// maxCertificateBody bounds PUT /certificates/game: both PEM fields at
// their limits, JSON-escaped.
const maxCertificateBody = 2 * (certs.MaxChainBytes + certs.MaxKeyBytes)

func (s *Server) needCerts() error {
	if s.certs == nil {
		return errUnavailable
	}
	return nil
}

func gameCertificate(host string, info certs.Info) apitypes.GameCertificate {
	return apitypes.GameCertificate{
		Mode: apitypes.CertificateMode(info.Mode), GameHost: host, Certificate: info.APIInfo(host),
	}
}

func (s *Server) handleGetGameCertificate(w http.ResponseWriter, r *http.Request) error {
	if err := s.needCerts(); err != nil {
		return err
	}
	host, err := s.effectiveGameHost(r.Context())
	if err != nil {
		return internal(err)
	}
	info, err := s.certs.Current(r.Context(), 0)
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, gameCertificate(host, info))
	return nil
}

func (s *Server) handlePutGameCertificate(w http.ResponseWriter, r *http.Request) error {
	if err := s.needCerts(); err != nil {
		return err
	}
	var req apitypes.UpdateGameCertificateRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	host, err := s.effectiveGameHost(r.Context())
	if err != nil {
		return internal(err)
	}
	var info certs.Info
	switch req.Mode {
	case apitypes.CertificateModeSelfSigned:
		if req.CertificatePEM != "" || req.PrivateKeyPEM != "" {
			return invalidField("certificate_pem", "certificate_pem and private_key_pem are only accepted with mode custom")
		}
		info, err = s.certs.UseSelfSigned(r.Context(), func(ctx context.Context) (string, error) {
			current, err := s.effectiveGameHost(ctx)
			host = current
			return current, err
		})
	case apitypes.CertificateModeCustom:
		info, err = s.certs.UseCustom(r.Context(), []byte(req.CertificatePEM), []byte(req.PrivateKeyPEM))
	default:
		return invalidField("mode", `mode must be "self_signed" or "custom"`)
	}
	var ve *certs.ValidationError
	switch {
	case errors.As(err, &ve):
		return invalidField(ve.Field, ve.Msg)
	case err != nil:
		return internal(err)
	}
	s.logger.Info("game certificate replaced", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID,
		"mode", info.Mode, "fingerprint", info.Fingerprint, "not_after", info.NotAfter)
	writeJSON(w, http.StatusOK, gameCertificate(host, info))
	return nil
}
