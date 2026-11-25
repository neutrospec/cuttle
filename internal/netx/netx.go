// Package netx holds the Network seam, the destination address classes, the
// direct dialer that applies the destination address policy (design 0003
// D11), and the upstream dialer, to which that policy does not apply (D1).
// Internal packages have no default Network: cmd/cuttle wires System, and
// tests pass internal/testenv.Net, so a test cannot reach real DNS by
// accident (design 0007 D3).
package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"

	"squid/internal/authority"
	"squid/internal/policy"
)

// Network is the only path from the proxy to name resolution and sockets.
type Network interface {
	LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
	DialContext(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
}

// Class is a destination address class. Classes are tried in the order
// declared here; the first that matches wins.
type Class int

const (
	ClassUnusable Class = iota
	ClassLoopback
	ClassSelf
	ClassPrivate
	ClassPublic
)

func (c Class) String() string {
	switch c {
	case ClassUnusable:
		return "unusable"
	case ClassLoopback:
		return "loopback"
	case ClassSelf:
		return "self"
	case ClassPrivate:
		return "private"
	default:
		return "public"
	}
}

var (
	unusable = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("::/128"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("ff00::/8"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
	loopback = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	private = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("fc00::/7"),
	}
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
)

// Embedded returns the address to classify: IPv4-mapped addresses are
// unmapped, and an address in the NAT64 prefix 64:ff9b::/96 yields its
// embedded IPv4 address.
func Embedded(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if nat64.Contains(a) {
		b := a.As16()
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return a
}

// Classify returns the class of an address. self lists this host's own
// addresses.
func Classify(a netip.Addr, self []netip.Addr) Class {
	a = Embedded(a.WithZone(""))
	switch {
	case containsAny(unusable, a):
		return ClassUnusable
	case containsAny(loopback, a):
		return ClassLoopback
	case slices.ContainsFunc(self, func(s netip.Addr) bool { return s.Unmap() == a }):
		return ClassSelf
	case containsAny(private, a):
		return ClassPrivate
	}
	return ClassPublic
}

func containsAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Allowed reports whether a direct route may reach an address of class c
// (design 0003 D11 table). literal is true when the client named the address
// as an IP literal rather than a name.
func Allowed(c Class, src policy.SourceClass, literal bool, d policy.Decision) bool {
	if src == policy.SourceRemote {
		return c == ClassPublic
	}
	switch c {
	case ClassUnusable:
		return false
	case ClassLoopback:
		return d.AllowLoopback
	case ClassSelf:
		return literal || d.AllowLoopback
	case ClassPrivate:
		return literal || d.AllowPrivate
	default:
		return true
	}
}

// Error types reported in Proxy-Status (design 0002 D6).
const (
	ErrDNS        = "dns_error"
	ErrProhibited = "destination_ip_prohibited"
	ErrRefused    = "connection_refused"
	ErrTimeout    = "connection_timeout"
	// ErrLoop marks an upstream whose resolved address is one of this
	// instance's own listeners (design 0003 D1).
	ErrLoop = "proxy_loop_detected"
)

// DialError is a direct-route or upstream dial failure with its
// Proxy-Status error type.
type DialError struct {
	Type   string // one of the Err constants
	Detail string // safe to log: addresses and classes, never request content
	Err    error
	// ClientGone reports that the dial was abandoned because the request's
	// context was canceled.
	ClientGone bool
}

func (e *DialError) Error() string { return e.Type + ": " + e.Detail }
func (e *DialError) Unwrap() error { return e.Err }

// Dialer resolves and dials direct routes.
type Dialer struct {
	Network Network
	// SelfAddrs returns this host's interface addresses.
	SelfAddrs func() []netip.Addr
	// Timeout bounds the lookup and all connection attempts together.
	Timeout time.Duration
	// Stagger is the delay before the next address is tried (RFC 8305).
	Stagger time.Duration
}

// Dial resolves the decision's target, drops addresses the address policy
// refuses, and dials the rest in the resolver's order.
func (dl *Dialer) Dial(ctx context.Context, d policy.Decision) (net.Conn, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, dl.Timeout)
	defer cancel()
	gone := func() bool { return parent.Err() != nil && !errors.Is(parent.Err(), context.DeadlineExceeded) }

	t := d.Target
	var addrs []netip.Addr
	literal := t.IsIP()
	if literal {
		addrs = []netip.Addr{t.Addr}
	} else {
		var err error
		addrs, err = dl.Network.LookupNetIP(ctx, t.Host)
		if err != nil {
			return nil, &DialError{Type: ErrDNS, Detail: "lookup failed: " + lookupReason(err), Err: err, ClientGone: gone()}
		}
		if len(addrs) == 0 {
			return nil, &DialError{Type: ErrDNS, Detail: "lookup returned no address"}
		}
	}
	var self []netip.Addr
	if dl.SelfAddrs != nil {
		self = dl.SelfAddrs()
	}
	var allowed []netip.AddrPort
	var refused []string
	for _, a := range addrs {
		c := Classify(a, self)
		if !Allowed(c, d.Source, literal, d) {
			if !slices.Contains(refused, c.String()) {
				refused = append(refused, c.String())
			}
			continue
		}
		// A NAT64 address is classified by its embedded IPv4 address but
		// dialed as is, through the NAT64 gateway, to the same destination.
		allowed = append(allowed, netip.AddrPortFrom(a.WithZone("").Unmap(), t.Port))
	}
	if len(allowed) == 0 {
		return nil, &DialError{Type: ErrProhibited, Detail: "refused address classes: " + strings.Join(refused, ",")}
	}
	conn, err := dialAll(ctx, dl.Network, dl.Stagger, allowed)
	if err != nil {
		return nil, connectError(err, dl.Timeout, gone())
	}
	return conn, nil
}

// connectError maps a failed connection attempt to its error type.
func connectError(err error, timeout time.Duration, clientGone bool) *DialError {
	de := &DialError{Type: ErrRefused, Err: err, ClientGone: clientGone}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		de.Type = ErrTimeout
		de.Detail = fmt.Sprintf("connect timed out after %s", timeout)
	} else {
		de.Detail = "connect failed: " + errReason(err)
	}
	return de
}

// UpstreamDialer resolves and dials an upstream proxy's own address
// (design 0003 D1). The destination address policy does not apply, because
// upstreams are often on loopback or private addresses. An address equal to
// one of this instance's listeners is refused as a loop.
type UpstreamDialer struct {
	Network Network
	// Timeout bounds the lookup and all connection attempts together.
	Timeout time.Duration
	// Stagger is the delay before the next address is tried (RFC 8305).
	Stagger time.Duration
	// Self reports whether an address is one of this instance's bound
	// listeners.
	Self func(netip.AddrPort) bool
}

// Dial resolves the upstream address and dials it. A failure is a
// *DialError of type ErrDNS, ErrRefused, ErrTimeout, or ErrLoop.
func (dl *UpstreamDialer) Dial(ctx context.Context, t authority.Authority) (net.Conn, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, dl.Timeout)
	defer cancel()
	gone := func() bool { return parent.Err() != nil && !errors.Is(parent.Err(), context.DeadlineExceeded) }

	var addrs []netip.Addr
	if t.IsIP() {
		addrs = []netip.Addr{t.Addr}
	} else {
		var err error
		addrs, err = dl.Network.LookupNetIP(ctx, t.Host)
		if err != nil {
			return nil, &DialError{Type: ErrDNS, Detail: "upstream lookup failed: " + lookupReason(err), Err: err, ClientGone: gone()}
		}
		if len(addrs) == 0 {
			return nil, &DialError{Type: ErrDNS, Detail: "upstream lookup returned no address"}
		}
	}
	targets := make([]netip.AddrPort, 0, len(addrs))
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a.WithZone("").Unmap(), t.Port)
		if dl.Self != nil && dl.Self(ap) {
			return nil, &DialError{Type: ErrLoop, Detail: "upstream address " + ap.String() + " is this instance's own listener"}
		}
		targets = append(targets, ap)
	}
	conn, err := dialAll(ctx, dl.Network, dl.Stagger, targets)
	if err != nil {
		return nil, connectError(err, dl.Timeout, gone())
	}
	return conn, nil
}

// dialAll starts the next address stagger after the previous one unless it
// has connected, and returns the first connection that succeeds.
func dialAll(ctx context.Context, network Network, stagger time.Duration, addrs []netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(addrs))
	started, finished := 0, 0
	var firstErr error
	start := func() {
		a := addrs[started]
		started++
		go func() {
			c, err := network.DialContext(ctx, a)
			results <- result{c, err}
		}()
	}
	start()
	if stagger <= 0 {
		stagger = 300 * time.Millisecond
	}
	timer := time.NewTimer(stagger)
	defer timer.Stop()
	for {
		select {
		case r := <-results:
			finished++
			if r.err == nil {
				cancel()
				// Close any later winners.
				go func(pending int) {
					for range pending {
						if late := <-results; late.c != nil {
							late.c.Close()
						}
					}
				}(started - finished)
				return r.c, nil
			}
			if firstErr == nil {
				firstErr = r.err
			}
			if started < len(addrs) {
				start()
				timer.Reset(stagger)
			} else if finished == started {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, context.DeadlineExceeded
				}
				return nil, firstErr
			}
		case <-timer.C:
			if started < len(addrs) {
				start()
				timer.Reset(stagger)
			}
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func lookupReason(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return "no such host"
		case de.IsTimeout:
			return "timeout"
		}
		return "server failure"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "error"
}

func errReason(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}
