package netx

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"
)

// System is the operating system's resolver and TCP dialer. Only cmd/cuttle
// uses it; tests pass internal/testenv.Net instead.
type System struct {
	Resolver *net.Resolver
}

// LookupNetIP resolves a host name to its IP addresses.
func (s System) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	r := s.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	return r.LookupNetIP(ctx, "ip", host)
}

// DialContext opens a TCP connection to an address literal. A Binding with
// an index binds the socket to that interface before connect (design 0008
// D7).
func (System) DialContext(ctx context.Context, addr netip.AddrPort, b Binding) (net.Conn, error) {
	var d net.Dialer
	if b.Index != 0 {
		d.Control = BindControl(b.Index)
	}
	return d.DialContext(ctx, "tcp", addr.String())
}

// InterfaceAddrs returns a function that lists this host's interface
// addresses, refreshed at most once per interval (design 0003 D11), and a
// function that makes the next call read them again (design 0008 D5).
func InterfaceAddrs(interval time.Duration) (addrs func() []netip.Addr, invalidate func()) {
	var (
		mu      sync.Mutex
		list    []netip.Addr
		fetched time.Time
	)
	addrs = func() []netip.Addr {
		mu.Lock()
		defer mu.Unlock()
		if !fetched.IsZero() && time.Since(fetched) < interval {
			return list
		}
		fetched = time.Now()
		ifaddrs, err := net.InterfaceAddrs()
		if err != nil {
			return list
		}
		next := make([]netip.Addr, 0, len(ifaddrs))
		for _, a := range ifaddrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				next = append(next, p.Addr().WithZone("").Unmap())
			}
		}
		list = next
		return list
	}
	invalidate = func() {
		mu.Lock()
		defer mu.Unlock()
		fetched = time.Time{}
	}
	return addrs, invalidate
}
