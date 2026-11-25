package testenv

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// Net implements the Network seam for tests (design 0007 D3). Lookups answer
// from a map; dials go to real mock listeners on 127.0.0.1 through routes
// from test addresses. Every lookup and dial is recorded in order.
type Net struct {
	mu        sync.Mutex
	names     map[string]nameEntry
	routes    map[netip.AddrPort]netip.AddrPort
	blackhole map[netip.Addr]bool
	events    []Event
}

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
}

func (e Event) String() string {
	if e.Op == "lookup" {
		return "lookup " + e.Host
	}
	return "dial " + e.Addr.String()
}

// NewNet returns an empty test network.
func NewNet() *Net {
	return &Net{
		names:     map[string]nameEntry{},
		routes:    map[netip.AddrPort]netip.AddrPort{},
		blackhole: map[netip.Addr]bool{},
	}
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

// Blackhole makes dials to addr block until the context ends.
func (n *Net) Blackhole(addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.blackhole[netip.MustParseAddr(addr)] = true
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
// unrouted address is refused; a blackholed one blocks until ctx ends.
func (n *Net) DialContext(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	n.mu.Lock()
	real, ok := n.routes[addr]
	hole := n.blackhole[addr.Addr()]
	n.events = append(n.events, Event{Op: "dial", Addr: addr})
	n.mu.Unlock()
	opErr := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Addr: net.TCPAddrFromAddrPort(addr), Err: err}
	}
	switch {
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
	return c, nil
}

// SelfAddrs returns the injected own address of the test host,
// 192.0.2.50 (design 0007 D3).
func SelfAddrs() []netip.Addr {
	return []netip.Addr{netip.MustParseAddr("192.0.2.50")}
}
