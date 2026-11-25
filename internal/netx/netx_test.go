package netx

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"squid/internal/authority"
	"squid/internal/policy"
)

func TestClassify(t *testing.T) {
	self := []netip.Addr{netip.MustParseAddr("192.0.2.50"), netip.MustParseAddr("192.168.1.20")}
	for _, c := range []struct {
		addr string
		want Class
	}{
		{"0.0.0.0", ClassUnusable},
		{"0.1.2.3", ClassUnusable},
		{"::", ClassUnusable},
		{"169.254.1.1", ClassUnusable},
		{"fe80::1", ClassUnusable},
		{"224.0.0.1", ClassUnusable},
		{"ff02::1", ClassUnusable},
		{"240.0.0.1", ClassUnusable},
		{"255.255.255.255", ClassUnusable},
		{"127.0.0.1", ClassLoopback},
		{"127.255.0.9", ClassLoopback},
		{"::1", ClassLoopback},
		{"::ffff:127.0.0.1", ClassLoopback},
		{"64:ff9b::7f00:1", ClassLoopback},
		{"64:ff9b::a9fe:101", ClassUnusable},
		{"192.0.2.50", ClassSelf},
		{"::ffff:192.0.2.50", ClassSelf},
		{"192.168.1.20", ClassSelf}, // self precedes private
		{"10.20.0.5", ClassPrivate},
		{"172.16.0.1", ClassPrivate},
		{"172.31.255.255", ClassPrivate},
		{"172.32.0.1", ClassPublic},
		{"192.168.0.1", ClassPrivate},
		{"100.64.0.1", ClassPrivate},
		{"100.127.255.255", ClassPrivate},
		{"100.128.0.1", ClassPublic},
		{"fd00::1", ClassPrivate},
		{"64:ff9b::a14:5", ClassPrivate},
		{"203.0.113.10", ClassPublic},
		{"2001:db8::1", ClassPublic},
		{"198.51.100.1", ClassPublic},
	} {
		if got := Classify(netip.MustParseAddr(c.addr), self); got != c.want {
			t.Errorf("Classify(%s) = %s, want %s", c.addr, got, c.want)
		}
	}
}

func TestAllowedTable(t *testing.T) {
	plain := policy.Decision{}
	lb := policy.Decision{AllowLoopback: true}
	pv := policy.Decision{AllowPrivate: true}
	both := policy.Decision{AllowLoopback: true, AllowPrivate: true}
	L, R := policy.SourceLocal, policy.SourceRemote
	for _, c := range []struct {
		class   Class
		src     policy.SourceClass
		literal bool
		d       policy.Decision
		want    bool
	}{
		{ClassUnusable, L, true, both, false},
		{ClassUnusable, L, false, both, false},
		{ClassLoopback, L, true, plain, false},
		{ClassLoopback, L, true, lb, true},
		{ClassLoopback, L, false, lb, true},
		{ClassLoopback, L, false, pv, false},
		{ClassSelf, L, true, plain, true},
		{ClassSelf, L, false, plain, false},
		{ClassSelf, L, false, lb, true},
		{ClassSelf, L, false, pv, false},
		{ClassPrivate, L, true, plain, true},
		{ClassPrivate, L, false, plain, false},
		{ClassPrivate, L, false, pv, true},
		{ClassPrivate, L, false, lb, false},
		{ClassPublic, L, false, plain, true},
		{ClassPublic, R, false, plain, true},
		{ClassPrivate, R, true, both, false},
		{ClassSelf, R, true, both, false},
		{ClassLoopback, R, true, both, false},
	} {
		if got := Allowed(c.class, c.src, c.literal, c.d); got != c.want {
			t.Errorf("Allowed(%s, %s, literal=%v, %+v) = %v", c.class, c.src, c.literal, c.d, got)
		}
	}
}

// fakeNet answers lookups from a map and dials by address behavior.
type fakeNet struct {
	mu    sync.Mutex
	names map[string][]netip.Addr
	dial  map[netip.AddrPort]string // "ok", "refused", "hang"
	dials []netip.AddrPort
}

func (f *fakeNet) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, ok := f.names[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fakeNet) DialContext(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	f.mu.Lock()
	f.dials = append(f.dials, ap)
	mode := f.dial[ap]
	f.mu.Unlock()
	switch mode {
	case "ok":
		c1, c2 := net.Pipe()
		go func() { <-ctx.Done(); _ = c2 }()
		return c1, nil
	case "hang":
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func TestDialerPolicyAndErrors(t *testing.T) {
	fn := &fakeNet{
		names: map[string][]netip.Addr{
			"mixed.example": {netip.MustParseAddr("10.20.0.5"), netip.MustParseAddr("203.0.113.10")},
			"lo.example":    {netip.MustParseAddr("127.0.0.1")},
			"slow.example":  {netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("203.0.113.21")},
			"dead.example":  {netip.MustParseAddr("203.0.113.30")},
		},
		dial: map[netip.AddrPort]string{
			netip.MustParseAddrPort("203.0.113.10:443"): "ok",
			netip.MustParseAddrPort("203.0.113.20:443"): "hang",
			netip.MustParseAddrPort("203.0.113.21:443"): "ok",
			netip.MustParseAddrPort("203.0.113.30:443"): "hang",
		},
	}
	dl := &Dialer{Network: fn, Timeout: 2 * time.Second, Stagger: 50 * time.Millisecond}
	dec := func(target string) policy.Decision {
		a, err := authority.ParseConnect(target)
		if err != nil {
			t.Fatal(err)
		}
		return policy.Decision{Target: a, Source: policy.SourceLocal}
	}

	// A name with a private and a public address is dialed on the public one.
	c, err := dl.Dial(context.Background(), dec("mixed.example:443"))
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	c.Close()
	if len(fn.dials) != 1 || fn.dials[0] != netip.MustParseAddrPort("203.0.113.10:443") {
		t.Fatalf("mixed dials = %v", fn.dials)
	}

	// A name resolving to loopback is refused without a dial.
	fn.dials = nil
	_, err = dl.Dial(context.Background(), dec("lo.example:443"))
	var de *DialError
	if !errors.As(err, &de) || de.Type != ErrProhibited || len(fn.dials) != 0 {
		t.Fatalf("lo: err %v dials %v", err, fn.dials)
	}

	// Unknown names are DNS errors.
	_, err = dl.Dial(context.Background(), dec("unknown.example:443"))
	if !errors.As(err, &de) || de.Type != ErrDNS {
		t.Fatalf("unknown: %v", err)
	}

	// The second address starts after the stagger and wins.
	fn.dials = nil
	start := time.Now()
	c, err = dl.Dial(context.Background(), dec("slow.example:443"))
	if err != nil {
		t.Fatalf("slow: %v", err)
	}
	c.Close()
	if el := time.Since(start); el < 40*time.Millisecond || el > time.Second {
		t.Errorf("slow: took %v", el)
	}

	// A refused literal is connection_refused; a hanging one times out.
	_, err = dl.Dial(context.Background(), dec("203.0.113.99:443"))
	if !errors.As(err, &de) || de.Type != ErrRefused {
		t.Fatalf("refused: %v", err)
	}
	dl.Timeout = 200 * time.Millisecond
	_, err = dl.Dial(context.Background(), dec("dead.example:443"))
	if !errors.As(err, &de) || de.Type != ErrTimeout {
		t.Fatalf("timeout: %v", err)
	}
}
