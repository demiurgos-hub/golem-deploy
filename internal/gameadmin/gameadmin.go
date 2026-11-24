// Package gameadmin is a small, strict client for the Golem Engine admin
// endpoint of a game server container (golem/host AdminOptions): GET
// /ready, /info and /stats and POST /snapshot with a bearer token.
//
// The game is untrusted code, so is everything it answers. The client only
// ever connects to 127.0.0.1 on the port Docker published the container's
// admin port on (it never resolves names, uses no proxy and never
// redials during a multi-request operation), bounds every request with a short timeout, never follows
// redirects, refuses compressed bodies, caps the response size, and
// returns only bounded, sanitised values. The token is sent in the
// Authorization header only and never appears in errors or logs.
package gameadmin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits.
const (
	// RequestTimeout bounds one request, connection included.
	RequestTimeout = 3 * time.Second
	// MaxResponseBytes caps a response body; larger answers are refused.
	MaxResponseBytes = 64 << 10
	// MaxString caps every string taken from an answer, in bytes.
	MaxString = 128
	// MinTokenLength is the engine's minimum admin token length.
	MinTokenLength = 16
)

// Errors contain no data received from the endpoint.
var (
	// ErrUnreachable: no connection or no complete answer in time.
	ErrUnreachable = errors.New("gameadmin: the admin endpoint did not answer")
	// ErrUnauthorized: the endpoint refused the token (401/403).
	ErrUnauthorized = errors.New("gameadmin: the admin endpoint refused the request")
	// ErrBadResponse: an answer that is not what the contract says
	// (unexpected status, redirect, wrong type, too large, malformed JSON).
	ErrBadResponse = errors.New("gameadmin: unexpected answer from the admin endpoint")
)

// Client talks to one container's admin endpoint.
type Client struct {
	addr      string
	token     string
	session   bool
	attempted bool
	conn      net.Conn
	wire      io.LimitedReader
	reader    *bufio.Reader
}

// New returns a client that opens a fresh loopback connection per request.
func New(port int, token string) (*Client, error) {
	return newClient(port, token, false)
}

// NewSession allows exactly one connection for a sequence of requests.
// Use it serially and Close it when done. A peer that closes the connection
// aborts the sequence rather than receiving the token again on a fresh socket.
func NewSession(port int, token string) (*Client, error) {
	return newClient(port, token, true)
}

func newClient(port int, token string, session bool) (*Client, error) {
	if port <= 0 || port > 65535 {
		return nil, errors.New("gameadmin: invalid port")
	}
	if len(token) < MinTokenLength || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("gameadmin: invalid token")
	}
	return &Client{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), token: token, session: session}, nil
}

// Close releases a session's connection.
func (c *Client) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// connection dials only the inspected address and only once per session.
func (c *Client) connection(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	if c.session && c.attempted {
		return ErrUnreachable
	}
	c.attempted = true
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", c.addr)
	if err != nil {
		return ErrUnreachable
	}
	c.conn = conn
	c.wire.R = conn
	c.reader = bufio.NewReader(&c.wire)
	return nil
}

// get performs GET path and decodes a JSON answer with an accepted status
// into dst; it returns the status.
func (c *Client) get(ctx context.Context, path string, dst any, accept ...int) (int, error) {
	return c.request(ctx, http.MethodGet, path, RequestTimeout, dst, accept...)
}

// request performs method path (without a body) within timeout and
// decodes a JSON answer with an accepted status into dst.
func (c *Client) request(ctx context.Context, method, path string, timeout time.Duration, dst any, accept ...int) (int, error) {
	if !c.session {
		// Non-session callers can use the same configuration concurrently.
		c = &Client{addr: c.addr, token: c.token}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.connection(ctx); err != nil {
		return 0, err
	}
	keep := false
	defer func() {
		if !keep {
			c.Close()
		}
	}()
	conn := c.conn
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, ErrUnreachable
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.addr+path, nil)
	if err != nil {
		return 0, ErrUnreachable
	}
	req.Close = !c.session
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if err := req.Write(conn); err != nil {
		return 0, ErrUnreachable
	}
	// Parse the response directly so there is no retry, proxy, redirect,
	// compression or transport logging of arbitrary bytes from the peer.
	c.wire.N = 8 << 10
	resp, err := http.ReadResponse(c.reader, req)
	if err != nil {
		var netErr net.Error
		if ctx.Err() != nil || errors.Is(err, io.EOF) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return 0, ErrUnreachable
		}
		return 0, ErrBadResponse
	}
	// Bound decoded bytes and wire overhead (e.g. chunk metadata).
	c.wire.N = 2 * MaxResponseBytes
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return resp.StatusCode, fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	case !containsInt(accept, resp.StatusCode):
		return resp.StatusCode, fmt.Errorf("%w: status %d", ErrBadResponse, resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return resp.StatusCode, fmt.Errorf("%w: content type is not JSON", ErrBadResponse)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return resp.StatusCode, fmt.Errorf("%w: encoded body", ErrBadResponse)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, ErrUnreachable
	}
	if len(body) > MaxResponseBytes {
		return resp.StatusCode, fmt.Errorf("%w: body larger than %d bytes", ErrBadResponse, MaxResponseBytes)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: malformed JSON", ErrBadResponse)
	}
	// ReadAll reached the end of the body, so no data remains to drain.
	_ = resp.Body.Close()
	keep = c.session && !resp.Close && ctx.Err() == nil
	return resp.StatusCode, nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Readiness is the answer of GET /ready.
type Readiness struct {
	Ready bool
	// Reason is the engine's reason when not ready ("starting",
	// "stopping"), sanitised.
	Reason string
}

// Ready asks whether the game is ready to accept players (200 with
// {"ready":true}; 503 with a reason while starting or stopping).
func (c *Client) Ready(ctx context.Context) (Readiness, error) {
	var v struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	}
	status, err := c.get(ctx, "/ready", &v, http.StatusOK, http.StatusServiceUnavailable)
	if err != nil {
		return Readiness{}, err
	}
	return Readiness{Ready: status == http.StatusOK && v.Ready, Reason: c.clean(v.Reason, 32)}, nil
}

// Info is the sanitised answer of GET /info.
type Info struct {
	Name              string
	SchemaFingerprint string
	EngineVersion     string
	GoVersion         string
	Transport         string
	TickRate          int
	// CertificateHashesPublished reports whether the game publishes its
	// certificate hashes for browser pinning.
	CertificateHashesPublished bool
	// HTTPCertificateHashes and WebTransportCertificateHashes are the
	// lower-case hex SHA-256 hashes of the served chains (at most 8).
	HTTPCertificateHashes         []string
	WebTransportCertificateHashes []string
}

type hashJSON struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// Info reads the game's facts.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var v struct {
		Name                          string     `json:"name"`
		SchemaFingerprint             string     `json:"schema_fingerprint"`
		EngineVersion                 string     `json:"engine_version"`
		GoVersion                     string     `json:"go_version"`
		Transport                     string     `json:"transport"`
		TickRate                      float64    `json:"tick_rate"`
		CertificateHashesPublished    bool       `json:"certificate_hashes_published"`
		WebTransportCertificateHashes []hashJSON `json:"webtransport_certificate_hashes"`
		HTTPCertificateHashes         []hashJSON `json:"http_certificate_hashes"`
	}
	if _, err := c.get(ctx, "/info", &v, http.StatusOK); err != nil {
		return Info{}, err
	}
	return Info{
		Name:                          c.clean(v.Name, MaxString),
		SchemaFingerprint:             Fingerprint(c.withoutToken(v.SchemaFingerprint)),
		EngineVersion:                 c.clean(v.EngineVersion, 64),
		GoVersion:                     c.clean(v.GoVersion, 32),
		Transport:                     Transport(c.withoutToken(v.Transport)),
		TickRate:                      clampInt(v.TickRate),
		CertificateHashesPublished:    v.CertificateHashesPublished,
		WebTransportCertificateHashes: c.hashes(v.WebTransportCertificateHashes),
		HTTPCertificateHashes:         c.hashes(v.HTTPCertificateHashes),
	}, nil
}

// SnapshotTimeout bounds POST /snapshot: the engine gives a snapshot 60
// seconds before it answers 504 itself.
const SnapshotTimeout = 75 * time.Second

// Errors of Snapshot besides ErrUnreachable, ErrUnauthorized and
// ErrBadResponse.
var (
	// ErrSnapshotInProgress: another snapshot is being written (409).
	ErrSnapshotInProgress = errors.New("gameadmin: a snapshot is already in progress")
	// ErrSnapshotUnsupported: the game has no snapshot hook (501).
	ErrSnapshotUnsupported = errors.New("gameadmin: the game does not support snapshots")
	// ErrSnapshotTimeout: the engine gave up waiting for the snapshot (504).
	ErrSnapshotTimeout = errors.New("gameadmin: the snapshot timed out")
	// ErrSnapshotFailed: the game could not write the snapshot (500).
	ErrSnapshotFailed = errors.New("gameadmin: the snapshot failed")
)

// Snapshot asks the game to write its entity snapshot now (POST /snapshot,
// no body) and returns how long it took as the game reports it (bounded).
// The answer's status decides the outcome; error bodies are only checked to
// be JSON.
func (c *Client) Snapshot(ctx context.Context) (time.Duration, error) {
	var v struct {
		OK         bool    `json:"ok"`
		DurationMs float64 `json:"duration_ms"`
	}
	status, err := c.request(ctx, http.MethodPost, "/snapshot", SnapshotTimeout, &v, http.StatusOK, http.StatusConflict,
		http.StatusNotImplemented, http.StatusGatewayTimeout, http.StatusInternalServerError)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusOK:
		if !v.OK {
			return 0, fmt.Errorf("%w: snapshot not confirmed", ErrBadResponse)
		}
		return time.Duration(math.Min(clampFloat(v.DurationMs), float64(time.Hour/time.Millisecond))) * time.Millisecond, nil
	case http.StatusConflict:
		return 0, ErrSnapshotInProgress
	case http.StatusNotImplemented:
		return 0, ErrSnapshotUnsupported
	case http.StatusGatewayTimeout:
		return 0, ErrSnapshotTimeout
	}
	return 0, ErrSnapshotFailed
}

// Stats is the sanitised answer of GET /stats.
type Stats struct {
	UptimeSeconds float64
	Sessions      int
	Entities      int
	Tick          int64
	TickRate      int
	BudgetMs      float64
	LastMs        float64
	MaxMs         float64
	Overruns      int64
}

// Stats reads session, entity and tick statistics.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var v struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Sessions      float64 `json:"sessions"`
		Entities      float64 `json:"entities"`
		Tick          struct {
			Tick     float64 `json:"tick"`
			Rate     float64 `json:"rate"`
			BudgetMs float64 `json:"budget_ms"`
			LastMs   float64 `json:"last_ms"`
			MaxMs    float64 `json:"max_ms"`
			Overruns float64 `json:"overruns"`
		} `json:"tick"`
	}
	if _, err := c.get(ctx, "/stats", &v, http.StatusOK); err != nil {
		return Stats{}, err
	}
	return Stats{
		UptimeSeconds: clampFloat(v.UptimeSeconds),
		Sessions:      clampInt(v.Sessions),
		Entities:      clampInt(v.Entities),
		Tick:          clampInt64(v.Tick.Tick),
		TickRate:      clampInt(v.Tick.Rate),
		BudgetMs:      clampFloat(v.Tick.BudgetMs),
		LastMs:        clampFloat(v.Tick.LastMs),
		MaxMs:         clampFloat(v.Tick.MaxMs),
		Overruns:      clampInt64(v.Tick.Overruns),
	}, nil
}

// Numbers are taken as JSON numbers (float64) and clamped, so neither a
// negative nor a huge value from the game survives.
const (
	maxInt   = 1 << 31
	maxInt64 = 1 << 53 // exact in JSON and JavaScript
	maxFloat = 1e12
)

func clampFloat(f float64) float64 {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	return math.Min(f, maxFloat)
}

func clampInt(f float64) int { return int(math.Min(math.Floor(clampFloat(f)), maxInt)) }

func clampInt64(f float64) int64 { return int64(math.Min(math.Floor(clampFloat(f)), maxInt64)) }

var fingerprintRE = regexp.MustCompile(`^[A-Za-z0-9._:+/=-]{1,128}$`)

// Fingerprint returns s if it looks like a schema fingerprint (at most 128
// characters of letters, digits and ._:+/=-), otherwise "".
func Fingerprint(s string) string {
	if fingerprintRE.MatchString(s) {
		return s
	}
	return ""
}

// Transport returns the engine's transport name if it is a known one,
// otherwise "unknown" ("" stays "").
func Transport(s string) string {
	switch s {
	case "", "webtransport", "websocket":
		return s
	}
	return "unknown"
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (c *Client) withoutToken(s string) string {
	if strings.Contains(s, c.token) {
		return ""
	}
	return s
}

func (c *Client) clean(s string, max int) string {
	return c.withoutToken(Clean(c.withoutToken(s), max))
}

func (c *Client) hashes(list []hashJSON) []string {
	out := []string{}
	for _, h := range list {
		if len(out) == 8 {
			break
		}
		if strings.EqualFold(c.withoutToken(h.Algorithm), "sha-256") && hexRE.MatchString(c.withoutToken(h.Value)) {
			out = append(out, h.Value)
		}
	}
	return out
}

// Clean makes an untrusted string safe to store and show: invalid UTF-8
// becomes U+FFFD, control, format (bidirectional overrides, zero-width)
// and line/paragraph separator characters are dropped, surrounding space
// is trimmed and the result is cut to at most max bytes on a character
// boundary.
func Clean(s string, max int) string {
	if len(s) > 4*max+16 {
		s = s[:4*max+16]
	}
	s = strings.ToValidUTF8(s, "\ufffd")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
