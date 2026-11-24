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
// Server.Certificate. In mode "custom" all servers share the uploaded
// certificate, described by Certificate (null while none is installed).
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
// servers. Running servers pick up the new certificate within a minute
// without a restart; switching modes marks them restart_required.
type UpdateGameCertificateRequest struct {
	Mode           CertificateMode `json:"mode"`
	CertificatePEM string          `json:"certificate_pem,omitempty"`
	PrivateKeyPEM  string          `json:"private_key_pem,omitempty"`
}
