package certs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/mholt/acmez/v3/acme"
)

const orderTimeout = 3 * time.Minute

type acmeFailure struct {
	message string
	retry   time.Time
}

func (e *acmeFailure) Error() string { return e.message }

func summarizeACME(err error) *acmeFailure {
	var safe *acmeFailure
	var problem acme.Problem
	var network net.Error
	switch {
	case errors.As(err, &safe):
		return safe
	case errors.As(err, &problem):
		typ := strings.TrimPrefix(problem.Type, "urn:ietf:params:acme:error:")
		if typ == "" || strings.Contains(typ, "-----BEGIN") {
			typ = "request rejected"
		}
		detail := problem.Detail
		if strings.Contains(detail, "-----BEGIN") {
			detail = "The CA returned a response containing PEM data."
		}
		msg := fmt.Sprintf("ACME %s: %s", clean(typ, 60), clean(detail, 200))
		failure := &acmeFailure{message: msg}
		switch {
		case problem.Status == http.StatusTooManyRequests || problem.Type == acme.ProblemTypeRateLimited:
			failure.message += " The CA rate limited this request."
		case problem.Type == acme.ProblemTypeDNS:
			failure.message += " Check that DNS points to this host."
		case problem.Type == acme.ProblemTypeConnection || problem.Type == acme.ProblemTypeUnauthorized ||
			problem.Type == acme.ProblemTypeTLS:
			failure.message += " Check DNS and that the internet can reach port 80 (HTTP-01) or port 443 (TLS-ALPN-01)."
		}
		return failure
	case errors.As(err, &network), errors.Is(err, context.DeadlineExceeded):
		return &acmeFailure{message: "The ACME CA could not be reached or did not respond in time. Check network access to the CA and try again."}
	default:
		return &acmeFailure{message: "The ACME request failed. Check that DNS points here, port 80 or 443 is reachable from the internet, and the CA is available."}
	}
}

func (a *Automation) loadACME(ctx context.Context, account ACMEAccount, host string) (*certificatePair, error) {
	chain, key, err := a.Client.Load(ctx, account, host)
	if err != nil {
		return nil, err
	}
	return validateACMEPair(chain, key, host, a.now())
}

func (a *Automation) obtain(ctx context.Context, account ACMEAccount, host string, force bool, log io.Writer) (*certificatePair, error) {
	at := a.now().UTC()
	methods := []string{}
	if a.HTTP01 {
		methods = append(methods, "HTTP-01")
	}
	if a.TLSALPN01 {
		methods = append(methods, "TLS-ALPN-01")
	}
	fmt.Fprintf(log, "Requesting a certificate for %s (available challenges: %s)\n", host, strings.Join(methods, ", "))
	a.logger.Info("requesting ACME certificate", "identifier", host, "renewal", force, "challenges", methods)
	cctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()
	err := a.Client.Issue(cctx, account, host, force, log)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil {
		pair, err := a.loadACME(ctx, account, host)
		if err == nil {
			return pair, nil
		}
		err = &acmeFailure{message: "The CA did not provide a usable certificate for " + host + "."}
		a.recordFailure(ctx, account, host, at, err, false)
		return nil, err
	}
	failure := summarizeACME(err)
	a.recordFailure(ctx, account, host, at, failure, false)
	return nil, failure
}

func (a *Automation) recordFailure(ctx context.Context, account ACMEAccount, host string, at time.Time, err error, install bool) {
	last, _ := a.attempt(context.WithoutCancel(ctx), account, host)
	count := min(last.Failures+1, 7)
	// Leave room for repeated attempts even in the minimum 12-hour margin.
	wait := min(time.Minute<<(count-1), 30*time.Minute)
	now := a.now()
	next := now.Add(wait)
	var failure *acmeFailure
	if errors.As(err, &failure) && failure.retry.After(next) {
		next = failure.retry
		if latest := now.Add(24 * time.Hour); next.After(latest) {
			next = latest
		}
	}
	message := operationError(err)
	a.record(ctx, account, host, attemptState{At: &at, Detail: message, Failures: count, Retry: next, Install: install, BlockReason: last.BlockReason})
	a.logger.Warn("ACME certificate attempt failed", "identifier", host, "detail", message)
}

func (a *Automation) run(ctx context.Context, action Action, requested []string, log io.Writer) error {
	s, names, err := a.plan(ctx, action)
	if err != nil {
		return err
	}
	if action != RenewAll && !slices.Equal(names, requested) {
		return errHostsChanged
	}
	account := s.account(a.Directory)
	var failures error
	issued := make(map[string]*certificatePair, len(names))
	for _, host := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		at := a.now().UTC()
		pair, err := a.obtain(ctx, account, host, action == RenewAll, log)
		if err == nil && action == RenewAll {
			_, err = a.installManaged(ctx, host, pair, false)
			if err != nil && ctx.Err() == nil {
				a.recordFailure(ctx, account, host, at, err, true)
			}
		}
		var pending *PublicationError
		if ctx.Err() != nil {
			if errors.As(err, &pending) {
				fmt.Fprintf(log, "%s: %s\n", host, pending.Error())
			}
			return ctx.Err()
		}
		if err != nil {
			if action != RenewAll {
				return err
			}
			fmt.Fprintf(log, "%s: %s\n", host, operationError(err))
			failures = errors.Join(failures, err)
			continue
		}
		if action == RenewAll {
			a.record(ctx, account, host, attemptState{At: &at})
			fmt.Fprintf(log, "Installed the certificate for %s\n", host)
		} else {
			issued[host] = pair
			a.record(ctx, account, host, attemptState{At: &at})
			fmt.Fprintf(log, "Certificate ready for %s\n", host)
		}
	}
	if action == RenewAll {
		if err := ctx.Err(); err != nil {
			return err
		}
		return failures
	}

	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	state, err := a.state(ctx)
	if err != nil {
		return err
	}
	current, err := a.targets(action, state)
	if err != nil {
		return err
	}
	if !slices.Equal(current, names) {
		return errHostsChanged
	}
	switch action {
	case IssueGame:
		host := names[0]
		pair := issued[host]
		if _, err := a.Game.useShared(ctx, ModeACME, pair.chain, pair.key); err != nil {
			var pending *PublicationError
			if errors.As(err, &pending) && ctx.Err() != nil {
				fmt.Fprintln(log, pending.Error())
				return nil
			}
			return err
		}
		// If this identifier is shared, reconciliation also publishes
		// the newly obtained pair to the already-active panel.
	case IssuePanel:
		p := *state.panel
		p.mode, p.hosts, p.issued = apitypes.PanelCertificateModeACME, names, issued
		for _, host := range names {
			if err := a.writePanelPair(panelPairName(host), issued[host]); err != nil {
				return err
			}
		}
		if err := a.fallback(&p, state.gameHost); err != nil {
			return err
		}
		if err := a.commitPanel(ctx, &p); err != nil {
			return err
		}
	}
	fmt.Fprintln(log, "Installed the certificate and enabled ACME mode")
	a.wakeup()
	return nil
}

func (a *Automation) installManaged(ctx context.Context, host string, pair *certificatePair, newerOnly bool) (bool, error) {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, err := a.state(ctx)
	if err != nil {
		return false, err
	}
	uses := state.managed[host]
	if len(uses) == 0 {
		return false, errHostsChanged
	}
	changed := false
	var pending *PublicationError
	if slices.Contains(uses, apitypes.ACMECertificateUseGame) {
		old, err := a.Game.Current(ctx, 0)
		if err != nil {
			return false, err
		}
		if pair.info.Fingerprint != old.Fingerprint && (!newerOnly || newer(pair.info, old, host)) {
			if _, err := a.Game.useShared(ctx, ModeACME, pair.chain, pair.key); err != nil && !errors.As(err, &pending) {
				return false, err
			}
			changed = true
		}
	}
	if ctx.Err() != nil && pending != nil {
		return changed, pending
	}
	if slices.Contains(uses, apitypes.ACMECertificateUsePanel) {
		old := state.panel.issued[host]
		if old == nil || (pair.info.Fingerprint != old.info.Fingerprint && (!newerOnly || newer(pair.info, old.info, host))) {
			if err := a.setPanelPair(host, pair); err != nil {
				if pending != nil {
					return changed, errors.Join(pending, err)
				}
				return changed, err
			}
			changed = true
		}
	}
	if pending != nil {
		return changed, pending
	}
	return changed, nil
}

func newer(pair, installed Info, host string) bool {
	return !installed.Covers(host) || !pair.NotBefore.Before(installed.NotBefore)
}

func (a *Automation) Maintain(ctx context.Context) {
	publicationDone := make(chan struct{})
	go func() {
		defer close(publicationDone)
		a.maintainPublication(ctx)
	}()
	defer func() { <-publicationDone }()
	hourly := time.NewTicker(CheckInterval)
	defer hourly.Stop()
	retry := time.NewTicker(time.Minute)
	defer retry.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		a.maintainOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-hourly.C:
			a.Game.Check(ctx, a.GameHost)
		case <-retry.C:
		case <-a.wake:
		}
	}
}

func (a *Automation) maintainPublication(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.Game.publicationWake:
		case <-retry:
		}
		retry = nil
		a.Game.mu.Lock()
		err := a.Game.finishPublication(ctx)
		a.Game.mu.Unlock()
		if err != nil && !errors.Is(err, errMountHeld) && ctx.Err() == nil {
			retry = time.After(time.Second)
		}
	}
}

func (a *Automation) maintainOnce(ctx context.Context) {
	a.admit.Lock()
	if a.Jobs != nil && a.Jobs.Busy(certificateSubject()) {
		a.admit.Unlock()
		return
	}
	select {
	case a.op <- struct{}{}:
		a.admit.Unlock()
		defer func() { <-a.op }()
	default:
		a.admit.Unlock()
		return
	}
	if err := a.check(ctx, true); err != nil && ctx.Err() == nil {
		a.logger.Warn("automatic certificate check failed", "detail", operationError(err))
	}
}

func (a *Automation) refreshPanel(ctx context.Context) error {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	host, err := a.GameHost(ctx)
	if err != nil {
		return err
	}
	p := *a.panel.Load()
	p.hosts = a.panelHosts(host)
	if a.configurable() {
		if err := a.fallback(&p, host); err != nil {
			return err
		}
	}
	if p.mode == apitypes.PanelCertificateModeACME {
		active := make(map[string]*certificatePair, len(p.hosts))
		for _, name := range p.hosts {
			if pair := p.issued[name]; pair != nil {
				active[name] = pair
			}
		}
		p.issued = active
	}
	a.panel.Store(&p)
	return nil
}

// check first reconciles storage, then obtains missing or due certificates.
func (a *Automation) check(ctx context.Context, network bool) error {
	if err := a.refreshPanel(ctx); err != nil {
		return err
	}
	state, err := a.state(ctx)
	if err != nil || len(state.managed) == 0 {
		return err
	}
	s, err := a.settings(ctx)
	if err != nil {
		return err
	}
	account := s.account(a.Directory)
	for host := range a.schedules {
		if _, ok := state.managed[host]; !ok {
			delete(a.schedules, host)
		}
	}
	var failures error
	for _, host := range certificateNames(state.managed) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.checkIdentifier(ctx, account, host, network, a.precheck(s, []string{host}))
		failures = errors.Join(failures, err)
	}
	return failures
}

func (a *Automation) checkIdentifier(ctx context.Context, account ACMEAccount, host string, network bool, blocked error) error {
	last, err := a.attempt(ctx, account, host)
	if err != nil {
		return err
	}
	var reason string
	if blocked != nil {
		reason = operationError(blocked)
	}
	if reason != last.BlockReason {
		if blocked != nil {
			at := a.now().UTC()
			last.At = &at
			a.logger.Warn("automatic certificate renewal blocked", "identifier", host, "detail", reason)
		}
		last.BlockReason = reason
		a.record(ctx, account, host, last)
	}
	if last.Install && last.Retry.After(a.now()) {
		return nil
	}
	pair, err := a.loadACME(ctx, account, host)
	if err == nil {
		reconciled, err := a.installManaged(ctx, host, pair, true)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			a.recordFailure(ctx, account, host, a.now().UTC(), err, true)
			return nil
		}
		if last.Install || (reconciled && last.Detail != "") {
			last = attemptState{At: last.At, BlockReason: last.BlockReason}
			a.record(ctx, account, host, last)
		}
	}
	if !network || blocked != nil || last.Retry.After(a.now()) {
		return nil
	}
	if pair != nil && !a.renewalDue(ctx, account, host, pair) {
		return nil
	}
	at := a.now().UTC()
	pair, err = a.obtain(ctx, account, host, pair != nil, io.Discard)
	if err != nil {
		return nil // already recorded and logged safely
	}
	if _, err := a.installManaged(ctx, host, pair, false); err != nil {
		a.recordFailure(ctx, account, host, at, err, true)
		return nil
	}
	a.record(ctx, account, host, attemptState{At: &at})
	a.logger.Info("installed automatic certificate", slog.String("identifier", host))
	return nil
}

func (a *Automation) renewalDue(ctx context.Context, account ACMEAccount, host string, pair *certificatePair) bool {
	now := a.now()
	info := pair.info
	schedule := a.schedules[host]
	if schedule.fingerprint != info.Fingerprint || !schedule.refresh.After(now) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		when, refresh, err := a.Client.RenewalInfo(cctx, account, host, info.leaf)
		cancel()
		if err != nil && schedule.fingerprint == info.Fingerprint {
			when = schedule.when
		}
		if refresh.Before(now.Add(time.Hour)) {
			refresh = now.Add(time.Hour)
		}
		if latest := now.Add(24 * time.Hour); refresh.After(latest) {
			refresh = latest
		}
		schedule = renewalSchedule{fingerprint: info.Fingerprint, when: when, refresh: refresh}
		a.schedules[host] = schedule
	}
	halfLife := info.NotBefore.Add(info.NotAfter.Sub(info.NotBefore) / 2)
	when := schedule.when
	if when.IsZero() {
		when = halfLife
	}
	margin := min(max(info.NotAfter.Sub(info.NotBefore)/4, 12*time.Hour), 7*24*time.Hour)
	if latest := info.NotAfter.Add(-margin); when.After(latest) {
		when = latest
	}
	return !now.Before(when)
}
