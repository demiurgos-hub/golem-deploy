package inspector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The hub is reached only by the daemon, over its loopback port. It runs
// in production mode behind the daemon as its proxy, with this fixed
// public origin; every request the daemon sends or relays carries it.
const (
	hubHost   = "inspector.golem-deploy.internal"
	HubOrigin = "https://" + hubHost
	// hubCookie is the hub's session cookie name (hub default).
	hubCookie = "golem_inspector_session"
	// HubSessionMaxAge is the hub's GOLEM_INSPECTOR_SESSION_MAX_AGE: longer
	// than SessionMaxAge, so a hub session outlives the daemon session it
	// belongs to.
	HubSessionMaxAge = 12 * time.Hour
	// maxTargetsBody bounds the hub's /api/targets answer.
	maxTargetsBody = 2 << 20
	// maxTargets bounds the targets read from it.
	maxTargets = 4096
)

// Hub login outcomes. A 401 or 429 from the hub's login is never retried
// in a loop: loginGuard blocks further attempts.
var (
	errHubCredentials = errors.New("inspector: the hub refused the daemon's password")
	errHubRateLimited = errors.New("inspector: the hub rate-limits logins")
	errHubLogin       = errors.New("inspector: the hub login failed")
	errHubAuth        = errors.New("inspector: the hub session is not valid")
	errHubBlocked     = errors.New("inspector: hub logins are paused after a refusal")
	// errHubAnswer: the hub answered something the daemon does not relay
	// (too large to check).
	errHubAnswer = errors.New("inspector: the hub's answer cannot be relayed")
	// errHubSessionLost: the hub lost the session's hub session and a new
	// one could not be opened.
	errHubSessionLost = errors.New("inspector: the hub session could not be renewed")
)

// transportError marks a failure to talk to the hub at all (dial, reset,
// timeout). The hub port is not used again until Docker confirms it.
type transportError struct{ err error }

func (e *transportError) Error() string { return "inspector: the hub did not answer: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func isTransport(err error) bool {
	var te *transportError
	return errors.As(err, &te)
}

// hubClient talks to the hub's loopback port: no proxy, no redirects, no
// compression, short timeouts.
type hubClient struct {
	http *http.Client
}

func newHubTransport() *http.Transport {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		DisableCompression:    true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     false,
	}
}

func newHubClient() *hubClient {
	return &hubClient{http: &http.Client{
		Transport:     newHubTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       15 * time.Second,
	}}
}

func hubBase(port int) string { return "http://127.0.0.1:" + strconv.Itoa(port) }

func (c *hubClient) request(ctx context.Context, method string, port int, path string, body []byte, sid string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, hubBase(port)+path, rd)
	if err != nil {
		return nil, err
	}
	req.Host = hubHost
	req.Header.Set("Origin", HubOrigin)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: hubCookie, Value: sid})
	}
	resp, err := c.http.Do(req)
	if err != nil && ctx.Err() == nil {
		return nil, &transportError{err}
	}
	return resp, err
}

// rateLimitedError carries the hub's Retry-After.
type rateLimitedError struct{ retryAfter time.Duration }

func (e *rateLimitedError) Error() string { return errHubRateLimited.Error() }
func (e *rateLimitedError) Unwrap() error { return errHubRateLimited }

// login logs in at the hub and returns its session id.
func (c *hubClient) login(ctx context.Context, port int, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{"password": password})
	resp, err := c.request(ctx, http.MethodPost, port, "/auth/login", body, "")
	if err != nil {
		return "", fmt.Errorf("%w: %w", errHubLogin, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", errHubCredentials
	case http.StatusTooManyRequests:
		wait := 15 * time.Minute
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 && n <= 24*3600 {
			wait = time.Duration(n) * time.Second
		}
		return "", &rateLimitedError{retryAfter: wait}
	default:
		return "", fmt.Errorf("%w: status %d", errHubLogin, resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == hubCookie && ck.Value != "" && len(ck.Value) <= 256 {
			return ck.Value, nil
		}
	}
	return "", fmt.Errorf("%w: no session cookie", errHubLogin)
}

// logout ends a hub session (best effort).
func (c *hubClient) logout(ctx context.Context, port int, sid string) error {
	if sid == "" || port == 0 {
		return nil
	}
	resp, err := c.request(ctx, http.MethodPost, port, "/auth/logout", nil, sid)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.Body.Close()
}

// ready asks the hub's readiness endpoint.
func (c *hubClient) ready(ctx context.Context, port int) error {
	resp, err := c.request(ctx, http.MethodGet, port, "/ready", nil, "")
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub not ready: status %d", resp.StatusCode)
	}
	return nil
}

// hubTarget is the part of the hub's TargetInfo the daemon reads; game
// supplied text (labels, errors, entity types) is not decoded.
type hubTarget struct {
	TargetID    string     `json:"targetId"`
	Connected   bool       `json:"connected"`
	EntityCount int        `json:"entityCount"`
	ConnectedAt *time.Time `json:"connectedAt"`
}

// targets reads the hub's target list with a hub session.
func (c *hubClient) targets(ctx context.Context, port int, sid string) ([]hubTarget, error) {
	resp, err := c.request(ctx, http.MethodGet, port, "/api/targets", nil, sid)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errHubAuth
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub targets: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTargetsBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTargetsBody {
		return nil, errors.New("hub targets: answer too large")
	}
	var doc struct {
		Targets []hubTarget `json:"targets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("hub targets: %w", err)
	}
	if len(doc.Targets) > maxTargets {
		doc.Targets = doc.Targets[:maxTargets]
	}
	return doc.Targets, nil
}

// loginGuard pauses hub logins after the hub refused one: after a 429
// until its Retry-After, after a 401 (the hub does not know the daemon's
// password) for good. A hold belongs to one hub generation (the hub
// container and the secrets' generation); it ends only when that
// generation changes, and a later answer never shortens it. It lives in
// memory, so the hold applies within one run of the daemon: after a
// restart the daemon tries to log in once more.
type loginGuard struct {
	mu    sync.Mutex
	key   string
	until time.Time
	cause error
}

// blocked reports whether logins at the hub generation key are paused, and
// why.
func (g *loginGuard) blocked(key string, now time.Time) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key != key || !now.Before(g.until) {
		return false, nil
	}
	return true, g.cause
}

// record notes the outcome of a login at the hub generation key.
func (g *loginGuard) record(key string, now time.Time, err error) {
	var rl *rateLimitedError
	var until time.Time
	switch {
	case errors.As(err, &rl):
		until = now.Add(rl.retryAfter)
	case errors.Is(err, errHubCredentials):
		until = now.Add(100 * 365 * 24 * time.Hour)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key != key {
		g.key, g.until, g.cause = key, time.Time{}, nil
	}
	if until.After(g.until) {
		g.until = until
		if errors.Is(err, errHubCredentials) {
			g.cause = errHubCredentials
		} else {
			g.cause = errHubRateLimited
		}
	}
}
