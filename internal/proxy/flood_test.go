package proxy

import (
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"squid/internal/stats"
	"squid/internal/testenv"
)

// floodListener reports a new peer address of 198.51.100.0/24, the
// rejected sources of the test address plan (design 0007 D3), for each
// accepted connection.
type floodListener struct {
	net.Listener
	n atomic.Int32
}

func (l *floodListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	i := l.n.Add(1)
	peer := netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}), 40000)
	return &floodConn{Conn: c, remote: net.TCPAddrFromAddrPort(peer)}, nil
}

type floodConn struct {
	net.Conn
	remote net.Addr
}

func (c *floodConn) RemoteAddr() net.Addr { return c.remote }

// TestRejectedSourceFlood checks that connections from many distinct
// rejected sources produce a bounded number of log lines (design 0003 D10,
// D12): every connection is rejected and counted, but at most 60 warnings
// are written per minute, followed by one summary line with the number
// suppressed, written at the latest when the proxy stops.
func TestRejectedSourceFlood(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	const flood = 200
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	p, err := New(mustParse(t, remoteConfig), Deps{
		Network:   testenv.NewNet(),
		SelfAddrs: testenv.SelfAddrs,
		Listeners: []net.Listener{&floodListener{Listener: ln}},
		Logger:    newTestLogger(logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { p.Serve(); close(done) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			p.Close()
			<-done
		}
	}
	defer stop()

	rejected := func() int64 { return p.Live().Get(stats.ConnectionsRejectedTotal, "reason", "source") }
	for i := 1; i <= flood; i++ {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "rejection", func() bool { return rejected() == int64(i) })
		c.Close()
	}
	stop()

	out := logs.String()
	if got := strings.Count(out, `msg="rejected source"`); got != 60 {
		t.Errorf("%d rejected-source warnings for %d distinct sources, want 60", got, flood)
	}
	if got := strings.Count(out, `msg="warnings suppressed"`); got != 1 || !strings.Contains(out, `msg="warnings suppressed" count=140 `) {
		t.Errorf("want one summary with count=140, have %d:\n%s", got, out)
	}
	if len(logs.decisions()) != 0 || p.Live().Get(stats.ConnectionsTotal) != 0 {
		t.Errorf("a rejected source was served")
	}
}
