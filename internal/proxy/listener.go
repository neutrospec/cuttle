package proxy

import (
	"errors"
	"net"
	"net/netip"
	"sync/atomic"

	"squid/internal/policy"
	"squid/internal/stats"
)

// listener classifies each source on accept, before net/http reads a byte
// (design 0003 D10). In M1.1 only loopback sources are served: every other
// source is closed at once and counted with reason "source". The remote
// allowlist arrives in M1.2.
type listener struct {
	net.Listener
	p *Proxy
}

type connKey struct{}

var connSeq atomic.Uint64

func (l *listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := remoteAddr(c)
		if !peer.Addr().IsLoopback() {
			l.p.live.Add(1, stats.ConnectionsRejectedTotal, "reason", "source")
			l.p.warnSource(peer.Addr())
			c.Close()
			continue
		}
		l.p.live.Add(1, stats.ConnectionsTotal)
		l.p.live.Add(1, stats.ConnectionsActive)
		return &trackedConn{Conn: c, id: connSeq.Add(1), source: policy.SourceLocal, live: l.p.live}, nil
	}
}

func remoteAddr(c net.Conn) netip.AddrPort {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ap := ta.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return netip.AddrPort{}
}

// trackedConn is an accepted client connection with its source class. It
// decrements connections_active once when closed, including after a
// hijack.
type trackedConn struct {
	net.Conn
	id     uint64
	source policy.SourceClass
	live   *stats.Live
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.live.Add(-1, stats.ConnectionsActive)
	}
	return c.Conn.Close()
}

// CloseWrite half-closes the connection when the underlying one supports it.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}
