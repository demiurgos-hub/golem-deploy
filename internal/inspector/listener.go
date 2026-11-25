package inspector

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Listener timeouts and limits.
const (
	// firstByteTimeout bounds the wait for a connection's first byte, which
	// tells TLS (0x16, a handshake record) from plaintext HTTP.
	firstByteTimeout = 10 * time.Second
	// maxSniffing bounds connections waiting for their first byte.
	maxSniffing = 512
	// maxConns bounds the open connections of the whole listener (browsers
	// and agents, TLS and plaintext) for their whole life, relayed
	// WebSocket streams included.
	maxConns          = 1024
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	// listenerShutdownGrace bounds the graceful close of the listener.
	listenerShutdownGrace = 5 * time.Second
)

// limitListener accepts at most cap(sem) open connections; a connection
// beyond that is closed at once. The slot is released when the connection
// is closed, also after a hijack.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(ln net.Listener, n int) *limitListener {
	return &limitListener{Listener: ln, sem: make(chan struct{}, n)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitConn{Conn: c, sem: l.sem}, nil
		default:
			_ = c.Close()
		}
	}
}

// open returns the number of open connections.
func (l *limitListener) open() int { return len(l.sem) }

type limitConn struct {
	net.Conn
	sem  chan struct{}
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.sem })
	return err
}

// peekedConn returns the sniffed first byte before the rest.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// chanListener hands out the connections the sniffer routed to it.
type chanListener struct {
	ch     chan net.Conn
	done   <-chan struct{}
	addr   net.Addr
	closer func() error
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error   { return l.closer() }
func (l *chanListener) Addr() net.Addr { return l.addr }

// sniffer accepts on one TCP listener and routes each connection by its
// first byte: 0x16 to the TLS side, anything else to the plaintext side.
// Each connection waits for its first byte on its own goroutine (bounded,
// with a deadline), so a slow client never blocks the accept loop. Closing
// the sniffer also closes the connections still waiting.
type sniffer struct {
	ln        net.Listener
	tls       *chanListener
	plain     *chanListener
	done      chan struct{}
	closeOnce sync.Once
	sem       chan struct{}
	timeout   time.Duration
	wg        sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	pending map[net.Conn]struct{}
}

func newSniffer(ln net.Listener, timeout time.Duration) *sniffer {
	s := &sniffer{ln: ln, done: make(chan struct{}), sem: make(chan struct{}, maxSniffing), timeout: timeout, pending: map[net.Conn]struct{}{}}
	s.tls = &chanListener{ch: make(chan net.Conn), done: s.done, addr: ln.Addr(), closer: s.close}
	s.plain = &chanListener{ch: make(chan net.Conn), done: s.done, addr: ln.Addr(), closer: s.close}
	s.wg.Go(s.loop)
	return s
}

func (s *sniffer) close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.ln.Close()
		s.mu.Lock()
		s.closed = true
		for c := range s.pending {
			_ = c.Close()
		}
		s.mu.Unlock()
	})
	return err
}

func (s *sniffer) loop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			_ = s.close()
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			_ = c.Close() // too many connections still silent
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			<-s.sem
			_ = c.Close()
			continue
		}
		s.pending[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() { <-s.sem }()
			s.route(c)
		})
	}
}

func (s *sniffer) route(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(s.timeout))
	r := bufio.NewReaderSize(c, 1024)
	first, err := r.Peek(1)
	s.mu.Lock()
	delete(s.pending, c)
	closed := s.closed
	s.mu.Unlock()
	if err != nil || closed {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	pc := &peekedConn{Conn: c, r: r}
	target := s.plain
	if first[0] == 0x16 {
		target = s.tls
	}
	select {
	case target.ch <- pc:
	case <-s.done:
		_ = c.Close()
	}
}

// listener is the open Inspector listener with its HTTP servers.
type listener struct {
	addr    net.Addr
	servers []*http.Server
	closers []func() error
	limit   *limitListener
	tr      *tracker
	wg      sync.WaitGroup
}

// bindError turns a bind failure into a user-safe message.
func bindError(addr string, err error) string {
	_, port, _ := net.SplitHostPort(addr)
	switch {
	case errors.Is(err, syscall.EADDRINUSE), strings.Contains(err.Error(), "Only one usage of each socket address"):
		return "the Inspector listener could not be opened: TCP port " + port + " is in use by another program"
	case errors.Is(err, os.ErrPermission), strings.Contains(err.Error(), "forbidden by its access permissions"):
		return "the Inspector listener could not be opened: permission denied for TCP port " + port
	case strings.Contains(err.Error(), "cannot assign requested address"), strings.Contains(err.Error(), "requested address is not valid"):
		return "the Inspector listener could not be opened: the address of --inspector-listen is not an address of this host"
	}
	return "the Inspector listener could not be opened on port " + port
}

// newHTTPServer returns a server whose read and write timeouts (reqTimeout)
// cover everything Go does on a connection, also what it answers before
// any handler runs (a 417 for an unsupported Expect, which then reads the
// announced body). A relayed WebSocket clears the deadlines when it takes
// over the connection (proxyWriter.Hijack).
func newHTTPServer(h http.Handler, logger *slog.Logger, reqTimeout time.Duration) *http.Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &http.Server{
		Handler:                      h,
		ReadTimeout:                  reqTimeout,
		WriteTimeout:                 reqTimeout,
		ReadHeaderTimeout:            min(readHeaderTimeout, reqTimeout),
		IdleTimeout:                  idleTimeout,
		MaxHeaderBytes:               maxHeaderBytes,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
}

// openListener binds addr. With tlsConfig, TLS connections get browser
// (and plaintext connections agent) handling on the same port; without
// it, one plaintext server serves both. Every handler runs under the
// listener's tracker.
func openListener(addr string, tlsConfig *tls.Config, browser, agentOnly http.Handler, sniffTimeout, reqTimeout time.Duration, conns int, logger *slog.Logger) (*listener, error) {
	raw, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln := newLimitListener(raw, conns)
	l := &listener{addr: raw.Addr(), limit: ln, tr: newTracker()}
	browser, agentOnly = l.tr.wrap(browser), l.tr.wrap(agentOnly)
	if tlsConfig == nil {
		srv := newHTTPServer(browser, logger, reqTimeout)
		l.servers = []*http.Server{srv}
		l.closers = []func() error{ln.Close}
		l.wg.Go(func() { _ = srv.Serve(ln) })
		return l, nil
	}
	sn := newSniffer(ln, sniffTimeout)
	tlsSrv := newHTTPServer(browser, logger, reqTimeout)
	tlsSrv.TLSConfig = tlsConfig
	plainSrv := newHTTPServer(agentOnly, logger, reqTimeout)
	l.servers = []*http.Server{tlsSrv, plainSrv}
	l.closers = []func() error{sn.close}
	l.wg.Go(func() { _ = tlsSrv.Serve(tls.NewListener(sn.tls, tlsConfig)) })
	l.wg.Go(func() { _ = plainSrv.Serve(sn.plain) })
	l.wg.Go(func() { sn.wg.Wait() })
	return l, nil
}

// close stops accepting, lets in-flight requests finish and waits for the
// relayed streams (whose requests the caller cancelled before), all within
// listenerShutdownGrace; what is left after it is closed.
func (l *listener) close() {
	ctx, cancel := context.WithTimeout(context.Background(), listenerShutdownGrace)
	defer cancel()
	for _, c := range l.closers {
		_ = c()
	}
	var wg sync.WaitGroup
	for _, srv := range l.servers {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				_ = srv.Close()
			}
		})
	}
	wg.Wait()
	if !l.tr.wait(ctx) {
		l.tr.closeAll()
		wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
		l.tr.wait(wctx)
		wcancel()
	}
	l.wg.Wait()
}

// port returns the bound TCP port.
func (l *listener) port() int {
	if a, ok := l.addr.(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}
