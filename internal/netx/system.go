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

// DialContext opens a TCP connection to an address literal.
func (System) DialContext(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr.String())
}

// InterfaceAddrs returns a function that lists this host's interface
// addresses, refreshed at most once per interval (design 0003 D11).
func InterfaceAddrs(interval time.Duration) func() []netip.Addr {
	var (
		mu      sync.Mutex
		addrs   []netip.Addr
		fetched time.Time
	)
	return func() []netip.Addr {
		mu.Lock()
		defer mu.Unlock()
		if !fetched.IsZero() && time.Since(fetched) < interval {
			return addrs
		}
		fetched = time.Now()
		list, err := net.InterfaceAddrs()
		if err != nil {
			return addrs
		}
		next := make([]netip.Addr, 0, len(list))
		for _, a := range list {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				next = append(next, p.Addr().WithZone("").Unmap())
			}
		}
		addrs = next
		return addrs
	}
}
