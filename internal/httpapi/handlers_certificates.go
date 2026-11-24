package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
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
	if req.Mode != apitypes.CertificateModeCustom {
		if req.CertificatePEM != "" || req.PrivateKeyPEM != "" {
			return invalidField("certificate_pem", "certificate_pem and private_key_pem are only accepted with mode custom")
		}
	}
	if req.Mode == apitypes.CertificateModeACME {
		if s.automation == nil {
			return errUnavailable
		}
		info, err := s.certs.Current(r.Context(), 0)
		if err != nil {
			return internal(err)
		}
		job, err := s.startCertificateJob(r, certs.IssueGame)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, apitypes.UpdateGameCertificateResponse{
			Certificate: gameCertificate(host, info), Job: &job,
		})
		return nil
	}
	var info certs.Info
	change := func() error {
		switch req.Mode {
		case apitypes.CertificateModeSelfSigned:
			info, err = s.certs.UseSelfSigned(r.Context(), func(ctx context.Context) (string, error) {
				current, err := s.effectiveGameHost(ctx)
				host = current
				return current, err
			})
		case apitypes.CertificateModeCustom:
			info, err = s.certs.UseCustom(r.Context(), []byte(req.CertificatePEM), []byte(req.PrivateKeyPEM))
		default:
			return invalidField("mode", `mode must be "self_signed", "custom" or "acme"`)
		}
		return err
	}
	if s.automation != nil {
		err = s.automation.Change(change)
	} else {
		err = change()
	}
	if err != nil {
		return certificateError(err)
	}
	s.logger.Info("game certificate replaced", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID,
		"mode", info.Mode, "fingerprint", info.Fingerprint, "not_after", info.NotAfter)
	writeJSON(w, http.StatusOK, apitypes.UpdateGameCertificateResponse{Certificate: gameCertificate(host, info)})
	return nil
}

func certificateError(err error) error {
	var ve *certs.ValidationError
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &ve):
		return invalidField(ve.Field, ve.Msg)
	case errors.Is(err, jobs.ErrSubjectBusy):
		return conflict(apitypes.ErrorCodeJobInProgress, "another certificate operation is in progress")
	case errors.Is(err, certs.ErrPanelFixed), errors.Is(err, certs.ErrStagingManaged), errors.Is(err, certs.ErrNothingManaged):
		return conflict(apitypes.ErrorCodeConflict, err.Error())
	case errors.Is(err, jobs.ErrShuttingDown):
		return errUnavailable
	default:
		return internal(err)
	}
}

func (s *Server) startCertificateJob(r *http.Request, action certs.Action) (apitypes.Job, error) {
	job, err := s.automation.Start(r.Context(), action, identityFrom(r).Account.ID)
	if err != nil {
		return apitypes.Job{}, certificateError(err)
	}
	return toAPIJob(job), nil
}

func (s *Server) handleGetPanelCertificate(w http.ResponseWriter, r *http.Request) error {
	if s.automation == nil {
		return errUnavailable
	}
	panel, err := s.automation.Panel(r.Context())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, panel)
	return nil
}

func (s *Server) handlePutPanelCertificate(w http.ResponseWriter, r *http.Request) error {
	if s.automation == nil {
		return errUnavailable
	}
	var req apitypes.UpdatePanelCertificateRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	panel, err := s.automation.Panel(r.Context())
	if err != nil {
		return internal(err)
	}
	if !panel.Configurable {
		return certificateError(certs.ErrPanelFixed)
	}
	switch req.Mode {
	case apitypes.PanelCertificateModeSelfSigned:
		panel, err = s.automation.UsePanelSelfSigned(r.Context())
		if err != nil {
			return certificateError(err)
		}
		writeJSON(w, http.StatusOK, apitypes.UpdatePanelCertificateResponse{Certificate: panel})
	case apitypes.PanelCertificateModeACME:
		job, err := s.startCertificateJob(r, certs.IssuePanel)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, apitypes.UpdatePanelCertificateResponse{Certificate: panel, Job: &job})
	default:
		return invalidField("mode", `mode must be "self_signed" or "acme"`)
	}
	return nil
}

func (s *Server) handleGetACME(w http.ResponseWriter, r *http.Request) error {
	if s.automation == nil {
		return errUnavailable
	}
	info, err := s.automation.ACME(r.Context())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) handlePutACME(w http.ResponseWriter, r *http.Request) error {
	if s.automation == nil {
		return errUnavailable
	}
	var req apitypes.UpdateACMERequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	info, err := s.automation.UpdateSettings(r.Context(), req)
	if err != nil {
		return certificateError(err)
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) handleRenewACME(w http.ResponseWriter, r *http.Request) error {
	if s.automation == nil {
		return errUnavailable
	}
	job, err := s.startCertificateJob(r, certs.RenewAll)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, job)
	return nil
}
