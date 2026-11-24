package apitypes

import "time"

// CertificateMode is how the game certificate is obtained.
type CertificateMode string

// Certificate modes.
const (
	// CertificateModeSelfSigned: the daemon generates and rotates a
	// short-lived self-signed certificate (the default).
	CertificateModeSelfSigned CertificateMode = "self_signed"
	// CertificateModeCustom: an admin uploaded a certificate chain and key.
	CertificateModeCustom CertificateMode = "custom"
	// CertificateModeACME: the daemon obtains and renews a publicly trusted
	// certificate for the game host from an ACME certificate authority
	// (Let's Encrypt by default). A DNS name gets an ordinary certificate;
	// an IP address gets a short-lived one (about 6 days).
	CertificateModeACME CertificateMode = "acme"
)

// CertificateInfo describes a TLS certificate a game server serves for
// WebTransport and HTTPS. The private key is never returned.
type CertificateInfo struct {
	// Subject and Issuer are the leaf certificate's distinguished names.
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
	// DNSNames and IPAddresses are the leaf's subject alternative names.
	DNSNames    []string  `json:"dns_names"`
	IPAddresses []string  `json:"ip_addresses"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	// FingerprintSHA256 is the lower-case hex SHA-256 of the leaf (the value
	// browsers pin through WebTransport serverCertificateHashes).
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	SelfSigned        bool   `json:"self_signed"`
	// ChainLength is the number of certificates in the chain.
	ChainLength int `json:"chain_length"`
	// CoversGameHost reports whether the certificate is valid for the
	// effective game host.
	CoversGameHost bool `json:"covers_game_host"`
}

// GameCertificate is the body of GET and PUT /api/v1/certificates/game
// (admin): how game servers get their TLS certificate.
//
// In mode "self_signed" every server has its own short-lived certificate and
// private key, generated and rotated by the daemon, so no game holds another
// game's key; Certificate is null and each server's certificate is
// Server.Certificate. In modes "custom" and "acme" all servers share one
// certificate and private key (uploaded, or obtained by the daemon),
// described by Certificate (null while none is installed).
type GameCertificate struct {
	Mode CertificateMode `json:"mode"`
	// GameHost is the effective game host.
	GameHost    string           `json:"game_host"`
	Certificate *CertificateInfo `json:"certificate" tstype:"CertificateInfo | null,required"`
}

// UpdateGameCertificateRequest is the body of PUT /api/v1/certificates/game.
// Mode "self_signed" (no PEM fields) switches to per-server self-signed
// certificates and regenerates them; mode "custom" installs CertificatePEM (the chain, leaf first,
// at most 8 certificates and 64 KiB) and PrivateKeyPEM (unencrypted PKCS#8,
// PKCS#1 or SEC 1; RSA of at least 2048 bits or ECDSA P-256/P-384/P-521; at
// most 16 KiB), which must match, be currently valid and be usable for TLS
// servers. Mode "acme" (no PEM fields) starts a certificate.issue job that
// obtains a certificate for the game host; the mode changes only when that
// job succeeds, so servers keep their current certificate until then and
// when it fails. Running servers pick up the new certificate within a
// minute without a restart; a switch between self_signed and another mode
// marks them restart_required.
type UpdateGameCertificateRequest struct {
	Mode           CertificateMode `json:"mode"`
	CertificatePEM string          `json:"certificate_pem,omitempty"`
	PrivateKeyPEM  string          `json:"private_key_pem,omitempty"`
}

// UpdateGameCertificateResponse is returned by PUT /api/v1/certificates/game:
// 200 with Job null when the change is complete (self_signed, custom), 202
// with the certificate.issue job when it continues in the background
// (acme). Certificate is the state at the time of the response.
type UpdateGameCertificateResponse struct {
	Certificate GameCertificate `json:"certificate"`
	Job         *Job            `json:"job" tstype:"Job | null,required"`
}

// PanelCertificateMode is where the panel's own TLS certificate comes from.
type PanelCertificateMode string

// Panel certificate modes.
const (
	// PanelCertificateModeNone: the panel serves plain HTTP (a loopback
	// listen address, or --insecure-http behind a proxy that terminates TLS).
	PanelCertificateModeNone PanelCertificateMode = "none"
	// PanelCertificateModeFiles: the operator's --tls-cert and --tls-key.
	PanelCertificateModeFiles PanelCertificateMode = "files"
	// PanelCertificateModeSelfSigned: a certificate the daemon generates;
	// browsers show a warning. This is the default when the panel listens
	// on a non-loopback address without --tls-cert.
	PanelCertificateModeSelfSigned PanelCertificateMode = "self_signed"
	// PanelCertificateModeACME: the daemon obtains and renews a publicly
	// trusted certificate for every panel host.
	PanelCertificateModeACME PanelCertificateMode = "acme"
)

// PanelCertificate is the body of GET /api/v1/certificates/panel (admin).
type PanelCertificate struct {
	Mode PanelCertificateMode `json:"mode"`
	// Configurable is false in modes "none" and "files", which are decided
	// by the serve flags; PUT then answers 409 conflict.
	Configurable bool `json:"configurable"`
	// Hosts are the names the panel's certificate is issued for: the
	// --public-host values, or the effective game host when there are none.
	Hosts []string `json:"hosts"`
	// Certificate is what the panel serves now for the first of Hosts (null
	// in mode "none"). Its CoversGameHost is about the game host; CoversHosts
	// reports whether it is valid for every one of Hosts.
	Certificate *CertificateInfo `json:"certificate" tstype:"CertificateInfo | null,required"`
	CoversHosts bool             `json:"covers_hosts"`
}

// UpdatePanelCertificateRequest is the body of PUT /api/v1/certificates/panel.
// Mode must be "self_signed" or "acme". "acme" starts a certificate.issue
// job; the mode changes only when that job succeeds.
type UpdatePanelCertificateRequest struct {
	Mode PanelCertificateMode `json:"mode"`
}

// UpdatePanelCertificateResponse is returned by PUT
// /api/v1/certificates/panel: 200 with Job null (self_signed), 202 with the
// certificate.issue job (acme).
type UpdatePanelCertificateResponse struct {
	Certificate PanelCertificate `json:"certificate"`
	Job         *Job             `json:"job" tstype:"Job | null,required"`
}

// ACMECertificateStatus is the state of one certificate the daemon manages
// through ACME.
type ACMECertificateStatus string

// ACME certificate states.
const (
	// ACMECertificateStatusPending: not obtained yet, no attempt has failed.
	ACMECertificateStatusPending ACMECertificateStatus = "pending"
	// ACMECertificateStatusIssued: a certificate is installed and the last
	// attempt (if any) succeeded.
	ACMECertificateStatusIssued ACMECertificateStatus = "issued"
	// ACMECertificateStatusError: the last attempt to obtain or renew
	// failed; LastError says why. An earlier certificate stays in use until
	// it expires and the daemon keeps retrying.
	ACMECertificateStatusError ACMECertificateStatus = "error"
)

// ACMECertificateUse says what a managed certificate is used for.
type ACMECertificateUse string

// ACME certificate uses.
const (
	ACMECertificateUseGame  ACMECertificateUse = "game"
	ACMECertificateUsePanel ACMECertificateUse = "panel"
)

// ACMECertificate is one certificate the daemon manages through ACME: one
// per identifier. When the game host is also a panel host, both use the
// same certificate and key.
type ACMECertificate struct {
	// Identifier is the DNS name or IP address the certificate is for.
	Identifier string `json:"identifier"`
	// UsedBy lists what uses it (never empty).
	UsedBy []ACMECertificateUse  `json:"used_by"`
	Status ACMECertificateStatus `json:"status"`
	// Certificate is the installed certificate; null until one is obtained.
	Certificate *CertificateInfo `json:"certificate" tstype:"CertificateInfo | null,required"`
	// LastError is a short, user-safe description of the last failed
	// attempt ("" when the last attempt succeeded or none was made).
	LastError string `json:"last_error"`
	// LastAttemptAt is when the daemon last tried to obtain or renew it.
	LastAttemptAt *time.Time `json:"last_attempt_at" tstype:"string | null,required"`
}

// ACME is the body of GET and PUT /api/v1/certificates/acme (admin): the
// account settings shared by every automatic certificate, what the host can
// answer challenges with, and the managed certificates.
type ACME struct {
	// Email is the optional contact address given to the certificate
	// authority.
	Email string `json:"email"`
	// Staging uses the authority's staging environment: certificates are
	// not trusted by browsers and the generous staging limits apply. It has
	// no effect when --acme-directory is set.
	Staging bool `json:"staging"`
	// TermsAgreed records that an admin accepted the authority's subscriber
	// agreement; no certificate is requested without it.
	TermsAgreed bool `json:"terms_agreed"`
	// DirectoryURL is the ACME directory in use.
	DirectoryURL string `json:"directory_url"`
	// HTTP01Available: the daemon answers HTTP-01 challenges (it listens on
	// --http-listen, which must be reachable as port 80 of every identifier).
	// TLSALPN01Available: it answers TLS-ALPN-01 challenges (the panel
	// serves TLS on port 443). Automatic certificates need at least one.
	HTTP01Available    bool `json:"http01_available"`
	TLSALPN01Available bool `json:"tls_alpn01_available"`
	// Certificates are the managed certificates, sorted by identifier
	// (empty when neither the game nor the panel uses ACME).
	Certificates []ACMECertificate `json:"certificates"`
}

// UpdateACMERequest is the body of PUT /api/v1/certificates/acme. Absent
// fields are left unchanged. Email is "" or one address (at most 254
// bytes). Staging cannot change while a certificate is managed
// (409 conflict): switch the game and the panel away from "acme" first.
type UpdateACMERequest struct {
	Email       *string `json:"email,omitempty"`
	Staging     *bool   `json:"staging,omitempty"`
	TermsAgreed *bool   `json:"terms_agreed,omitempty"`
}
