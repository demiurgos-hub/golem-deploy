package gameadmin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Profile limits.
const (
	// MaxProfileBytes caps a profile download; larger answers are refused.
	MaxProfileBytes = 64 << 20
	// ProfileTimeoutMargin is added to a timed capture's length (and to
	// RequestTimeout) to bound the whole request: the engine buffers the
	// profile and answers only when the capture ends.
	ProfileTimeoutMargin = 15 * time.Second
)

// Errors of Profile besides ErrUnreachable, ErrUnauthorized, ErrBadResponse
// and ErrNotReady.
var (
	// ErrProfilingUnsupported: the game does not serve profiles (404): the
	// container was started without GOLEM_ADMIN_PROFILING, or its engine
	// predates the profile endpoint.
	ErrProfilingUnsupported = errors.New("gameadmin: the game does not serve profiles")
	// ErrProfileInProgress: the game is already capturing a CPU profile or
	// trace (409).
	ErrProfileInProgress = errors.New("gameadmin: a profile is already being captured")
)

// profileKindRE bounds the kind put into the request path; the caller
// validates it against the engine's kinds.
var profileKindRE = regexp.MustCompile(`^[a-z]{1,16}$`)

// Profile downloads GET /profile/{kind} (with ?seconds=N when seconds is
// positive) and returns the file as the game sent it: the engine's
// compressed protobuf profile or execution trace, which the caller passes on
// without interpreting. It always opens its own connection (use a client
// from New), refuses anything but a 200 application/octet-stream answer
// without Content-Encoding of at most MaxProfileBytes, and refuses a body
// that contains the admin token. Error answers are decided by their status
// alone; their bodies are never read.
func (c *Client) Profile(ctx context.Context, kind string, seconds int) ([]byte, error) {
	if c.session {
		return nil, errors.New("gameadmin: Profile needs a client from New")
	}
	if !profileKindRE.MatchString(kind) || seconds < 0 || seconds > 3600 {
		return nil, errors.New("gameadmin: invalid profile request")
	}
	path := "/profile/" + kind
	if seconds > 0 {
		path += "?seconds=" + strconv.Itoa(seconds)
	}
	c = &Client{addr: c.addr, token: c.token}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout+time.Duration(seconds)*time.Second+ProfileTimeoutMargin)
	defer cancel()
	if err := c.connection(ctx); err != nil {
		return nil, err
	}
	defer c.Close()
	conn := c.conn
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, ErrUnreachable
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.addr+path, nil)
	if err != nil {
		return nil, ErrUnreachable
	}
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/octet-stream")
	if err := req.Write(conn); err != nil {
		return nil, ErrUnreachable
	}
	c.wire.N = 8 << 10
	resp, err := http.ReadResponse(c.reader, req)
	if err != nil {
		var netErr net.Error
		if ctx.Err() != nil || errors.Is(err, io.EOF) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil, ErrUnreachable
		}
		return nil, ErrBadResponse
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	case http.StatusNotFound:
		return nil, ErrProfilingUnsupported
	case http.StatusConflict:
		return nil, ErrProfileInProgress
	case http.StatusServiceUnavailable:
		return nil, ErrNotReady
	default:
		return nil, fmt.Errorf("%w: status %d", ErrBadResponse, resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/octet-stream" {
		return nil, fmt.Errorf("%w: content type is not a file", ErrBadResponse)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return nil, fmt.Errorf("%w: encoded body", ErrBadResponse)
	}
	// Bound decoded bytes and wire overhead (e.g. chunk metadata).
	c.wire.N = MaxProfileBytes + MaxProfileBytes/8
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxProfileBytes+1))
	if err != nil {
		if c.wire.N <= 0 {
			return nil, fmt.Errorf("%w: body larger than %d bytes", ErrBadResponse, MaxProfileBytes)
		}
		return nil, ErrUnreachable
	}
	switch {
	case len(body) > MaxProfileBytes:
		return nil, fmt.Errorf("%w: body larger than %d bytes", ErrBadResponse, MaxProfileBytes)
	case len(body) == 0:
		return nil, fmt.Errorf("%w: empty profile", ErrBadResponse)
	case bytes.Contains(body, []byte(c.token)):
		return nil, fmt.Errorf("%w: the profile contains the admin token", ErrBadResponse)
	}
	return body, nil
}
