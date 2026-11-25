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

	"github.com/neutrospec/cuttle/internal/authority"
	"github.com/neutrospec/cuttle/internal/iface"
	"github.com/neutrospec/cuttle/internal/policy"
)

// Binding names the interface a dial must use. The zero value is an
// unbound dial that follows system routing (design 0008 D7).
type Binding struct {
	Name  string // configured interface name, for logs and counters
	Index int    // system interface index for IP_BOUND_IF and IPV6_BOUND_IF
}

// Network is the only path from the proxy to name resolution and sockets.
type Network interface {
	LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
	// DialContext dials an address literal; b binds the dial to one
	// interface, and the zero Binding follows system routing.
	DialContext(ctx context.Context, addr netip.AddrPort, b Binding) (net.Conn, error)
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

// NonPublic reports whether every address of the prefix is of class
// loopback, private, or unusable, so that interface lists never apply to
// it (design 0008 D6 warnings).
func NonPublic(p netip.Prefix) bool {
	p = p.Masked()
	for _, list := range [][]netip.Prefix{unusable, loopback, private} {
		for _, q := range list {
			if q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
				return true
			}
		}
	}
	return false
}

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
	// ErrUnavailable marks a plan without a group: no interface of the
	// list in force is usable (design 0008 D10).
	ErrUnavailable = "destination_unavailable"
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
	// Skipped is the decision log's skipped field (design 0008 D15).
	Skipped string
	// Unusable lists each interface of the list in force with its reason,
	// as "name: reason", when Type is ErrUnavailable.
	Unusable []string
}

func (e *DialError) Error() string { return e.Type + ": " + e.Detail }
func (e *DialError) Unwrap() error { return e.Err }

// Info describes the outbound connection of a successful dial for the
// decision log (design 0008 D15).
type Info struct {
	// Interface names the interface of a bound dial; "" for an unbound
	// dial, whose interface is attributed by local address.
	Interface string
	// Local marks a destination of class loopback or self, whose traffic
	// never leaves the host, so the decision names no interface.
	Local bool
	// Skipped is the decision log's skipped field.
	Skipped string
}

// Dialer resolves and dials direct routes.
type Dialer struct {
	Network Network
	// SelfAddrs returns this host's interface addresses.
	SelfAddrs func() []netip.Addr
	// Timeout bounds the lookup and all connection attempts together.
	Timeout time.Duration
	// Stagger is the delay before the next address is tried (RFC 8305).
	Stagger time.Duration
	// AttemptTimeout is the time each group of a plan but the last gets
	// (proxy.interface_attempt_timeout, design 0008 D8).
	AttemptTimeout time.Duration
	// Interfaces is the interface monitor; it is needed only for decisions
	// with a list in force.
	Interfaces *iface.Monitor
}

// Dial resolves the decision's target, drops addresses the address policy
// refuses, and dials the rest: in the resolver's order without a list in
// force, and by the dial plan of design 0008 D7 with one.
func (dl *Dialer) Dial(ctx context.Context, d policy.Decision) (net.Conn, Info, error) {
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
			return nil, Info{}, &DialError{Type: ErrDNS, Detail: "lookup failed: " + lookupReason(err), Err: err, ClientGone: gone()}
		}
		if len(addrs) == 0 {
			return nil, Info{}, &DialError{Type: ErrDNS, Detail: "lookup returned no address"}
		}
	}
	var self []netip.Addr
	if dl.SelfAddrs != nil {
		self = dl.SelfAddrs()
	}
	var allowed []classified
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
		allowed = append(allowed, classified{netip.AddrPortFrom(a.WithZone("").Unmap(), t.Port), c})
	}
	if len(allowed) == 0 {
		return nil, Info{}, &DialError{Type: ErrProhibited, Detail: "refused address classes: " + strings.Join(refused, ",")}
	}
	if len(d.Interfaces) == 0 || dl.Interfaces == nil {
		targets := make([]netip.AddrPort, len(allowed))
		for i, a := range allowed {
			targets[i] = a.addr
		}
		conn, addr, gr := dialGroup(ctx, dl.Network, dl.Stagger, targets, Binding{})
		if conn == nil {
			return nil, Info{}, connectError(gr.err(ctx), dl.Timeout, gone())
		}
		return conn, Info{Local: localClass(allowed, addr)}, nil
	}
	return dialList(ctx, dl.Network, dl.Interfaces, d.Interfaces, dl.Stagger, dl.AttemptTimeout, dl.Timeout, allowed, gone,
		func(c Class) bool { return c != ClassPublic })
}

// classified is an allowed address with its class.
type classified struct {
	addr  netip.AddrPort
	class Class
}

// localClass reports whether the address that connected is of class
// loopback or self.
func localClass(list []classified, addr netip.AddrPort) bool {
	for _, a := range list {
		if a.addr == addr {
			return a.class == ClassLoopback || a.class == ClassSelf
		}
	}
	return false
}

// dialList dials by the plan for list: group 0 with the addresses that
// unbound reports true for, then one bound group per usable interface
// (design 0008 D7, D8, D11). It reports the dial to the monitor, which
// demotes and restores interfaces and re-reads a stale table (D5, D9).
func dialList(ctx context.Context, n Network, mon *iface.Monitor, list []string, stagger, attempt, timeout time.Duration,
	allowed []classified, gone func() bool, unbound func(Class) bool) (net.Conn, Info, error) {
	pas := make([]iface.PlanAddr, len(allowed))
	for i, a := range allowed {
		pas[i] = iface.PlanAddr{Addr: a.addr, Unbound: unbound(a.class)}
	}
	plan := iface.BuildPlan(list, mon.PlanState, pas)
	if len(plan.Groups) == 0 {
		un := plan.Unusable()
		detail := make([]string, len(un))
		for i, u := range un {
			name, reason, _ := strings.Cut(u, ":")
			detail[i] = name + ": " + reason
		}
		return nil, Info{}, &DialError{Type: ErrUnavailable, Detail: "no usable interface (" + strings.Join(detail, ", ") + ")",
			Skipped: strings.Join(un, ","), Unusable: detail}
	}
	conn, res, err := runPlan(ctx, n, stagger, attempt, plan.Groups)
	mon.Report(res.tried, res.carried)
	if res.stale {
		mon.StaleTable()
	}
	skipped := plan.Skipped(res.tried, res.carried, err != nil)
	if err != nil {
		de := connectError(err, timeout, gone())
		de.Skipped = skipped
		return nil, Info{}, de
	}
	return conn, Info{Interface: res.carried, Local: localClass(allowed, res.addr), Skipped: skipped}, nil
}

// planResult is what runPlan reports about a dial.
type planResult struct {
	tried   []iface.Tried
	carried string         // interface of the bound group that connected
	addr    netip.AddrPort // the address that connected
	// stale reports a bound attempt that failed with ENXIO,
	// EADDRNOTAVAIL, or ENETDOWN, which shows a stale table (design 0008
	// D5).
	stale bool
}

// runPlan runs the groups one after another (design 0008 D8). Every group
// but the last gets attempt, or the rest of ctx's budget when that is
// shorter; the last gets the rest. A group that connected ends the dial. A
// bound group whose attempts all ended with failover errors or were
// pending at its deadline passes to the next group; one with another error
// ends the dial with that error. Group 0 passes to the next group on any
// error. When the plan is exhausted, the last group that ran decides the
// error: a deadline, or its failover error.
func runPlan(ctx context.Context, n Network, stagger, attempt time.Duration, groups []iface.Group) (net.Conn, planResult, error) {
	var res planResult
	var last error
	for i, g := range groups {
		if ctx.Err() != nil {
			break
		}
		gctx, cancel := ctx, context.CancelFunc(func() {})
		if i < len(groups)-1 {
			gctx, cancel = context.WithTimeout(ctx, attempt)
		}
		b := Binding{Name: g.Interface, Index: g.Index}
		conn, addr, gr := dialGroup(gctx, n, stagger, g.Addrs, b)
		cancel()
		if b.Index != 0 && slices.ContainsFunc(gr.errs, isStale) {
			res.stale = true
		}
		if conn != nil {
			res.addr = addr
			if g.Interface != "" {
				res.tried = append(res.tried, iface.Tried{Interface: g.Interface, Result: iface.ResultConnected})
				res.carried = g.Interface
			}
			return conn, res, nil
		}
		if g.Interface == "" {
			last = gr.err(gctx)
			continue
		}
		switch {
		case gr.other != nil:
			result := iface.ResultError
			if errors.Is(gr.other, syscall.ECONNREFUSED) || errors.Is(gr.other, syscall.ECONNRESET) {
				result = iface.ResultRefused
			}
			res.tried = append(res.tried, iface.Tried{Interface: g.Interface, Result: result})
			return nil, res, gr.other
		case gr.pending:
			res.tried = append(res.tried, iface.Tried{Interface: g.Interface, Result: iface.ResultTimeout})
			last = context.DeadlineExceeded
		default:
			res.tried = append(res.tried, iface.Tried{Interface: g.Interface, Result: iface.ResultUnreachable})
			last = gr.failover
		}
	}
	if last == nil {
		last = context.DeadlineExceeded
	}
	return nil, res, last
}

// failoverErrnos are the local and path errors that say an interface
// cannot carry the connection now (design 0008 D8).
var failoverErrnos = []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.EHOSTDOWN,
	syscall.EADDRNOTAVAIL, syscall.ENXIO}

// IsFailover reports whether a connect error moves a plan to its next
// group (design 0008 D8). Every other error, refusals, local policy, and
// errors cuttle does not recognize, ends the dial.
func IsFailover(err error) bool {
	for _, e := range failoverErrnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// isStale reports the bound-dial errors that show a stale interface table
// (design 0008 D5).
func isStale(err error) bool {
	return errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.ENETDOWN)
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
	// SelfAddrs returns this host's interface addresses, to group the
	// upstream's addresses and to leave a loopback or own address without
	// an interface (design 0008 D11, D15).
	SelfAddrs func() []netip.Addr
	// Interfaces is the upstream's own list; empty follows system routing
	// (design 0008 D11).
	Interfaces []string
	// AttemptTimeout returns proxy.interface_attempt_timeout of the
	// configuration in effect.
	AttemptTimeout func() time.Duration
	// Monitor is the interface monitor, needed when Interfaces is set.
	Monitor *iface.Monitor
}

// Dial resolves the upstream address and dials it. A failure is a
// *DialError of type ErrDNS, ErrRefused, ErrTimeout, ErrLoop, or
// ErrUnavailable.
func (dl *UpstreamDialer) Dial(ctx context.Context, t authority.Authority) (net.Conn, Info, error) {
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
			return nil, Info{}, &DialError{Type: ErrDNS, Detail: "upstream lookup failed: " + lookupReason(err), Err: err, ClientGone: gone()}
		}
		if len(addrs) == 0 {
			return nil, Info{}, &DialError{Type: ErrDNS, Detail: "upstream lookup returned no address"}
		}
	}
	var self []netip.Addr
	if dl.SelfAddrs != nil {
		self = dl.SelfAddrs()
	}
	targets := make([]classified, 0, len(addrs))
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a.WithZone("").Unmap(), t.Port)
		if dl.Self != nil && dl.Self(ap) {
			return nil, Info{}, &DialError{Type: ErrLoop, Detail: "upstream address " + ap.String() + " is this instance's own listener"}
		}
		targets = append(targets, classified{ap, Classify(a, self)})
	}
	if len(dl.Interfaces) == 0 || dl.Monitor == nil {
		list := make([]netip.AddrPort, len(targets))
		for i, a := range targets {
			list[i] = a.addr
		}
		conn, addr, gr := dialGroup(ctx, dl.Network, dl.Stagger, list, Binding{})
		if conn == nil {
			return nil, Info{}, connectError(gr.err(ctx), dl.Timeout, gone())
		}
		return conn, Info{Local: localClass(targets, addr)}, nil
	}
	// Only the grouping uses the classes: every non-public address,
	// whatever its class, is dialed unbound (design 0008 D11).
	attempt := 3 * time.Second
	if dl.AttemptTimeout != nil {
		attempt = dl.AttemptTimeout()
	}
	return dialList(ctx, dl.Network, dl.Monitor, dl.Interfaces, dl.Stagger, attempt, dl.Timeout, targets, gone,
		func(c Class) bool { return c != ClassPublic })
}

// groupResult collects the failed attempts of one group.
type groupResult struct {
	errs     []error
	first    error // the first error of any kind
	pending  bool  // an attempt was still pending at the group's deadline
	failover error // the first failover error
	other    error // the first error that is not a failover error
}

func (g *groupResult) add(ctx context.Context, err error) {
	g.errs = append(g.errs, err)
	if g.first == nil {
		g.first = err
	}
	switch {
	case ctx.Err() != nil && (isTimeout(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)):
		g.pending = true
	case IsFailover(err):
		if g.failover == nil {
			g.failover = err
		}
	default:
		if g.other == nil {
			g.other = err
		}
	}
}

// err is the error of a group that did not connect, as M1.4 reported it:
// a deadline when ctx's deadline passed, else the first error.
func (g *groupResult) err(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || g.first == nil {
		return context.DeadlineExceeded
	}
	return g.first
}

// dialGroup starts the next address stagger after the previous one unless
// it has connected, and at once when it fails, and returns the first
// connection that succeeds with its address. No attempt starts after ctx
// ends.
func dialGroup(ctx context.Context, network Network, stagger time.Duration, addrs []netip.AddrPort, b Binding) (net.Conn, netip.AddrPort, groupResult) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		i   int
		c   net.Conn
		err error
	}
	results := make(chan result, len(addrs))
	started, finished := 0, 0
	var gr groupResult
	start := func() {
		i := started
		started++
		go func() {
			c, err := network.DialContext(ctx, addrs[i], b)
			results <- result{i, c, err}
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
				return r.c, addrs[r.i], gr
			}
			gr.add(ctx, r.err)
			if started < len(addrs) && ctx.Err() == nil {
				start()
				timer.Reset(stagger)
			} else if finished == started {
				return nil, netip.AddrPort{}, gr
			}
		case <-timer.C:
			if started < len(addrs) && ctx.Err() == nil {
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
	case errors.Is(err, syscall.ENETDOWN), errors.Is(err, syscall.EHOSTDOWN):
		return "network down"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "address not available"
	case errors.Is(err, syscall.ENXIO):
		return "no such interface"
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return "not permitted"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "error"
}
