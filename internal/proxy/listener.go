package proxy

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/policy"
	"github.com/neutrospec/cuttle/internal/stats"
)

// listener classifies each source on accept, before net/http reads a byte
// (design 0003 D10), and applies the connection limits (D12). Loopback
// sources are local and sources inside proxy.remote.sources are remote.
// Every other source, and a connection over a limit, is closed at once
// without a byte read or written and counted in
// connections_rejected_total with reason "source" or "limit".
type listener struct {
	net.Listener
	p *Proxy
}

type connKey struct{}

var connSeq atomic.Uint64

func (l *listener) Accept() (net.Conn, error) {
	p := l.p
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		// The current snapshot holds the allowlist and the limits.
		cfg := p.snap.Load().cfg
		peer := remoteAddr(c).Addr()
		src, ok := classify(peer, cfg.Proxy.Remote.Sources)
		if !ok {
			c.Close()
			p.live.Add(1, stats.ConnectionsRejectedTotal, "reason", "source")
			p.warnSource(peer)
			continue
		}
		if key, limit, ok := p.conns.admit(peer, src, cfg); !ok {
			c.Close()
			p.live.Add(1, stats.ConnectionsRejectedTotal, "reason", "limit")
			p.warnLimit(peer, key, limit)
			continue
		}
		p.live.Add(1, stats.ConnectionsTotal)
		p.live.Add(1, stats.ConnectionsActive)
		return &trackedConn{Conn: c, id: connSeq.Add(1), source: src, peer: peer, p: p}, nil
	}
}

// classify returns the source class of a peer address, which must already
// be unmapped. Every address with an IPv6 zone is refused first, even one
// that netip reports as loopback, such as ::1%lo0. Then loopback addresses
// are local, addresses inside sources are remote, and anything else, an
// invalid address included, is refused.
func classify(peer netip.Addr, sources []netip.Prefix) (policy.SourceClass, bool) {
	if !peer.IsValid() || peer.Zone() != "" {
		return 0, false
	}
	if peer.IsLoopback() {
		return policy.SourceLocal, true
	}
	for _, s := range sources {
		if s.Contains(peer) {
			return policy.SourceRemote, true
		}
	}
	return 0, false
}

func remoteAddr(c net.Conn) netip.AddrPort {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ap := ta.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return netip.AddrPort{}
}

// connLimits counts open client connections for proxy.max_connections, in
// total, and proxy.remote.max_connections_per_source, per remote source
// address (design 0003 D12).
type connLimits struct {
	mu        sync.Mutex
	active    int
	perSource map[netip.Addr]int
}

// admit reserves a slot for a new connection. When a limit is reached it
// returns the limit's key and value and false.
func (l *connLimits) admit(peer netip.Addr, src policy.SourceClass, cfg *config.Config) (string, int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit := cfg.Proxy.MaxConnections; l.active >= limit {
		return "proxy.max_connections", limit, false
	}
	if src == policy.SourceRemote {
		if limit := cfg.Proxy.Remote.MaxConnectionsPerSource; l.perSource[peer] >= limit {
			return "proxy.remote.max_connections_per_source", limit, false
		}
		if l.perSource == nil {
			l.perSource = map[netip.Addr]int{}
		}
		l.perSource[peer]++
	}
	l.active++
	return "", 0, true
}

// release frees the slot of a closed connection.
func (l *connLimits) release(peer netip.Addr, src policy.SourceClass) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if src == policy.SourceRemote {
		if l.perSource[peer]--; l.perSource[peer] <= 0 {
			delete(l.perSource, peer)
		}
	}
}

// trackedConn is an accepted client connection with its source class. When
// first closed, including after a hijack, it decrements connections_active
// and frees its connection-limit slot.
type trackedConn struct {
	net.Conn
	id     uint64
	source policy.SourceClass
	peer   netip.Addr
	p      *Proxy
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.p.live.Add(-1, stats.ConnectionsActive)
		c.p.conns.release(c.peer, c.source)
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
