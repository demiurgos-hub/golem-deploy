package inspector

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wsHandshakeTimeout bounds a relayed WebSocket upgrade until the hub
// answered it with a valid 101; the stream then runs without it.
const wsHandshakeTimeout = 30 * time.Second

// handshake is the deadline of one relayed WebSocket upgrade.
type handshake struct {
	t *time.Timer
}

// startHandshake returns ctx with a deadline of d that only done clears.
func startHandshake(parent context.Context, d time.Duration) (context.Context, *handshake, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return ctx, &handshake{t: time.AfterFunc(d, cancel)}, cancel
}

// done clears the deadline after a valid 101; false when it passed
// already. A nil handshake (not an upgrade) has nothing to clear.
func (h *handshake) done() bool {
	return h == nil || h.t.Stop()
}

// wsGUID is the key suffix of RFC 6455.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsAccept is the Sec-WebSocket-Accept value for a Sec-WebSocket-Key.
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// cancelSet holds the cancel functions of running requests, so closing
// admission can end them all (drain).
type cancelSet struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]context.CancelFunc
}

func newCancelSet() *cancelSet { return &cancelSet{m: map[uint64]context.CancelFunc{}} }

// add returns a context that closeAll cancels and its release.
func (c *cancelSet) add(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.next++
	id := c.next
	c.m[id] = cancel
	c.mu.Unlock()
	return ctx, func() {
		cancel()
		c.mu.Lock()
		delete(c.m, id)
		c.mu.Unlock()
	}
}

// closeAll cancels every registered request.
func (c *cancelSet) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, cancel := range c.m {
		cancel()
		delete(c.m, id)
	}
}

// count returns the number of registered requests.
func (c *cancelSet) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// trackerKey carries the listener's tracker in a request context.
type trackerKey struct{}

// tracker counts the running handlers of one listener and holds the client
// connections of relayed (hijacked) streams, so closing the listener can
// wait for them and close what is left. http.Server.Shutdown does neither
// for hijacked connections.
type tracker struct {
	mu      sync.Mutex
	active  int
	changed chan struct{}
	conns   map[*trackedConn]struct{}
}

func newTracker() *tracker {
	return &tracker{changed: make(chan struct{}), conns: map[*trackedConn]struct{}{}}
}

// wrap runs h under the tracker.
func (t *tracker) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		t.active++
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			t.active--
			close(t.changed)
			t.changed = make(chan struct{})
			t.mu.Unlock()
		}()
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), trackerKey{}, t)))
	})
}

// wait waits until no handler runs; false when ctx ended first.
func (t *tracker) wait(ctx context.Context) bool {
	for {
		t.mu.Lock()
		n, ch := t.active, t.changed
		t.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// add tracks the hijacked client connection c: ending ctx closes it.
func (t *tracker) add(ctx context.Context, c net.Conn) *trackedConn {
	tc := &trackedConn{Conn: c, t: t}
	t.mu.Lock()
	t.conns[tc] = struct{}{}
	t.mu.Unlock()
	context.AfterFunc(ctx, func() { _ = tc.Close() })
	return tc
}

// closeAll closes every tracked connection.
func (t *tracker) closeAll() {
	t.mu.Lock()
	list := make([]*trackedConn, 0, len(t.conns))
	for c := range t.conns {
		list = append(list, c)
	}
	t.mu.Unlock()
	for _, c := range list {
		_ = c.Close()
	}
}

// streams returns the number of open tracked connections.
func (t *tracker) streams() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// running returns the number of running handlers.
func (t *tracker) running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// trackedConn is a relayed client connection. It hides CloseWrite, so when
// the hub ends the stream the proxy closes the client connection instead
// of waiting for a client that may never send again.
type trackedConn struct {
	net.Conn
	t    *tracker
	once sync.Once
	err  error
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.t.mu.Lock()
		delete(c.t.conns, c)
		c.t.mu.Unlock()
	})
	return c.err
}

// proxyWriter is the ResponseWriter the reverse proxies write to. It never
// forwards informational (1xx) responses or trailers of the hub, and hands
// out a hijacked client connection only as a trackedConn that ending ctx
// closes. Header changes become visible at WriteHeader only, after the
// header filters of the proxy ran.
type proxyWriter struct {
	w     http.ResponseWriter
	ctx   context.Context
	h     http.Header
	wrote bool
}

func newProxyWriter(w http.ResponseWriter, ctx context.Context) *proxyWriter {
	return &proxyWriter{w: w, ctx: ctx, h: w.Header().Clone()}
}

// Header returns the header to build; after WriteHeader it returns a map
// that is never sent (the proxy writes trailers there).
func (p *proxyWriter) Header() http.Header { return p.h }

func (p *proxyWriter) WriteHeader(code int) {
	if p.wrote || (code >= 100 && code < 200) {
		return
	}
	p.wrote = true
	real := p.w.Header()
	clear(real)
	for k, v := range p.h {
		if k == "Trailer" || strings.HasPrefix(k, http.TrailerPrefix) {
			continue
		}
		real[k] = v
	}
	p.h = http.Header{}
	p.w.WriteHeader(code)
}

func (p *proxyWriter) Write(b []byte) (int, error) {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	return p.w.Write(b)
}

func (p *proxyWriter) Flush() {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(p.w).Flush()
}

// Hijack takes over the client connection of a relayed upgrade: the
// request deadlines are cleared and the connection is tracked.
func (p *proxyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	t, _ := p.ctx.Value(trackerKey{}).(*tracker)
	if t == nil {
		return nil, nil, errors.New("inspector: relayed stream without a tracker")
	}
	c, brw, err := http.NewResponseController(p.w).Hijack()
	if err != nil {
		return nil, nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return t.add(p.ctx, c), brw, nil
}
