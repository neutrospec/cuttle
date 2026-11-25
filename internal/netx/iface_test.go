package netx

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"squid/internal/authority"
	"squid/internal/iface"
	"squid/internal/policy"
)

// fakeSource is an interface source with fixed rows and no notices.
type fakeSource struct{ rows []iface.Link }

func (f fakeSource) Read() ([]iface.Link, error)               { return f.rows, nil }
func (f fakeSource) Watch(ctx context.Context) <-chan struct{} { return make(chan struct{}) }

// noTimers never fires, so a test sees only the reads it causes.
type noTimers struct{}

func (noTimers) AfterFunc(time.Duration, func()) func() bool { return func() bool { return true } }

func mac(last byte) net.HardwareAddr { return net.HardwareAddr{0, 0, 0x5e, 0, 0x53, last} }

func link(index int, device string, last byte, addrs ...string) iface.Link {
	l := iface.Link{Index: index, Device: device, MAC: mac(last), Up: true, Kind: iface.KindWired,
		Media: iface.MediaActive, Default4: iface.RouteScoped}
	for _, a := range addrs {
		l.Addrs = append(l.Addrs, netip.MustParseAddr(a))
	}
	return l
}

// testMonitor returns a monitor for eth-a (IPv4 only), eth-b (both
// families), and wifi, each online unless changed.
func testMonitor(t *testing.T, change func([]iface.Link)) *iface.Monitor {
	t.Helper()
	rows := []iface.Link{
		link(7, "en7", 0x0a, "198.18.0.7"),
		link(8, "en8", 0x0b, "198.18.0.8", "2001:db8::8"),
		link(4, "en0", 0x01, "198.18.0.4"),
	}
	rows[1].Default6 = iface.RouteScoped
	if change != nil {
		change(rows)
	}
	m := iface.NewMonitor(iface.Options{Source: fakeSource{rows}, Timers: noTimers{}})
	defs := []iface.Def{{Name: "eth-a", MAC: mac(0x0a)}, {Name: "eth-b", MAC: mac(0x0b)}, {Name: "wifi", Device: "en0"}}
	if err := m.Configure(defs, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m
}

// bnet is a test network whose dials depend on the binding and address:
// "ok", "hang", or an errno. Unset pairs connect.
type bnet struct {
	mu    sync.Mutex
	names map[string][]netip.Addr
	modes map[string]any // "iface addr" or " addr" -> "hang" or error
	dials []string       // "iface addr" in order
	times []time.Time
}

func newBnet() *bnet { return &bnet{names: map[string][]netip.Addr{}, modes: map[string]any{}} }

func (n *bnet) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, ok := n.names[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (n *bnet) set(ifname, addr string, mode any) { n.modes[ifname+" "+addr] = mode }

func (n *bnet) DialContext(ctx context.Context, ap netip.AddrPort, b Binding) (net.Conn, error) {
	key := b.Name + " " + ap.Addr().String()
	n.mu.Lock()
	n.dials = append(n.dials, key)
	n.times = append(n.times, time.Now())
	mode := n.modes[key]
	n.mu.Unlock()
	switch m := mode.(type) {
	case string:
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	case error:
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: m}}
	}
	c1, c2 := net.Pipe()
	go func() { <-time.After(time.Second); c2.Close() }()
	return c1, nil
}

func (n *bnet) log() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.dials)
}

func (n *bnet) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dials, n.times = nil, nil
}

func decision(t *testing.T, target string, list ...string) policy.Decision {
	t.Helper()
	a, err := authority.ParseConnect(target)
	if err != nil {
		t.Fatal(err)
	}
	return policy.Decision{Target: a, Source: policy.SourceLocal, Interfaces: list}
}

// TestDialPlan checks plans (design 0008 D7, D11, I3).
func TestDialPlan(t *testing.T) {
	all := []string{"eth-a", "eth-b", "wifi"}
	pa := func(a string, unbound bool) iface.PlanAddr {
		return iface.PlanAddr{Addr: netip.MustParseAddrPort(a), Unbound: unbound}
	}
	groups := func(p iface.Plan) string {
		var out []string
		for _, g := range p.Groups {
			var as []string
			for _, a := range g.Addrs {
				as = append(as, a.Addr().String())
			}
			name := g.Interface
			if name == "" {
				name = "0"
			}
			out = append(out, name+"="+strings.Join(as, "+"))
		}
		return strings.Join(out, " ")
	}

	t.Run("list order and group 0 first", func(t *testing.T) {
		m := testMonitor(t, nil)
		p := iface.BuildPlan(all, m.PlanState, []iface.PlanAddr{
			pa("203.0.113.10:443", false), pa("10.20.0.5:443", true), pa("127.0.0.1:443", true), pa("192.0.2.50:443", true),
		})
		want := "0=10.20.0.5+127.0.0.1+192.0.2.50 eth-a=203.0.113.10 eth-b=203.0.113.10 wifi=203.0.113.10"
		if got := groups(p); got != want {
			t.Errorf("groups %q, want %q", got, want)
		}
		if p.Groups[1].Index != 7 || p.Groups[2].Index != 8 {
			t.Errorf("indexes %d %d", p.Groups[1].Index, p.Groups[2].Index)
		}
		// Other orders follow the list.
		p = iface.BuildPlan([]string{"wifi", "eth-a"}, m.PlanState, []iface.PlanAddr{pa("203.0.113.10:443", false)})
		if got := groups(p); got != "wifi=203.0.113.10 eth-a=203.0.113.10" {
			t.Errorf("wifi first: %q", got)
		}
	})

	t.Run("rule list replaces the default list", func(t *testing.T) {
		pol := policy.New(policy.Settings{
			Default:    policy.Route{Action: policy.ActionDirect},
			Interfaces: []string{"eth-a", "wifi"},
			Rules: []policy.Rule{
				{Name: "pin", Hosts: []string{"pin.example"}, Route: policy.Route{Action: policy.ActionDirect}, Interfaces: []string{"eth-b"}},
				{Name: "up", Hosts: []string{"up.example"}, Route: policy.Route{Action: policy.ActionUpstream, Upstream: "u"}},
				{Name: "plain", Hosts: []string{"plain.example"}, Route: policy.Route{Action: policy.ActionDirect}},
			},
		})
		for target, want := range map[string]string{
			"pin.example:443":   "eth-b",
			"plain.example:443": "eth-a,wifi",
			"other.example:443": "eth-a,wifi",
			"up.example:443":    "",
		} {
			a, _ := authority.ParseConnect(target)
			if got := strings.Join(pol.Decide(a, policy.KindConnect, policy.SourceLocal).Interfaces, ","); got != want {
				t.Errorf("%s: list %q, want %q", target, got, want)
			}
		}
	})

	t.Run("families and offline interfaces", func(t *testing.T) {
		m := testMonitor(t, func(rows []iface.Link) { rows[2].Media = iface.MediaInactive })
		p := iface.BuildPlan(all, m.PlanState, []iface.PlanAddr{pa("[2001:db8::1]:443", false), pa("203.0.113.10:443", false)})
		if got := groups(p); got != "eth-a=203.0.113.10 eth-b=2001:db8::1+203.0.113.10" {
			t.Errorf("groups %q", got)
		}
		if p.Passed["wifi"] != iface.ReasonLinkDown {
			t.Errorf("wifi passed with %q", p.Passed["wifi"])
		}
		p = iface.BuildPlan(all, m.PlanState, []iface.PlanAddr{pa("[2001:db8::1]:443", false)})
		if got := groups(p); got != "eth-b=2001:db8::1" || p.Passed["eth-a"] != iface.ReasonNoFamily {
			t.Errorf("IPv6 only: groups %q, eth-a %q", got, p.Passed["eth-a"])
		}
		// A NAT64 address is IPv6.
		p = iface.BuildPlan([]string{"eth-a"}, m.PlanState, []iface.PlanAddr{pa("[64:ff9b::cb00:710a]:443", false)})
		if len(p.Groups) != 0 || p.Passed["eth-a"] != iface.ReasonNoFamily {
			t.Errorf("NAT64 on an IPv4-only interface: %q %q", groups(p), p.Passed["eth-a"])
		}
	})

	t.Run("demoted interfaces last", func(t *testing.T) {
		m := testMonitor(t, nil)
		m.Report([]iface.Tried{{Interface: "eth-a", Result: iface.ResultTimeout}, {Interface: "eth-b", Result: iface.ResultUnreachable},
			{Interface: "wifi", Result: iface.ResultConnected}}, "wifi")
		p := iface.BuildPlan(all, m.PlanState, []iface.PlanAddr{pa("203.0.113.10:443", false)})
		if got := groups(p); got != "wifi=203.0.113.10 eth-a=203.0.113.10 eth-b=203.0.113.10" {
			t.Errorf("groups %q", got)
		}
	})

	t.Run("no group gives D10", func(t *testing.T) {
		m := testMonitor(t, func(rows []iface.Link) {
			for i := range rows {
				rows[i].Media = iface.MediaInactive
			}
		})
		n := newBnet()
		dl := &Dialer{Network: n, Timeout: time.Second, AttemptTimeout: 100 * time.Millisecond, Interfaces: m}
		_, _, err := dl.Dial(context.Background(), decision(t, "203.0.113.10:443", all...))
		var de *DialError
		if !errors.As(err, &de) || de.Type != ErrUnavailable || len(n.log()) != 0 {
			t.Fatalf("all offline: %v, dials %v", err, n.log())
		}
		if de.Detail != "no usable interface (eth-a: link_down, eth-b: link_down, wifi: link_down)" ||
			de.Skipped != "eth-a:link_down,eth-b:link_down,wifi:link_down" {
			t.Errorf("detail %q skipped %q", de.Detail, de.Skipped)
		}
	})

	t.Run("dialer groups by class", func(t *testing.T) {
		m := testMonitor(t, nil)
		n := newBnet()
		n.names["mixed.example"] = []netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("10.20.0.5")}
		n.set("", "10.20.0.5", syscall.ECONNREFUSED)
		dl := &Dialer{Network: n, Timeout: time.Second, Stagger: 50 * time.Millisecond, AttemptTimeout: 100 * time.Millisecond,
			Interfaces: m, SelfAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.50")} }}
		d := decision(t, "mixed.example:443", all...)
		d.AllowPrivate = true
		c, info, err := dl.Dial(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if got := strings.Join(n.log(), ","); got != " 10.20.0.5,eth-a 203.0.113.10" {
			t.Errorf("dials %q", got)
		}
		if info.Interface != "eth-a" || info.Local || info.Skipped != "" {
			t.Errorf("info %+v", info)
		}
		// A self literal is dialed unbound and names no interface.
		n.reset()
		c, info, err = dl.Dial(context.Background(), decision(t, "192.0.2.50:443", all...))
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if got := strings.Join(n.log(), ","); got != " 192.0.2.50" || !info.Local || info.Interface != "" {
			t.Errorf("self: dials %q info %+v", got, info)
		}
	})

	t.Run("upstream private address in group 0", func(t *testing.T) {
		m := testMonitor(t, nil)
		n := newBnet()
		for _, addr := range []string{"10.8.0.6", "203.0.113.20"} {
			a, _ := authority.ParseConnect(addr + ":3128")
			dl := &UpstreamDialer{Network: n, Timeout: time.Second, Interfaces: []string{"eth-a", "wifi"}, Monitor: m,
				AttemptTimeout: func() time.Duration { return 100 * time.Millisecond }}
			c, info, err := dl.Dial(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			_ = info
		}
		if got := strings.Join(n.log(), ","); got != " 10.8.0.6,eth-a 203.0.113.20" {
			t.Errorf("dials %q", got)
		}
	})
}

// TestFailover checks which errors move a plan to its next group, which
// end the dial, and the group deadlines (design 0008 D8, I4).
func TestFailover(t *testing.T) {
	all := []string{"eth-a", "eth-b", "wifi"}
	dial := func(n *bnet, m *iface.Monitor, target string, list ...string) (Info, error) {
		dl := &Dialer{Network: n, Timeout: 2 * time.Second, Stagger: 50 * time.Millisecond, AttemptTimeout: 300 * time.Millisecond, Interfaces: m}
		c, info, err := dl.Dial(context.Background(), decision(t, target, list...))
		if c != nil {
			c.Close()
		}
		return info, err
	}
	for _, e := range []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.EHOSTDOWN,
		syscall.EADDRNOTAVAIL, syscall.ENXIO} {
		t.Run("failover "+e.Error(), func(t *testing.T) {
			n := newBnet()
			n.set("eth-a", "203.0.113.10", e)
			info, err := dial(n, testMonitor(t, nil), "203.0.113.10:443", all...)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(n.log(), ","); got != "eth-a 203.0.113.10,eth-b 203.0.113.10" {
				t.Errorf("dials %q", got)
			}
			if info.Interface != "eth-b" || info.Skipped != "eth-a:unreachable" {
				t.Errorf("info %+v", info)
			}
			if !IsFailover(&net.OpError{Err: e}) {
				t.Errorf("IsFailover(%v) = false", e)
			}
		})
	}
	for _, c := range []struct {
		err     error
		skipped string
	}{
		{syscall.ECONNREFUSED, "eth-a:refused"},
		{syscall.ECONNRESET, "eth-a:refused"},
		{syscall.EACCES, "eth-a:error"},
		{syscall.EPERM, "eth-a:error"},
		{syscall.EIO, "eth-a:error"},
		{errors.New("unrecognized"), "eth-a:error"},
	} {
		t.Run("end "+c.err.Error(), func(t *testing.T) {
			n := newBnet()
			n.set("eth-a", "203.0.113.10", c.err)
			_, err := dial(n, testMonitor(t, nil), "203.0.113.10:443", all...)
			var de *DialError
			if !errors.As(err, &de) || de.Type != ErrRefused {
				t.Fatalf("err %v", err)
			}
			if got := strings.Join(n.log(), ","); got != "eth-a 203.0.113.10" {
				t.Errorf("dials %q", got)
			}
			if de.Skipped != c.skipped {
				t.Errorf("skipped %q, want %q", de.Skipped, c.skipped)
			}
		})
	}

	t.Run("one refused and one unreachable attempt end the dial", func(t *testing.T) {
		n := newBnet()
		n.names["two.example"] = []netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("203.0.113.11")}
		n.set("eth-a", "203.0.113.10", syscall.ENETUNREACH)
		n.set("eth-a", "203.0.113.11", syscall.ECONNREFUSED)
		_, err := dial(n, testMonitor(t, nil), "two.example:443", all...)
		var de *DialError
		if !errors.As(err, &de) || de.Type != ErrRefused || !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("err %v", err)
		}
		if got := strings.Join(n.log(), ","); got != "eth-a 203.0.113.10,eth-a 203.0.113.11" {
			t.Errorf("dials %q", got)
		}
	})

	t.Run("group 0 passes on any error", func(t *testing.T) {
		for _, e := range []error{syscall.ECONNREFUSED, syscall.EPERM, errors.New("unrecognized")} {
			n := newBnet()
			n.names["lan.example"] = []netip.Addr{netip.MustParseAddr("10.20.0.5"), netip.MustParseAddr("203.0.113.10")}
			n.set("", "10.20.0.5", e)
			dl := &Dialer{Network: n, Timeout: 2 * time.Second, AttemptTimeout: 300 * time.Millisecond, Interfaces: testMonitor(t, nil)}
			d := decision(t, "lan.example:443", all...)
			d.AllowPrivate = true
			c, info, err := dl.Dial(context.Background(), d)
			if err != nil {
				t.Fatalf("%v: %v", e, err)
			}
			c.Close()
			if got := strings.Join(n.log(), ","); got != " 10.20.0.5,eth-a 203.0.113.10" || info.Skipped != "" {
				t.Errorf("%v: dials %q skipped %q", e, got, info.Skipped)
			}
		}
	})

	t.Run("deadlines", func(t *testing.T) {
		n := newBnet()
		for _, name := range all {
			n.set(name, "203.0.113.10", "hang")
		}
		dl := &Dialer{Network: n, Timeout: 900 * time.Millisecond, AttemptTimeout: 200 * time.Millisecond, Interfaces: testMonitor(t, nil)}
		start := time.Now()
		_, _, err := dl.Dial(context.Background(), decision(t, "203.0.113.10:443", all...))
		el := time.Since(start)
		var de *DialError
		if !errors.As(err, &de) || de.Type != ErrTimeout {
			t.Fatalf("err %v", err)
		}
		if de.Skipped != "eth-a:timeout,eth-b:timeout,wifi:timeout" {
			t.Errorf("skipped %q", de.Skipped)
		}
		n.mu.Lock()
		times := slices.Clone(n.times)
		n.mu.Unlock()
		if len(times) != 3 {
			t.Fatalf("dials %v", n.log())
		}
		near := func(what string, got, want time.Duration) {
			if got < want-50*time.Millisecond || got > want+100*time.Millisecond {
				t.Errorf("%s at %v, want %v", what, got, want)
			}
		}
		near("eth-b start", times[1].Sub(start), 200*time.Millisecond)
		near("wifi start", times[2].Sub(start), 400*time.Millisecond)
		near("end", el, 900*time.Millisecond)

		// A budget shorter than the attempt timeout ends in the first group.
		n.reset()
		dl.Timeout = 150 * time.Millisecond
		start = time.Now()
		_, _, err = dl.Dial(context.Background(), decision(t, "203.0.113.10:443", all...))
		if !errors.As(err, &de) || de.Type != ErrTimeout || len(n.log()) != 1 {
			t.Fatalf("short budget: %v dials %v", err, n.log())
		}
		near("short budget end", time.Since(start), 150*time.Millisecond)
	})
}

// TestBoundDial checks that a real socket gets the binding option (design
// 0008 D7, I5). Only loopback is used, and the documentation destinations
// fail before any packet leaves the host.
func TestBoundDial(t *testing.T) {
	if testing.Short() {
		t.Skip("uses sockets; skipped with -short")
	}
	lo, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sys := System{}
	b := Binding{Name: "lo", Index: lo.Index}
	for _, c := range []struct {
		listen, dead string
		want         []syscall.Errno
	}{
		{"127.0.0.1:0", "192.0.2.1:9", []syscall.Errno{syscall.ENETUNREACH}},
		{"[::1]:0", "[2001:db8::1]:9", []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH}},
	} {
		ln, err := net.Listen("tcp", c.listen)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		conn, err := sys.DialContext(ctx, ln.Addr().(*net.TCPAddr).AddrPort(), b)
		if err != nil {
			t.Fatalf("bound dial to %s: %v", ln.Addr(), err)
		}
		// Where getsockopt supports the option, it returns the index.
		rc, _ := conn.(*net.TCPConn).SyscallConn()
		level, opt := syscall.IPPROTO_IP, syscall.IP_BOUND_IF
		if strings.HasPrefix(c.listen, "[") {
			level, opt = syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF
		}
		var got int
		var gerr error
		rc.Control(func(fd uintptr) { got, gerr = syscall.GetsockoptInt(int(fd), level, opt) })
		if gerr == nil && got != lo.Index {
			t.Errorf("%s: getsockopt returned index %d, want %d", c.listen, got, lo.Index)
		}
		conn.Close()

		start := time.Now()
		_, err = sys.DialContext(ctx, netip.MustParseAddrPort(c.dead), b)
		if el := time.Since(start); el > 100*time.Millisecond {
			t.Errorf("%s: failed after %v", c.dead, el)
		}
		if !slices.ContainsFunc(c.want, func(e syscall.Errno) bool { return errors.Is(err, e) }) {
			t.Errorf("%s: %v, want one of %v", c.dead, err, c.want)
		}
		if !IsFailover(err) {
			t.Errorf("%s: %v is not a failover error", c.dead, err)
		}
		var errno syscall.Errno
		errors.As(err, &errno)
		t.Logf("%s bound to lo0: %s (errno %d); getsockopt of the option: %v", c.dead, errno.Error(), int(errno), gerr)
	}
	// An index that names no interface fails with ENXIO.
	ifs, _ := net.Interfaces()
	max := 0
	for _, i := range ifs {
		max = maxInt(max, i.Index)
	}
	_, err = sys.DialContext(ctx, netip.MustParseAddrPort("127.0.0.1:9"), Binding{Name: "gone", Index: max + 1000})
	if !errors.Is(err, syscall.ENXIO) || !IsFailover(err) || !isStale(err) {
		t.Errorf("unknown index: %v", err)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
