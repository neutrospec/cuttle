package proxy

import (
	"net"
	"net/netip"
	"testing"

	"github.com/neutrospec/cuttle/internal/policy"
	"github.com/neutrospec/cuttle/internal/stats"
	"github.com/neutrospec/cuttle/internal/testenv"
)

// queueListener hands out the given connections, then reports closed.
type queueListener struct {
	conns []net.Conn
}

func (l *queueListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, net.ErrClosed
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}

func (l *queueListener) Close() error   { return nil }
func (l *queueListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

// peerPipe is one end of a pipe that reports peer as its remote address,
// as a TCP connection from peer would.
type peerPipe struct {
	net.Conn
	peer   *net.TCPAddr
	closed bool
}

func (c *peerPipe) RemoteAddr() net.Addr { return c.peer }
func (c *peerPipe) Close() error         { c.closed = true; return c.Conn.Close() }

// TestSourceClassification checks how listener.Accept classifies peers
// (design 0003 D10): addresses are unmapped first, so IPv4-mapped peers
// are classified by their IPv4 address, and every peer with an IPv6 zone
// is refused, a loopback one included and even when its prefix is
// allowlisted.
func TestSourceClassification(t *testing.T) {
	cfg := mustParse(t, `name = "cuttle-test"
[proxy]
default = "block"
[admin]
listen = ""
[proxy.remote]
sources = ["10.8.0.1/32", "fe80::/10", "2001:db8::1/128"]
`)
	p, err := New(cfg, Deps{Network: testenv.NewNet(), SelfAddrs: testenv.SelfAddrs, Listeners: []net.Listener{idleListener{}}})
	if err != nil {
		t.Fatal(err)
	}
	const rejected = -1
	for _, c := range []struct {
		peer  string
		class int // a policy.SourceClass, or rejected
		addr  string
	}{
		{"127.0.0.1", int(policy.SourceLocal), "127.0.0.1"},
		{"::ffff:127.0.0.1", int(policy.SourceLocal), "127.0.0.1"},
		{"::1", int(policy.SourceLocal), "::1"},
		{"10.8.0.1", int(policy.SourceRemote), "10.8.0.1"},
		{"::ffff:10.8.0.1", int(policy.SourceRemote), "10.8.0.1"},
		{"2001:db8::1", int(policy.SourceRemote), "2001:db8::1"},
		{"10.8.0.2", rejected, ""},
		{"::ffff:10.8.0.2", rejected, ""},
		{"198.51.100.7", rejected, ""},
		{"fe80::1%lo0", rejected, ""},
		{"::1%lo0", rejected, ""}, // netip calls it loopback; every zoned peer is refused
		{"fe80::1", int(policy.SourceRemote), "fe80::1"},
	} {
		server, client := net.Pipe()
		conn := &peerPipe{Conn: server, peer: net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(c.peer), 40000))}
		before := p.Live().Get(stats.ConnectionsRejectedTotal, "reason", "source")
		got, err := (&listener{Listener: &queueListener{conns: []net.Conn{conn}}, p: p}).Accept()
		if c.class == rejected {
			if err != net.ErrClosed || !conn.closed || p.Live().Get(stats.ConnectionsRejectedTotal, "reason", "source") != before+1 {
				t.Errorf("%s: accepted (%v, %v), want rejected", c.peer, got, err)
			}
		} else {
			tc, ok := got.(*trackedConn)
			if err != nil || !ok || int(tc.source) != c.class || tc.peer != netip.MustParseAddr(c.addr) {
				t.Errorf("%s: got %+v %v, want class %d peer %s", c.peer, got, err, c.class, c.addr)
			}
			if ok {
				tc.Close()
			}
		}
		client.Close()
		server.Close()
	}
}
