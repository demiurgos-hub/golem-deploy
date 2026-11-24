package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"time"
)

// Limits on uploaded certificates.
const (
	MaxChainBytes = 64 << 10
	MaxKeyBytes   = 16 << 10
	MaxChainCerts = 8
	MinRSABits    = 2048
)

// Request fields named by ValidationError.
const (
	FieldCertificate = "certificate_pem"
	FieldPrivateKey  = "private_key_pem"
)

// ValidationError is a rejected upload; Msg is safe to show to users (it
// never contains key material).
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

func invalid(field, format string, a ...any) error {
	return &ValidationError{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// ValidateCustom checks an uploaded chain (PEM CERTIFICATE blocks, leaf
// first) and private key (PKCS#8, PKCS#1 or SEC 1 PEM) and returns them
// normalised (re-encoded chain, PKCS#8 key). The key must be RSA of at
// least MinRSABits or ECDSA on P-256, P-384 or P-521, match the leaf, and
// the leaf must be valid at now and usable for TLS servers.
func ValidateCustom(chainPEM, keyPEM []byte, now time.Time) (chainOut, keyOut []byte, err error) {
	if len(chainPEM) == 0 {
		return nil, nil, invalid(FieldCertificate, "a PEM certificate chain is required")
	}
	if len(chainPEM) > MaxChainBytes {
		return nil, nil, invalid(FieldCertificate, "the certificate chain is larger than %d KiB", MaxChainBytes>>10)
	}
	if len(keyPEM) == 0 {
		return nil, nil, invalid(FieldPrivateKey, "a PEM private key is required")
	}
	if len(keyPEM) > MaxKeyBytes {
		return nil, nil, invalid(FieldPrivateKey, "the private key is larger than %d KiB", MaxKeyBytes>>10)
	}
	var chain []*x509.Certificate
	rest := chainPEM
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, nil, invalid(FieldCertificate, "the chain may only contain CERTIFICATE blocks (found %q)", clean(b.Type, 40))
		}
		if len(chain) == MaxChainCerts {
			return nil, nil, invalid(FieldCertificate, "the chain has more than %d certificates", MaxChainCerts)
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, nil, invalid(FieldCertificate, "certificate %d of the chain cannot be parsed", len(chain)+1)
		}
		chain = append(chain, c)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, invalid(FieldCertificate, "the chain contains data that is not a PEM block")
	}
	if len(chain) == 0 {
		return nil, nil, invalid(FieldCertificate, "no PEM CERTIFICATE block found")
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, nil, err
	}
	leaf := chain[0]
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if k.N.BitLen() < MinRSABits {
			return nil, nil, invalid(FieldPrivateKey, "RSA keys must have at least %d bits", MinRSABits)
		}
	case *ecdsa.PrivateKey:
		if c := k.Curve; c != elliptic.P256() && c != elliptic.P384() && c != elliptic.P521() {
			return nil, nil, invalid(FieldPrivateKey, "ECDSA keys must use P-256, P-384 or P-521")
		}
	default:
		return nil, nil, invalid(FieldPrivateKey, "the key must be an RSA or ECDSA key")
	}
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.(crypto.Signer).Public()) {
		return nil, nil, invalid(FieldPrivateKey, "the private key does not belong to the first certificate of the chain")
	}
	switch {
	case !now.Before(leaf.NotAfter):
		return nil, nil, invalid(FieldCertificate, "the certificate expired on %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	case now.Before(leaf.NotBefore):
		return nil, nil, invalid(FieldCertificate, "the certificate is not valid before %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) &&
		!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return nil, nil, invalid(FieldCertificate, "the certificate is not valid for TLS servers (extended key usage)")
	}
	for i := 1; i < len(chain); i++ {
		child, parent := chain[i-1], chain[i]
		if !bytes.Equal(child.RawIssuer, parent.RawSubject) || child.CheckSignatureFrom(parent) != nil {
			return nil, nil, invalid(FieldCertificate, "certificate %d in the chain must be signed by certificate %d", i, i+1)
		}
	}
	var out bytes.Buffer
	for _, c := range chain {
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, invalid(FieldPrivateKey, "the key cannot be stored")
	}
	keyOut = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := tls.X509KeyPair(out.Bytes(), keyOut); err != nil {
		return nil, nil, invalid(FieldPrivateKey, "the certificate and key do not form a valid TLS pair")
	}
	return out.Bytes(), keyOut, nil
}

// parseKey decodes exactly one private key block (an "EC PARAMETERS"
// block before it, as openssl writes, is skipped).
func parseKey(keyPEM []byte) (any, error) {
	rest := keyPEM
	var block *pem.Block
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type == "EC PARAMETERS" {
			continue
		}
		if block != nil {
			return nil, invalid(FieldPrivateKey, "exactly one private key is expected")
		}
		block = b
	}
	if len(bytes.TrimSpace(rest)) != 0 || block == nil {
		return nil, invalid(FieldPrivateKey, "no PEM private key found")
	}
	if len(block.Headers) > 0 || block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, invalid(FieldPrivateKey, "encrypted private keys are not supported; upload the decrypted key")
	}
	var (
		key any
		err error
	)
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, invalid(FieldPrivateKey, "unsupported key block %q", clean(block.Type, 40))
	}
	if err != nil {
		return nil, invalid(FieldPrivateKey, "the private key cannot be parsed")
	}
	return key, nil
}
