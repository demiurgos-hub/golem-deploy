package certs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/caddyserver/certmagic"
	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

var (
	ErrPanelFixed     = errors.New("panel TLS is controlled by the serve flags")
	ErrStagingManaged = errors.New("switch both game and panel away from ACME before changing staging")
	ErrNothingManaged = errors.New("no certificates are managed by ACME")
	errHostsChanged   = errors.New("the certificate hosts changed during issuance; retry for the current hosts")
)

type accountSettings struct {
	Email       string
	TermsAgreed bool
	Staging     bool
}

func (s accountSettings) account(override string) ACMEAccount {
	if override == "" {
		override = certmagic.LetsEncryptProductionCA
		if s.Staging {
			override = certmagic.LetsEncryptStagingCA
		}
	}
	return ACMEAccount{Directory: override, Email: s.Email}
}

// AutomationOptions wires the ACME client and the panel's startup TLS mode.
type AutomationOptions struct {
	Game              *Manager
	GameHost          func(context.Context) (string, error)
	PublicHosts       []string
	PanelMode         apitypes.PanelCertificateMode
	PanelDir          string
	FileCertificate   *tls.Certificate
	Client            ACMEClient
	Directory         string
	HTTP01, TLSALPN01 bool
	Jobs              *jobs.Runner
}

// Automation serializes certificate operations independently of TLS handshakes
// and server starts. The game manager owns game file changes. Handshakes read
// an immutable panel snapshot and never read files or contact the CA.
type Automation struct {
	AutomationOptions
	logger *slog.Logger
	now    func() time.Time
	admit  sync.Mutex    // explicit job admission and synchronous mutations
	hostMu sync.Mutex    // game_host writes and publication for a given host
	op     chan struct{} // one issuer or mode/settings operation at a time
	wake   chan struct{}

	panel     atomic.Pointer[panelState]
	schedules map[string]renewalSchedule // owned by op
}

type renewalSchedule struct {
	fingerprint   string
	when, refresh time.Time
}

func NewAutomation(o AutomationOptions) *Automation {
	return &Automation{
		AutomationOptions: o, logger: o.Game.logger, now: o.Game.now,
		op: make(chan struct{}, 1), wake: make(chan struct{}, 1),
		schedules: make(map[string]renewalSchedule),
	}
}

func certificateSubject() jobs.Subject {
	return jobs.Subject{Type: string(apitypes.SubjectTypeCertificate), ID: "acme"}
}

// Change serializes a synchronous mutation with issuance, including jobs that
// are queued but have not started.
func (a *Automation) Change(fn func() error) error {
	a.admit.Lock()
	defer a.admit.Unlock()
	if a.Jobs != nil && a.Jobs.Busy(certificateSubject()) {
		return jobs.ErrSubjectBusy
	}
	select {
	case a.op <- struct{}{}:
		defer func() { <-a.op }()
		return fn()
	default:
		return jobs.ErrSubjectBusy
	}
}

// ChangeHost commits a host change without waiting for an in-flight order.
// Publication rechecks the effective hosts under the same lock.
func (a *Automation) ChangeHost(fn func() error) error {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	if err := fn(); err != nil {
		return err
	}
	a.wakeup()
	return nil
}

func (a *Automation) wakeup() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Automation) settings(ctx context.Context) (accountSettings, error) {
	var s accountSettings
	value, ok, err := a.Game.settings.GetSetting(ctx, store.SettingACME)
	if err == nil && ok {
		err = json.Unmarshal([]byte(value), &s)
	}
	return s, err
}

func validEmail(email string) bool {
	if email == "" {
		return true
	}
	if len(email) > 254 || strings.ContainsAny(email, "<>()") ||
		strings.IndexFunc(email, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return false
	}
	m, err := mail.ParseAddress(email)
	return err == nil && m.Name == "" && m.Address == email
}

func (a *Automation) UpdateSettings(ctx context.Context, req apitypes.UpdateACMERequest) (apitypes.ACME, error) {
	var out apitypes.ACME
	err := a.Change(func() error {
		s, err := a.settings(ctx)
		if err != nil {
			return err
		}
		if req.Email != nil {
			if !validEmail(*req.Email) {
				return invalid("email", "email must be empty or one plain address of at most 254 bytes")
			}
			s.Email = *req.Email
		}
		if req.Staging != nil && *req.Staging != s.Staging {
			mode, err := a.Game.Mode(ctx)
			if err != nil {
				return err
			}
			if mode == ModeACME || a.panel.Load().mode == apitypes.PanelCertificateModeACME {
				return ErrStagingManaged
			}
			s.Staging = *req.Staging
		}
		if req.TermsAgreed != nil {
			s.TermsAgreed = *req.TermsAgreed
		}
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if err := a.Game.settings.SetSetting(ctx, store.SettingACME, string(data)); err != nil {
			return err
		}
		out, err = a.ACME(ctx)
		a.wakeup()
		return err
	})
	return out, err
}

type certificateState struct {
	gameHost   string
	gameMode   Mode
	panel      *panelState
	panelHosts []string
	managed    map[string][]apitypes.ACMECertificateUse
}

func (a *Automation) state(ctx context.Context) (certificateState, error) {
	host, err := a.GameHost(ctx)
	if err != nil {
		return certificateState{}, err
	}
	mode, err := a.Game.Mode(ctx)
	if err != nil {
		return certificateState{}, err
	}
	state := certificateState{
		gameHost: host, gameMode: mode, panel: a.panel.Load(), panelHosts: a.panelHosts(host),
		managed: make(map[string][]apitypes.ACMECertificateUse),
	}
	if mode == ModeACME && host != "" {
		state.managed[host] = []apitypes.ACMECertificateUse{apitypes.ACMECertificateUseGame}
	}
	if state.panel.mode == apitypes.PanelCertificateModeACME {
		for _, name := range state.panelHosts {
			state.managed[name] = append(state.managed[name], apitypes.ACMECertificateUsePanel)
		}
	}
	return state, nil
}

func certificateNames(managed map[string][]apitypes.ACMECertificateUse) []string {
	names := make([]string, 0, len(managed))
	for name := range managed {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

type attemptState struct {
	At       *time.Time
	Detail   string
	Failures int
	Retry    time.Time
}

func attemptKey(account ACMEAccount, host string) string {
	return fmt.Sprintf("acme_attempt_%x", sha256.Sum256([]byte(account.Directory+"\x00"+host)))
}

func (a *Automation) attempt(ctx context.Context, account ACMEAccount, host string) (attemptState, error) {
	var status attemptState
	value, ok, err := a.Game.settings.GetSetting(ctx, attemptKey(account, host))
	if ok && err == nil {
		err = json.Unmarshal([]byte(value), &status)
	}
	return status, err
}

func (a *Automation) record(ctx context.Context, account ACMEAccount, host string, state attemptState) {
	data, err := json.Marshal(state)
	if err == nil {
		err = a.Game.settings.SetSetting(context.WithoutCancel(ctx), attemptKey(account, host), string(data))
	}
	if err != nil {
		a.logger.Warn("could not save ACME attempt status", "identifier", host)
	}
}

func (a *Automation) ACME(ctx context.Context) (apitypes.ACME, error) {
	s, err := a.settings(ctx)
	if err != nil {
		return apitypes.ACME{}, err
	}
	state, err := a.state(ctx)
	if err != nil {
		return apitypes.ACME{}, err
	}
	game, err := a.Game.Current(ctx, 0)
	if err != nil {
		return apitypes.ACME{}, err
	}
	account := s.account(a.Directory)
	out := apitypes.ACME{
		Email: s.Email, TermsAgreed: s.TermsAgreed, Staging: s.Staging, DirectoryURL: account.Directory,
		HTTP01Available: a.HTTP01, TLSALPN01Available: a.TLSALPN01, Certificates: []apitypes.ACMECertificate{},
	}
	for _, host := range certificateNames(state.managed) {
		status, err := a.attempt(ctx, account, host)
		if err != nil {
			return apitypes.ACME{}, err
		}
		managed := apitypes.ACMECertificate{Identifier: host, UsedBy: state.managed[host],
			Status: apitypes.ACMECertificateStatusPending, LastAttemptAt: status.At, LastError: status.Detail}
		if slices.Contains(managed.UsedBy, apitypes.ACMECertificateUseGame) && game.Covers(host) {
			managed.Certificate = game.APIInfo(state.gameHost)
		}
		if managed.Certificate == nil {
			if pair := state.panel.issued[host]; pair != nil {
				managed.Certificate = pair.info.APIInfo(state.gameHost)
			}
		}
		if status.Detail != "" {
			managed.Status = apitypes.ACMECertificateStatusError
		} else if managed.Certificate != nil {
			managed.Status = apitypes.ACMECertificateStatusIssued
		}
		out.Certificates = append(out.Certificates, managed)
	}
	return out, nil
}

func (a *Automation) precheck(s accountSettings, names []string) error {
	switch {
	case !s.TermsAgreed:
		return invalid("terms_agreed", "accept the ACME subscriber agreement before requesting certificates")
	case len(names) == 0:
		return invalid("game_host", "configure an effective game host or panel host before requesting a certificate")
	case !a.HTTP01 && !a.TLSALPN01:
		return invalid("mode", "ACME needs --http-listen reachable on port 80 or panel HTTPS on port 443")
	}
	if a.Directory == "" {
		for _, host := range names {
			if !publicIdentifier(host) {
				return invalid("mode", "%s is not eligible for a public certificate; use a public DNS name or IP address", host)
			}
		}
	}
	return nil
}

// IsGlobalUnicast includes private, documentation and reserved addresses.
func publicIdentifier(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return false
		}
		if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
			return false
		}
		for _, special := range []string{
			"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
			"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
			"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
		} {
			if netip.MustParsePrefix(special).Contains(ip) {
				return false
			}
		}
		return true
	}
	_, err := ValidateHost(host)
	tld := host[strings.LastIndexByte(host, '.')+1:]
	return err == nil && tld != host && tld != "test" && tld != "invalid" && tld != "example" &&
		certmagic.SubjectQualifiesForPublicCert(host)
}

type Action int

const (
	IssueGame Action = iota
	IssuePanel
	RenewAll
)

func (a *Automation) targets(action Action, state certificateState) ([]string, error) {
	switch action {
	case IssueGame:
		if state.gameHost == "" {
			return nil, nil
		}
		return []string{state.gameHost}, nil
	case IssuePanel:
		if !a.configurable() {
			return nil, ErrPanelFixed
		}
		return state.panelHosts, nil
	default:
		if len(state.managed) == 0 {
			return nil, ErrNothingManaged
		}
		return certificateNames(state.managed), nil
	}
}

func (a *Automation) plan(ctx context.Context, action Action) (accountSettings, []string, error) {
	state, err := a.state(ctx)
	if err != nil {
		return accountSettings{}, nil, err
	}
	names, err := a.targets(action, state)
	if err != nil {
		return accountSettings{}, nil, err
	}
	s, err := a.settings(ctx)
	if err != nil {
		return accountSettings{}, nil, err
	}
	return s, names, a.precheck(s, names)
}

// Start starts a restricted, subject-serialized administrator certificate job.
func (a *Automation) Start(ctx context.Context, action Action, by int64) (store.Job, error) {
	a.admit.Lock()
	defer a.admit.Unlock()
	if a.Jobs == nil {
		return store.Job{}, jobs.ErrShuttingDown
	}
	if a.Jobs.Busy(certificateSubject()) {
		return store.Job{}, jobs.ErrSubjectBusy
	}
	_, names, err := a.plan(ctx, action)
	if err != nil {
		return store.Job{}, err
	}
	kind := apitypes.JobKindCertificateIssue
	if action == RenewAll {
		kind = apitypes.JobKindCertificateRenew
	}
	return a.Jobs.Start(ctx, string(kind), certificateSubject(), &by, func(ctx context.Context, log io.Writer) error {
		select {
		case a.op <- struct{}{}:
			defer func() { <-a.op }()
		case <-ctx.Done():
			return ctx.Err()
		}
		err := a.run(ctx, action, names, log)
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			msg := operationError(err)
			fmt.Fprintln(log, msg)
			return jobs.Fail(msg, nil)
		}
		return nil
	}, jobs.Restricted(true))
}

func operationError(err error) string {
	var validation *ValidationError
	var failure *acmeFailure
	switch {
	case errors.As(err, &validation):
		return validation.Msg
	case errors.As(err, &failure):
		return failure.message
	case errors.Is(err, errHostsChanged):
		return errHostsChanged.Error()
	default:
		return "Could not install the certificate. Check the daemon log and available disk space, then retry."
	}
}
