package testenv

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"

	"squid/internal/netx"
)

// Net implements the Network seam for tests (design 0007 D3). Lookups answer
// from a map; dials go to real mock listeners on 127.0.0.1 through routes
// from test addresses. Every lookup and dial is recorded in order, each
// dial with its interface binding. A test can give an interface and
// address its own behavior (design 0008 Interfaces).
type Net struct {
	mu        sync.Mutex
	names     map[string]nameEntry
	routes    map[netip.AddrPort]netip.AddrPort
	blackhole map[netip.Addr]bool
	holePorts map[netip.AddrPort]bool
	limits    map[netip.AddrPort]int
	bound     map[boundKey]Behavior
	events    []Event
}

// boundKey selects the dials bound to one interface: to one address and
// port, to one address on every port, or, with neither, to every address.
type boundKey struct {
	iface string
	addr  netip.Addr
	port  uint16
}

// Behavior is what a dial bound to an interface does (design 0007 D3).
type Behavior struct {
	kind  int
	errno syscall.Errno
}

// Behaviors of a bound dial.
var (
	// Route dials as an unbound dial to the address would.
	Route = Behavior{kind: 1}
	// Refuse fails with ECONNREFUSED.
	Refuse = Behavior{kind: 2, errno: syscall.ECONNREFUSED}
	// Blackhole blocks until the context ends.
	Blackhole = Behavior{kind: 3}
)

// Errno fails a bound dial with errno, as a real connect would report it.
func Errno(e syscall.Errno) Behavior { return Behavior{kind: 2, errno: e} }

type nameEntry struct {
	addrs []netip.Addr
	err   error
	delay time.Duration
}

// Event is one recorded lookup or dial.
type Event struct {
	Op   string // "lookup" or "dial"
	Host string // lookups
	Addr netip.AddrPort
	Err  string
	// Interface is the name of a dial's binding; "" for an unbound dial.
	Interface string
	Index     int
	At        time.Time
}

func (e Event) String() string {
	if e.Op == "lookup" {
		return "lookup " + e.Host
	}
	if e.Interface != "" {
		return "dial " + e.Addr.String() + " via " + e.Interface
	}
	return "dial " + e.Addr.String()
}

// NewNet returns an empty test network.
func NewNet() *Net {
	return &Net{
		names:     map[string]nameEntry{},
		routes:    map[netip.AddrPort]netip.AddrPort{},
		blackhole: map[netip.Addr]bool{},
		holePorts: map[netip.AddrPort]bool{},
		limits:    map[netip.AddrPort]int{},
		bound:     map[boundKey]Behavior{},
	}
}

// OnInterface gives dials bound to the interface named iface a behavior;
// iface "" selects unbound dials. addr is an address and port, an address
// for every port, or "" for every address. The most specific setting
// applies; without one, a bound dial behaves as an unbound dial to its
// address.
func (n *Net) OnInterface(iface, addr string, b Behavior) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.bound[parseBoundKey(iface, addr)] = b
}

// ClearInterface removes every behavior of the interface.
func (n *Net) ClearInterface(iface string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k := range n.bound {
		if k.iface == iface {
			delete(n.bound, k)
		}
	}
}

func parseBoundKey(iface, addr string) boundKey {
	k := boundKey{iface: iface}
	switch {
	case addr == "":
	case netip.MustParseAddr(stripPort(addr)).IsValid() && addr != stripPort(addr):
		ap := netip.MustParseAddrPort(addr)
		k.addr, k.port = ap.Addr(), ap.Port()
	default:
		k.addr = netip.MustParseAddr(addr)
	}
	return k
}

// stripPort returns the address part of "addr:port" or "[addr]:port", or
// addr itself.
func stripPort(s string) string {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().String()
	}
	return s
}

// boundBehavior returns the behavior for a bound dial; the caller holds
// n.mu.
func (n *Net) boundBehavior(iface string, ap netip.AddrPort) (Behavior, bool) {
	for _, k := range []boundKey{{iface, ap.Addr(), ap.Port()}, {iface, ap.Addr(), 0}, {iface: iface}} {
		if b, ok := n.bound[k]; ok {
			return b, true
		}
	}
	return Behavior{}, false
}

// LimitWrites makes each connection dialed to the test address accept at
// most n bytes of writes in total. The write that crosses the limit passes
// only the bytes up to it and fails with ECONNRESET, and the connection
// closes, so a test knows exactly which bytes a socket write accepted.
func (n *Net) LimitWrites(test string, limit int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.limits[netip.MustParseAddrPort(test)] = limit
}

// Map makes name resolve to addrs, in order.
func (n *Net) Map(name string, addrs ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.names[name]
	e.addrs = nil
	for _, a := range addrs {
		e.addrs = append(e.addrs, netip.MustParseAddr(a))
	}
	n.names[name] = e
}

// MapError makes lookups of name fail with err.
func (n *Net) MapError(name string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.names[name]
	e.err = err
	n.names[name] = e
}

// Delay delays lookups of name.
func (n *Net) Delay(name string, d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.names[name]
	e.delay = d
	n.names[name] = e
}

// Route sends dials to the test address (such as "203.0.113.10:443") to a
// real listener.
func (n *Net) Route(test string, real netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[netip.MustParseAddrPort(test)] = real
}

// Blackhole makes dials to addr block until the context ends. addr is an
// address, which covers every port, or an address and port.
func (n *Net) Blackhole(addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		n.holePorts[ap] = true
		return
	}
	n.blackhole[netip.MustParseAddr(addr)] = true
}

// Unblackhole undoes Blackhole for the same addr, so dials follow their
// routes again.
func (n *Net) Unblackhole(addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		delete(n.holePorts, ap)
		return
	}
	delete(n.blackhole, netip.MustParseAddr(addr))
}

// Events returns the recorded lookups and dials in order.
func (n *Net) Events() []Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Event(nil), n.events...)
}

// Lookups returns the names looked up, in order.
func (n *Net) Lookups() []string {
	var out []string
	for _, e := range n.Events() {
		if e.Op == "lookup" {
			out = append(out, e.Host)
		}
	}
	return out
}

// DialEvents returns the recorded dials in order.
func (n *Net) DialEvents() []Event {
	var out []Event
	for _, e := range n.Events() {
		if e.Op == "dial" {
			out = append(out, e)
		}
	}
	return out
}

// Dials returns the addresses dialed, in order.
func (n *Net) Dials() []netip.AddrPort {
	var out []netip.AddrPort
	for _, e := range n.Events() {
		if e.Op == "dial" {
			out = append(out, e.Addr)
		}
	}
	return out
}

// Reset clears the recorded events.
func (n *Net) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = nil
}

// LookupNetIP answers from the map. An unknown name returns a
// *net.DNSError with IsNotFound.
func (n *Net) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	e, ok := n.names[host]
	n.events = append(n.events, Event{Op: "lookup", Host: host})
	n.mu.Unlock()
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return nil, &net.DNSError{Err: "timeout", Name: host, IsTimeout: true}
		}
	}
	switch {
	case e.err != nil:
		return nil, e.err
	case !ok || len(e.addrs) == 0:
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return append([]netip.Addr(nil), e.addrs...), nil
}

// DialContext connects a routed test address to its real listener. An
// unrouted address is refused; a blackholed one blocks until ctx ends. A
// dial bound to an interface with its own behavior follows that behavior
// instead.
func (n *Net) DialContext(ctx context.Context, addr netip.AddrPort, b netx.Binding) (net.Conn, error) {
	n.mu.Lock()
	real, ok := n.routes[addr]
	limit, limited := n.limits[addr]
	hole := n.blackhole[addr.Addr()] || n.holePorts[addr]
	var fail syscall.Errno
	if bh, set := n.boundBehavior(b.Name, addr); set {
		switch bh.kind {
		case 2:
			fail = bh.errno
		case 3:
			hole = true
		}
	}
	n.events = append(n.events, Event{Op: "dial", Addr: addr, Interface: b.Name, Index: b.Index, At: time.Now()})
	n.mu.Unlock()
	opErr := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Addr: net.TCPAddrFromAddrPort(addr), Err: err}
	}
	switch {
	case fail != 0:
		return nil, opErr(&os.SyscallError{Syscall: "connect", Err: fail})
	case hole:
		<-ctx.Done()
		return nil, opErr(ctx.Err())
	case !ok:
		return nil, opErr(syscall.ECONNREFUSED)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", real.String())
	if err != nil {
		return nil, opErr(fmt.Errorf("route to %s: %w", real, err))
	}
	if limited {
		return &limitConn{Conn: c, left: limit}, nil
	}
	return c, nil
}

// limitConn accepts a limited number of written bytes (Net.LimitWrites).
type limitConn struct {
	net.Conn
	mu   sync.Mutex
	left int
}

func (c *limitConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(b) <= c.left {
		n, err := c.Conn.Write(b)
		c.left -= n
		return n, err
	}
	n, err := c.Conn.Write(b[:c.left])
	c.left -= n
	if err == nil {
		err = &net.OpError{Op: "write", Net: "tcp", Addr: c.RemoteAddr(), Err: syscall.ECONNRESET}
	}
	c.Conn.Close()
	return n, err
}

// SelfAddrs returns the injected own address of the test host,
// 192.0.2.50 (design 0007 D3).
func SelfAddrs() []netip.Addr {
	return []netip.Addr{netip.MustParseAddr("192.0.2.50")}
}
