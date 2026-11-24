package certs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
	"github.com/mholt/acmez/v3"
)

type certificatePair struct {
	chain, key []byte
	tls        *tls.Certificate
	info       Info
}

func parsePair(chain, key []byte) (*certificatePair, error) {
	pair, err := tls.X509KeyPair(chain, key)
	if err != nil {
		return nil, ErrNoCertificate
	}
	if pair.Leaf == nil {
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, ErrNoCertificate
		}
	}
	return &certificatePair{chain: chain, key: key, tls: &pair, info: describe(pair.Leaf, len(pair.Certificate))}, nil
}

func validateACMEPair(chain, key []byte, host string, now time.Time) (*certificatePair, error) {
	rejected := &acmeFailure{message: "The CA did not provide a usable certificate for " + host + "."}
	chain, key, err := ValidateCustom(chain, key, now)
	if err != nil {
		return nil, rejected
	}
	pair, err := parsePair(chain, key)
	if err != nil {
		return nil, rejected
	}
	leaf := pair.info.leaf
	if len(leaf.EmailAddresses)+len(leaf.URIs) != 0 {
		return nil, rejected
	}
	if ip := net.ParseIP(host); ip != nil {
		if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(ip) || len(leaf.DNSNames) != 0 {
			return nil, rejected
		}
	} else if len(leaf.DNSNames) != 1 || !strings.EqualFold(leaf.DNSNames[0], host) || len(leaf.IPAddresses) != 0 {
		return nil, rejected
	}
	return pair, nil
}

type panelState struct {
	mode     apitypes.PanelCertificateMode
	hosts    []string
	fallback *certificatePair
	issued   map[string]*certificatePair
}

func (a *Automation) configurable() bool {
	return a.PanelMode == apitypes.PanelCertificateModeSelfSigned
}

func (a *Automation) IsPanelHost(host string) bool {
	return slices.Contains(a.panel.Load().hosts, host)
}

func (a *Automation) panelHosts(game string) []string {
	hosts := []string{}
	for _, h := range a.PublicHosts {
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 && game != "" {
		hosts = append(hosts, game)
	}
	return hosts
}

func (a *Automation) Initialize(ctx context.Context) error {
	host, err := a.GameHost(ctx)
	if err != nil {
		return err
	}
	p := &panelState{mode: a.PanelMode, hosts: a.panelHosts(host), issued: make(map[string]*certificatePair)}
	switch {
	case a.configurable():
		if err := os.MkdirAll(a.PanelDir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(a.PanelDir, 0o700); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(a.PanelDir)); err != nil {
			return err
		}
		entries, err := os.ReadDir(a.PanelDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".tmp-") {
				if err := os.Remove(filepath.Join(a.PanelDir, entry.Name())); err != nil {
					return err
				}
			}
		}
		if err := a.fallback(p, host); err != nil {
			return err
		}
		a.logger.Info("panel self-signed TLS fingerprint", "sha256", p.fallback.info.Fingerprint)
		mode, _, err := a.Game.settings.GetSetting(ctx, store.SettingPanelCertificateMode)
		if err != nil {
			return err
		}
		if apitypes.PanelCertificateMode(mode) == apitypes.PanelCertificateModeACME {
			p.mode = apitypes.PanelCertificateModeACME
			for _, name := range p.hosts {
				pair, err := a.readIssuedPanelPair(name)
				switch {
				case err == nil:
					p.issued[name] = pair
				case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrNoCertificate):
					// Startup reconciliation can recover this from ACME storage.
				default:
					return err
				}
			}
		}
	case a.FileCertificate != nil:
		pair := a.FileCertificate
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return errors.New("cannot parse the panel TLS leaf certificate")
		}
		p.fallback = &certificatePair{tls: pair, info: describe(leaf, len(pair.Certificate))}
	}
	a.panel.Store(p)
	// Reconcile locally before the daemon starts any game recovery jobs.
	if err := a.check(ctx, false); err != nil {
		a.logger.Warn("could not reconcile stored ACME certificates", "err", operationError(err))
	}
	return nil
}

func panelPairName(host string) string {
	return fmt.Sprintf("acme-%x.pem", sha256.Sum256([]byte(host)))
}

func (a *Automation) readPanelPair(name string) (*certificatePair, error) {
	data, err := os.ReadFile(filepath.Join(a.PanelDir, name))
	if err != nil {
		return nil, err
	}
	return parsePair(data, data)
}

func (a *Automation) readIssuedPanelPair(host string) (*certificatePair, error) {
	data, err := os.ReadFile(filepath.Join(a.PanelDir, panelPairName(host)))
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndex(data, []byte("-----BEGIN PRIVATE KEY-----"))
	if i < 0 {
		return nil, ErrNoCertificate
	}
	pair, err := validateACMEPair(data[:i], data[i:], host, a.now())
	if err != nil {
		return nil, ErrNoCertificate
	}
	return pair, nil
}

func (a *Automation) writePanelPair(name string, pair *certificatePair) error {
	// One atomic file lets recovery load a complete TLS pair after a crash.
	data := slices.Concat(pair.chain, []byte("\n"), pair.key)
	if err := a.Game.writeFile(a.PanelDir, name, data, 0o600); err != nil {
		return err
	}
	return syncDir(a.PanelDir)
}

func (a *Automation) fallback(p *panelState, game string) error {
	names := append(slices.Clone(p.hosts), game)
	if p.fallback != nil && a.Game.selfSignedOK(p.fallback.info, names...) {
		return nil
	}
	pair, err := a.readPanelPair("fallback.pem")
	if err == nil && a.Game.selfSignedOK(pair.info, names...) {
		p.fallback = pair
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrNoCertificate) {
		return err
	}
	chain, key, _, err := a.Game.generate(game, p.hosts...)
	if err != nil {
		return err
	}
	pair, err = parsePair(chain, key)
	if err != nil {
		return err
	}
	if err := a.writePanelPair("fallback.pem", pair); err != nil {
		return err
	}
	p.fallback = pair
	return nil
}

func (p *panelState) selectPair(now time.Time, name, localIP string) *certificatePair {
	if p.mode == apitypes.PanelCertificateModeACME {
		if name != "" {
			if pair := p.usable(now, name); pair != nil {
				return pair
			}
		} else {
			if pair := p.usable(now, localIP); pair != nil {
				return pair
			}
			var soleIP *certificatePair
			for _, host := range p.hosts {
				if _, err := netip.ParseAddr(host); err == nil {
					pair := p.usable(now, host)
					if pair == nil {
						continue
					}
					if soleIP != nil {
						soleIP = nil
						break
					}
					soleIP = pair
				}
			}
			if soleIP != nil {
				return soleIP
			}
			if len(p.hosts) != 0 {
				if pair := p.usable(now, p.hosts[0]); pair != nil {
					return pair
				}
			}
		}
	}
	return p.fallback
}

func (p *panelState) usable(now time.Time, name string) *certificatePair {
	pair := p.issued[name]
	if pair != nil && !now.Before(pair.info.NotBefore) && now.Before(pair.info.NotAfter) && pair.info.Covers(name) {
		return pair
	}
	return nil
}

func (a *Automation) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if slices.Equal(hello.SupportedProtos, []string{acmez.ACMETLS1Protocol}) {
		if a.TLSALPN01 {
			if cert, err := a.Client.TLSChallenge(hello); cert != nil || err != nil {
				return cert, err
			}
		}
		return nil, errors.New("no active TLS-ALPN-01 challenge for this host")
	}
	var localIP string
	if hello.Conn != nil {
		localIP = certificateHost(hello.Conn.LocalAddr().String())
	}
	if pair := a.panel.Load().selectPair(a.now(), certificateHost(hello.ServerName), localIP); pair != nil {
		return pair.tls, nil
	}
	return nil, errors.New("no panel TLS certificate is available")
}

func (a *Automation) Panel(ctx context.Context) (apitypes.PanelCertificate, error) {
	host, err := a.GameHost(ctx)
	if err != nil {
		return apitypes.PanelCertificate{}, err
	}
	p := a.panel.Load()
	hosts := a.panelHosts(host)
	out := apitypes.PanelCertificate{Mode: p.mode, Configurable: a.configurable(), Hosts: hosts}
	now := a.now()
	first := ""
	if len(hosts) > 0 {
		first = hosts[0]
	}
	if pair := p.selectPair(now, first, ""); pair != nil {
		out.Certificate = pair.info.APIInfo(host)
		out.CoversHosts = len(hosts) > 0
		for _, name := range hosts {
			pair := p.selectPair(now, name, "")
			out.CoversHosts = out.CoversHosts && pair != nil && pair.info.Covers(name)
		}
	}
	return out, nil
}

func (a *Automation) UsePanelSelfSigned(ctx context.Context) (apitypes.PanelCertificate, error) {
	if !a.configurable() {
		return apitypes.PanelCertificate{}, ErrPanelFixed
	}
	err := a.Change(func() error {
		a.hostMu.Lock()
		defer a.hostMu.Unlock()
		host, err := a.GameHost(ctx)
		if err != nil {
			return err
		}
		p := *a.panel.Load()
		p.hosts, p.mode, p.issued = a.panelHosts(host), apitypes.PanelCertificateModeSelfSigned, nil
		if err := a.fallback(&p, host); err != nil {
			return err
		}
		return a.commitPanel(ctx, &p)
	})
	if err != nil {
		return apitypes.PanelCertificate{}, err
	}
	return a.Panel(ctx)
}

func (a *Automation) commitPanel(ctx context.Context, p *panelState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	mode := string(p.mode)
	err := a.Game.settings.SetSetting(ctx, store.SettingPanelCertificateMode, mode)
	if err != nil {
		// Recover the persisted decision if the commit had an ambiguous result.
		saved, _, readErr := a.Game.settings.GetSetting(ctx, store.SettingPanelCertificateMode)
		if readErr != nil || saved != mode {
			return errors.Join(err, readErr)
		}
	}
	a.panel.Store(p)
	a.wakeup()
	return nil
}

func (a *Automation) setPanelPair(host string, pair *certificatePair) error {
	p := *a.panel.Load()
	if previous := p.issued[host]; previous != nil && previous.info.Fingerprint == pair.info.Fingerprint {
		return nil
	}
	if err := a.writePanelPair(panelPairName(host), pair); err != nil {
		return err
	}
	p.issued = maps.Clone(p.issued)
	if p.issued == nil {
		p.issued = make(map[string]*certificatePair)
	}
	p.issued[host] = pair
	a.panel.Store(&p)
	return nil
}
