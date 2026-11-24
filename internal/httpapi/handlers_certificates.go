package httpapi

import (
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

func (s *Server) gameCertificate(r *http.Request, info certs.Info) (apitypes.GameCertificate, error) {
	host, err := s.effectiveGameHost(r.Context())
	if err != nil {
		return apitypes.GameCertificate{}, internal(err)
	}
	out := apitypes.GameCertificate{Mode: apitypes.CertificateMode(info.Mode), GameHost: host}
	// Interim: the certificate is still shared in both modes. With per-server
	// self-signed certificates, Certificate is set only in custom mode.
	if info.Fingerprint != "" {
		out.Certificate = &apitypes.CertificateInfo{
			Subject:           info.Subject,
			Issuer:            info.Issuer,
			DNSNames:          append([]string{}, info.DNSNames...),
			IPAddresses:       append([]string{}, info.IPAddresses...),
			NotBefore:         info.NotBefore.UTC(),
			NotAfter:          info.NotAfter.UTC(),
			FingerprintSHA256: info.Fingerprint,
			SelfSigned:        info.SelfSigned,
			ChainLength:       info.ChainLength,
			CoversGameHost:    info.Covers(host),
		}
	}
	return out, nil
}

func (s *Server) handleGetGameCertificate(w http.ResponseWriter, r *http.Request) error {
	if err := s.needCerts(); err != nil {
		return err
	}
	host, err := s.effectiveGameHost(r.Context())
	if err != nil {
		return internal(err)
	}
	// Ensure (not just read): a self-signed certificate is created or
	// renewed here as it would be before a server start.
	info, err := s.certs.Ensure(r.Context(), host)
	switch {
	case errors.Is(err, certs.ErrNoCertificate):
		return conflict(apitypes.ErrorCodeConflict, "no valid custom game certificate is installed; upload one or switch to self_signed")
	case err != nil && !errors.Is(err, certs.ErrExpired):
		return internal(err)
	}
	out, err := s.gameCertificate(r, info)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
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
	var info certs.Info
	var err error
	switch req.Mode {
	case apitypes.CertificateModeSelfSigned:
		if req.CertificatePEM != "" || req.PrivateKeyPEM != "" {
			return invalidField("certificate_pem", "certificate_pem and private_key_pem are only accepted with mode custom")
		}
		host, herr := s.effectiveGameHost(r.Context())
		if herr != nil {
			return internal(herr)
		}
		info, err = s.certs.UseSelfSigned(r.Context(), host)
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
	out, err := s.gameCertificate(r, info)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
