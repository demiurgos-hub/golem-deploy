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
	maxSniffing       = 512
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	// listenerShutdownGrace bounds the graceful close of the listener.
	listenerShutdownGrace = 5 * time.Second
)

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
// with a deadline), so a slow client never blocks the accept loop.
type sniffer struct {
	ln        net.Listener
	tls       *chanListener
	plain     *chanListener
	done      chan struct{}
	closeOnce sync.Once
	sem       chan struct{}
	timeout   time.Duration
	wg        sync.WaitGroup
}

func newSniffer(ln net.Listener, timeout time.Duration) *sniffer {
	s := &sniffer{ln: ln, done: make(chan struct{}), sem: make(chan struct{}, maxSniffing), timeout: timeout}
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
	if err != nil {
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

func newHTTPServer(h http.Handler, logger *slog.Logger) *http.Server {
	// No ReadTimeout or WriteTimeout: proxied WebSocket streams live long.
	// Handlers set per-request deadlines for everything else.
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
}

// openListener binds addr. With tlsConfig, TLS connections get browser
// (and plaintext connections agent) handling on the same port; without
// it, one plaintext server serves both.
func openListener(addr string, tlsConfig *tls.Config, browser, agentOnly http.Handler, sniffTimeout time.Duration, logger *slog.Logger) (*listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &listener{addr: ln.Addr()}
	if tlsConfig == nil {
		srv := newHTTPServer(browser, logger)
		l.servers = []*http.Server{srv}
		l.closers = []func() error{ln.Close}
		l.wg.Go(func() { _ = srv.Serve(ln) })
		return l, nil
	}
	sn := newSniffer(ln, sniffTimeout)
	tlsSrv := newHTTPServer(browser, logger)
	tlsSrv.TLSConfig = tlsConfig
	plainSrv := newHTTPServer(agentOnly, logger)
	l.servers = []*http.Server{tlsSrv, plainSrv}
	l.closers = []func() error{sn.close}
	l.wg.Go(func() { _ = tlsSrv.Serve(tls.NewListener(sn.tls, tlsConfig)) })
	l.wg.Go(func() { _ = plainSrv.Serve(sn.plain) })
	l.wg.Go(func() { sn.wg.Wait() })
	return l, nil
}

// close stops accepting, lets in-flight requests finish for a short grace
// and closes the rest. Hijacked (WebSocket) connections are ended by the
// caller by cancelling their requests.
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
	l.wg.Wait()
}

// port returns the bound TCP port.
func (l *listener) port() int {
	if a, ok := l.addr.(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}
