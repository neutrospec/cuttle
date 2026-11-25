// Package proxy serves the proxy port: the listener wrapper, the root
// handler, tunnels, plain-HTTP forwarding, cuttle's own responses, live
// counters, and the decision log (design 0002 D2-D8).
//
// It serves local clients and allowlisted remote clients on loopback and
// non-loopback listeners (design 0003 D9-D12) with direct, block, and
// upstream routes, including chains of cuttle instances (design 0003
// D1-D8). It retries non-loopback listeners that cannot bind, applies a
// reloaded configuration without dropping tunnels (design 0004 D6), and
// reports its listeners, upstreams, and tunnels for cuttle status (design
// 0006 D4).
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"squid/internal/config"
	"squid/internal/iface"
	"squid/internal/netx"
	"squid/internal/policy"
	"squid/internal/stats"
)

// Server settings from design 0002 D2.
const (
	ReadHeaderTimeout = 30 * time.Second
	IdleTimeout       = 120 * time.Second
	MaxHeaderBytes    = 64 << 10
	relayBufferSize   = 32 << 10
	dialStagger       = 300 * time.Millisecond
	// RetryInterval is how often a non-loopback listener that cannot bind
	// is tried again (design 0003 D9).
	RetryInterval = 30 * time.Second
	// handlerWait bounds how long Shutdown waits, after closing tunnels and
	// connections, for their handlers to write their decision lines, so
	// shutdown stays within the 8 s budget of design 0006 D6.
	handlerWait = time.Second
)

// Deps are the proxy's injected dependencies. Network and SelfAddrs are
// required: internal packages have no default network (design 0007 D3).
type Deps struct {
	Network   netx.Network
	SelfAddrs func() []netip.Addr
	// Listeners are the proxy listeners, already bound.
	Listeners []net.Listener
	// Retry lists the configured listener addresses that could not bind at
	// startup. The proxy tries each again every RetryInterval with Bind while
	// the other listeners serve, and reports it as retrying (design 0003
	// D9).
	Retry []RetryListener
	// Bind binds a retried listener; it is net.Listen outside tests.
	Bind func(network, address string) (net.Listener, error)
	// RetryInterval overrides the package RetryInterval when positive.
	RetryInterval time.Duration
	// Logger receives decision lines at info and server warnings.
	Logger *slog.Logger
	// Live receives the live counters; New creates one when nil.
	Live *stats.Live
	// Stats receives each completed decision and samples the bytes of open
	// tunnels and exchanges for the hourly statistics; nil records nothing
	// (design 0005 D4).
	Stats *stats.Recorder
	// Clock is the time source of the unreachable-upstream hold and of
	// interface state times and demotion; nil means time.Now. Tests inject
	// a clock (design 0003 U11, 0008 D9).
	Clock func() time.Time
	// Interfaces is the interface table source. A configuration that
	// defines an interface needs it: New fails and a reload is refused
	// without it (design 0008 Interfaces).
	Interfaces iface.Source
	// SelfChanged is called after each successful re-read of the
	// interface table, so the self address class follows it (design 0008
	// D5).
	SelfChanged func()
	// Timers drives the debounce and safety re-reads of the interface
	// table; nil means real timers. Tests inject a timer.
	Timers iface.Timers
}

// RetryListener is a listener address that could not bind at startup.
type RetryListener struct {
	Address string // as configured, IP:port
	Err     error  // the bind error
	// Index is the address's position in proxy.listen, so status lists the
	// listeners in configuration order; Deps.Listeners fill the other
	// positions in their order.
	Index int
}

// Listener states for status displays (design 0003 D9, 0006 D4).
const (
	ListenerBound    = "bound"
	ListenerRetrying = "retrying"
)

// ListenerStatus is one proxy listener for status displays.
type ListenerStatus struct {
	Address string
	State   string // ListenerBound or ListenerRetrying
	Error   string // the last bind error while retrying
	Since   time.Time
}

// listenerState is a listener and its state; lmu guards it.
type listenerState struct {
	addr  string
	state string
	err   string
	since time.Time
	ln    net.Listener
}

// Proxy is one running proxy instance.
type Proxy struct {
	deps Deps
	log  *slog.Logger
	live *stats.Live
	rec  *stats.Recorder
	snap atomic.Pointer[snapshot]
	srv  *http.Server

	conns connLimits
	warn  *warnLimiter
	// ifm keeps the state of the configured interfaces (design 0008).
	ifm *iface.Monitor

	// lmu guards the listener states and the bound addresses, which the
	// upstream self-dial check reads (design 0003 D1).
	lmu       sync.Mutex
	lstates   []*listenerState
	listeners []netip.AddrPort
	serving   sync.WaitGroup
	serveErr  error
	done      chan struct{} // closed when shutdown starts
	doneOnce  sync.Once

	// reloadMu serializes Reload.
	reloadMu sync.Mutex

	// mu guards the tracked connections: CONNECT tunnels and connections
	// upgraded after a 101, which Shutdown closes (design 0006 D6).
	mu      sync.Mutex
	tunnels map[tracked]struct{}
	closing bool
	// forced is set when Shutdown closes connections whose exchanges did not
	// finish within the drain, so they log outcome shutdown.
	forced atomic.Bool
	// handlers counts root handler calls in progress; Shutdown waits for
	// them, so every decision line comes before the caller logs stopped.
	handlers handlerCount
}

// handlerCount counts calls in progress and lets one waiter wait, with a
// bound, until none is left.
type handlerCount struct {
	mu   sync.Mutex
	n    int
	zero chan struct{} // closed when n drops to 0 while someone waits
}

func (h *handlerCount) enter() {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
}

func (h *handlerCount) leave() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n--; h.n == 0 && h.zero != nil {
		close(h.zero)
		h.zero = nil
	}
}

// wait waits until no call is in progress or timeout passes, and returns
// how many calls are still in progress.
func (h *handlerCount) wait(timeout time.Duration) int {
	h.mu.Lock()
	if h.n == 0 {
		h.mu.Unlock()
		return 0
	}
	if h.zero == nil {
		h.zero = make(chan struct{})
	}
	ch := h.zero
	h.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// sourceClasses lists the client classes; each has its own direct
// transport.
var sourceClasses = [...]policy.SourceClass{policy.SourceLocal, policy.SourceRemote}

// snapshot is an immutable policy snapshot with a direct transport per
// client class and interface list in force, so a pooled connection is only
// reused for the same snapshot, class, list, host, and port, under the
// policy that allowed its dial (design 0002 D5, 0008 D12). Without the
// class split, a connection dialed for a local client, for example to a
// private address through allow_private, would carry a remote client's
// request to the same host and port; without the list split, a connection
// pooled on one interface would serve a request pinned to another. The
// upstream table holds one runtime state per configured upstream; a reload
// keeps the state of an upstream whose settings are unchanged (design
// 0003 D8, 0004 D6).
type snapshot struct {
	cfg    *config.Config
	policy *policy.Policy
	dialer *netx.Dialer
	// direct and rp are keyed by listKey of the list in force: "" for no
	// list, the default list, and each rule's list.
	direct    map[string]*directTransports
	upstreams map[string]*upstream
}

// directTransports are the direct transports of one interface list.
type directTransports struct {
	list []string
	tr   [len(sourceClasses)]*http.Transport
	rp   [len(sourceClasses)]*httputil.ReverseProxy
}

// listKey identifies an interface list in force.
func listKey(list []string) string { return strings.Join(list, ",") }

// New builds a proxy for cfg. It does not start serving.
func New(cfg *config.Config, deps Deps) (*Proxy, error) {
	if deps.Network == nil {
		return nil, errors.New("proxy: Deps.Network is required")
	}
	if deps.SelfAddrs == nil {
		return nil, errors.New("proxy: Deps.SelfAddrs is required")
	}
	if len(deps.Listeners)+len(deps.Retry) == 0 {
		return nil, errors.New("proxy: no listeners")
	}
	if len(deps.Retry) > 0 && deps.Bind == nil {
		return nil, errors.New("proxy: Deps.Bind is required to retry listeners")
	}
	if deps.RetryInterval <= 0 {
		deps.RetryInterval = RetryInterval
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Live == nil {
		deps.Live = stats.NewLive()
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	p := &Proxy{
		deps:    deps,
		log:     deps.Logger,
		live:    deps.Live,
		rec:     deps.Stats,
		tunnels: map[tracked]struct{}{},
		warn:    newWarnLimiter(deps.Logger),
		done:    make(chan struct{}),
	}
	p.ifm = iface.NewMonitor(iface.Options{
		Source: deps.Interfaces, Logger: deps.Logger, Live: deps.Live, Clock: deps.Clock,
		Timers: deps.Timers, SelfChanged: deps.SelfChanged, Reconnect: p.reconnect,
	})
	if err := p.ifm.Check(cfg.Defs()); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	p.lstates = listenerStates(deps.Listeners, deps.Retry, time.Now())
	for _, l := range deps.Listeners {
		p.addBoundLocked(l)
	}
	ups := map[string]*upstream{}
	for _, u := range cfg.Upstreams {
		up, err := p.newUpstream(u)
		if err != nil {
			return nil, err
		}
		ups[u.Name] = up
	}
	if err := checkRoutes(cfg, ups); err != nil {
		return nil, err
	}
	if err := p.ifm.Configure(cfg.Defs(), cfg.Proxy.InterfaceDemotion); err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	p.snap.Store(p.newSnapshot(cfg, ups))
	for _, up := range ups {
		up.hold.publish()
	}
	p.srv = &http.Server{
		Handler:                      p,
		ReadHeaderTimeout:            ReadHeaderTimeout,
		IdleTimeout:                  IdleTimeout,
		MaxHeaderBytes:               MaxHeaderBytes,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(deps.Logger.Handler(), slog.LevelWarn),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*trackedConn); ok {
				return context.WithValue(ctx, connKey{}, tc)
			}
			return ctx
		},
	}
	return p, nil
}

func ruleRoutes(cfg *config.Config) []policy.Route {
	out := make([]policy.Route, len(cfg.Rules))
	for i, r := range cfg.Rules {
		out[i] = r.Route
	}
	return out
}

// listenerStates orders the bound and the retried listeners as configured:
// each retried one at its Index, the bound ones in the remaining positions
// in their order. A retried listener whose Index is out of range or taken
// fills a remaining position.
func listenerStates(bound []net.Listener, retry []RetryListener, now time.Time) []*listenerState {
	slots := make([]*listenerState, len(bound)+len(retry))
	var rest []*listenerState
	for _, r := range retry {
		st := &listenerState{addr: r.Address, state: ListenerRetrying, err: r.Err.Error(), since: now}
		if r.Index >= 0 && r.Index < len(slots) && slots[r.Index] == nil {
			slots[r.Index] = st
		} else {
			rest = append(rest, st)
		}
	}
	for i := range slots {
		if slots[i] != nil {
			continue
		}
		if len(bound) > 0 {
			slots[i] = &listenerState{addr: bound[0].Addr().String(), state: ListenerBound, since: now, ln: bound[0]}
			bound = bound[1:]
		} else {
			slots[i], rest = rest[0], rest[1:]
		}
	}
	return slots
}

// checkRoutes refuses a configuration whose routes name an upstream that the
// table lacks; validation already rejects it.
func checkRoutes(cfg *config.Config, ups map[string]*upstream) error {
	for _, r := range append([]policy.Route{cfg.Proxy.Default, cfg.Proxy.Remote.Default}, ruleRoutes(cfg)...) {
		if r.Action == policy.ActionUpstream && ups[r.Upstream] == nil {
			return fmt.Errorf("proxy: route %s names an unknown upstream", r)
		}
	}
	return nil
}

// now is the hold's clock.
func (p *Proxy) now() time.Time { return p.deps.Clock() }

// addBoundLocked records a bound listener's address for the self-dial
// check. The caller holds lmu or owns p exclusively.
func (p *Proxy) addBoundLocked(l net.Listener) {
	if ta, ok := l.Addr().(*net.TCPAddr); ok {
		ap := ta.AddrPort()
		p.listeners = append(p.listeners, netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
	}
}

// isListener reports whether ap is one of this instance's bound listeners.
func (p *Proxy) isListener(ap netip.AddrPort) bool {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	return slices.Contains(p.listeners, ap)
}

// Upstreams returns the hold state of every upstream of the current
// configuration, in configuration order (design 0003 D8).
func (p *Proxy) Upstreams() []UpstreamStatus {
	s := p.snap.Load()
	out := make([]UpstreamStatus, 0, len(s.cfg.Upstreams))
	for _, u := range s.cfg.Upstreams {
		if up := s.upstreams[u.Name]; up != nil {
			out = append(out, up.hold.status())
		}
	}
	return out
}

// Listeners returns the state of every proxy listener: the ones bound at
// startup, then the ones retried (design 0003 D9).
func (p *Proxy) Listeners() []ListenerStatus {
	p.lmu.Lock()
	defer p.lmu.Unlock()
	out := make([]ListenerStatus, len(p.lstates))
	for i, st := range p.lstates {
		out[i] = ListenerStatus{Address: st.addr, State: st.state, Error: st.err, Since: st.since}
	}
	return out
}

// Config returns the configuration in effect.
func (p *Proxy) Config() *config.Config { return p.snap.Load().cfg }

func (p *Proxy) newSnapshot(cfg *config.Config, ups map[string]*upstream) *snapshot {
	s := &snapshot{
		cfg:       cfg,
		policy:    policy.New(cfg.PolicySettings()),
		upstreams: ups,
		direct:    map[string]*directTransports{},
		dialer: &netx.Dialer{
			Network:        p.deps.Network,
			SelfAddrs:      p.deps.SelfAddrs,
			Timeout:        cfg.Proxy.ConnectTimeout,
			Stagger:        dialStagger,
			AttemptTimeout: cfg.Proxy.InterfaceAttemptTimeout,
			Interfaces:     p.ifm,
		},
	}
	lists := [][]string{nil, cfg.Proxy.Interfaces}
	for _, r := range cfg.Rules {
		lists = append(lists, r.Interfaces)
	}
	for _, list := range lists {
		if s.direct[listKey(list)] == nil {
			s.direct[listKey(list)] = p.newDirectTransports(s, list)
		}
	}
	return s
}

// newDirectTransports builds the direct transports of one interface list.
func (p *Proxy) newDirectTransports(s *snapshot, list []string) *directTransports {
	cfg := s.cfg
	dt := &directTransports{list: list}
	for _, class := range sourceClasses {
		dt.tr[class] = &http.Transport{
			Proxy: nil, // never http.ProxyFromEnvironment
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				ex := exchangeFrom(ctx)
				if ex == nil {
					return nil, errors.New("proxy: dial without a decision")
				}
				if ex.d.Source != class {
					// Never reached: forward picks the transport by the
					// exchange's class. Fail closed if that ever changes.
					return nil, fmt.Errorf("proxy: %s dial on the %s transport", ex.d.Source, class)
				}
				c, info, err := s.dialer.Dial(ctx, ex.d)
				if err != nil {
					return nil, err
				}
				return p.newOutConn(c, info), nil
			},
			DisableCompression:    true,
			ForceAttemptHTTP2:     false,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: cfg.Proxy.ResponseTimeout,
			ExpectContinueTimeout: time.Second,
		}
		dt.rp[class] = &httputil.ReverseProxy{
			Rewrite:        rewriteDirect,
			Transport:      dt.tr[class],
			ModifyResponse: modifyResponse,
			ErrorHandler:   p.forwardError,
			ErrorLog:       slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
		}
	}
	return dt
}

// closeIdleDirect closes the idle pooled connections of every direct
// transport.
func (s *snapshot) closeIdleDirect() {
	for _, dt := range s.direct {
		for _, t := range dt.tr {
			t.CloseIdleConnections()
		}
	}
}

// closeIdle closes the idle pooled connections of every direct and
// upstream transport.
func (s *snapshot) closeIdle() {
	s.closeIdleDirect()
	for _, u := range s.upstreams {
		u.transport.CloseIdleConnections()
	}
}

// reconnect closes the idle connections of every direct and upstream
// transport whose list holds the interface, when it comes online or its
// demotion ends with cause state_changed or connected, so the next
// plain-HTTP request plans again (design 0008 D12).
func (p *Proxy) reconnect(name string) {
	s := p.snap.Load()
	if s == nil {
		return
	}
	for _, dt := range s.direct {
		if slices.Contains(dt.list, name) {
			for _, t := range dt.tr {
				t.CloseIdleConnections()
			}
		}
	}
	for _, u := range s.upstreams {
		if slices.Contains(u.cfg.Interfaces, name) {
			u.transport.CloseIdleConnections()
		}
	}
}

// InterfaceStatus returns the state of every interface definition in
// configuration order (design 0008 D15).
func (p *Proxy) InterfaceStatus() []iface.Status { return p.ifm.Status() }

// upstreamKept reports whether a reload keeps an upstream's runtime state:
// its settings, interfaces included, are unchanged, and so are the mac,
// device, and kind of each definition its list names (design 0004 D6, 0008
// D14).
func upstreamKept(prev *upstream, u config.Upstream, oldCfg, newCfg *config.Config) bool {
	if prev == nil || !prev.cfg.Equal(u) {
		return false
	}
	for _, name := range u.Interfaces {
		a, okA := oldCfg.Interface(name)
		b, okB := newCfg.Interface(name)
		if okA != okB || !a.Def().Same(b.Def()) {
			return false
		}
	}
	return true
}

// Reload applies a validated configuration (design 0004 D6 steps 2-4). It
// keeps the runtime state of every upstream whose settings are unchanged:
// its transport with the pooled connections and its unreachable hold. A
// changed or new upstream starts afresh in state up; a replaced or removed
// one stops publishing its state and closes its idle connections. The new
// snapshot gets new direct transports, and the old ones close their idle
// connections. Requests already decided keep their snapshot, and tunnels
// are never re-evaluated or closed. Listener changes wait for the next
// start (design 0003 D9).
func (p *Proxy) Reload(cfg *config.Config) error {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	if err := p.ifm.Check(cfg.Defs()); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	old := p.snap.Load()
	ups := make(map[string]*upstream, len(cfg.Upstreams))
	var fresh []*upstream
	for _, u := range cfg.Upstreams {
		if prev := old.upstreams[u.Name]; upstreamKept(prev, u, old.cfg, cfg) {
			ups[u.Name] = prev
			continue
		}
		up, err := p.newUpstream(u)
		if err != nil {
			return err
		}
		ups[u.Name] = up
		fresh = append(fresh, up)
	}
	if err := checkRoutes(cfg, ups); err != nil {
		return err
	}
	// The table is read again before the new snapshot is used (design
	// 0008 D14).
	if err := p.ifm.Configure(cfg.Defs(), cfg.Proxy.InterfaceDemotion); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	p.snap.Store(p.newSnapshot(cfg, ups))
	for name, prev := range old.upstreams {
		if ups[name] == prev {
			continue
		}
		prev.retire()
		if ups[name] == nil {
			for _, st := range upstreamStates {
				p.live.Delete(stats.UpstreamState, "upstream", name, "state", st)
			}
		}
	}
	// After the old states stop publishing, so the gauges show the new one.
	for _, up := range fresh {
		up.hold.publish()
	}
	old.closeIdleDirect()
	return nil
}

// Live returns the live counters.
func (p *Proxy) Live() *stats.Live { return p.live }

// Serve serves every listener until Shutdown, and binds and serves each
// retried listener once it can bind. It returns the first serving error
// other than http.ErrServerClosed.
func (p *Proxy) Serve() error {
	p.lmu.Lock()
	for _, st := range p.lstates {
		if st.ln != nil {
			p.serveLocked(st.ln)
		} else {
			p.serving.Add(1)
			go p.retry(st)
		}
	}
	p.lmu.Unlock()
	p.serving.Wait()
	p.lmu.Lock()
	defer p.lmu.Unlock()
	return p.serveErr
}

// serveLocked serves one bound listener. The caller holds lmu.
func (p *Proxy) serveLocked(l net.Listener) {
	p.serving.Add(1)
	go func() {
		defer p.serving.Done()
		err := p.srv.Serve(&listener{Listener: l, p: p})
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.lmu.Lock()
			if p.serveErr == nil {
				p.serveErr = err
			}
			p.lmu.Unlock()
		}
	}()
}

// retry binds a listener that failed at startup every RetryInterval until
// it binds or shutdown starts, then serves it (design 0003 D9).
func (p *Proxy) retry(st *listenerState) {
	defer p.serving.Done()
	tick := time.NewTicker(p.deps.RetryInterval)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
		l, err := p.deps.Bind("tcp", st.addr)
		p.lmu.Lock()
		if err != nil {
			st.err = err.Error()
			p.lmu.Unlock()
			p.log.Debug("listener still retrying", "address", st.addr, "error", err.Error())
			continue
		}
		select {
		case <-p.done:
			p.lmu.Unlock()
			l.Close()
			return
		default:
		}
		st.state, st.err, st.since, st.ln = ListenerBound, "", time.Now(), l
		p.addBoundLocked(l)
		p.serveLocked(l)
		p.lmu.Unlock()
		p.log.Info("listener bound", "address", st.addr, "listener", l.Addr().String())
		return
	}
}

// Shutdown closes the listeners and idle connections, stops retrying
// listeners, waits for in-flight plain-HTTP exchanges until ctx ends, then
// closes every remaining connection, tunnels and upgraded connections
// included, with outcome shutdown (design 0006 D6). Before it returns it
// waits, for at most handlerWait, until every handler has returned, so
// their decision lines are written before the caller logs stopped and
// closes the log.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
	err := p.srv.Shutdown(ctx)
	if err != nil {
		p.forced.Store(true)
		p.srv.Close()
	}
	p.mu.Lock()
	ts := make([]tracked, 0, len(p.tunnels))
	for t := range p.tunnels {
		ts = append(ts, t)
	}
	p.mu.Unlock()
	for _, t := range ts {
		t.shutdown()
	}
	if n := p.handlers.wait(handlerWait); n > 0 {
		p.log.Warn("shutdown: handlers still running", "count", n, "waited", handlerWait.String())
	}
	p.snap.Load().closeIdle()
	p.ifm.Stop()
	p.warn.stop()
	return err
}

// MaxTunnelList bounds the tunnels listed in status (design 0002 D7).
const MaxTunnelList = 500

// TunnelStatus is one active tunnel or upgraded connection for status
// displays (design 0002 D7, 0006 D4).
type TunnelStatus struct {
	Target   string // normalized authority
	Kind     string // "connect", or "upgrade" for a connection upgraded after a 101
	Action   string // direct or upstream
	Upstream string
	Source   string // local or remote
	// Interface is the decision-log value of design 0008 D15: a configured
	// interface name, other, or empty.
	Interface string
	Started   time.Time
	Up, Down  int64
}

// Tunnels returns up to MaxTunnelList active tunnels and upgraded
// connections, oldest first.
func (p *Proxy) Tunnels() []TunnelStatus {
	p.mu.Lock()
	out := make([]TunnelStatus, 0, len(p.tunnels))
	for t := range p.tunnels {
		out = append(out, t.status())
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	if len(out) > MaxTunnelList {
		out = out[:MaxTunnelList]
	}
	return out
}

// Close stops the proxy at once.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return p.Shutdown(ctx)
}

// warnSource logs a rejected source, at most once per minute per address
// and within the warning budget (design 0003 D10, D12).
func (p *Proxy) warnSource(a netip.Addr) {
	p.warn.warn(warnKey{a, "source"}, "rejected source", "source", a.String(), "reason", "not in proxy.remote.sources")
}

// warnLimit logs a connection over a limit, at most once per minute per
// address and within the warning budget (design 0003 D12).
func (p *Proxy) warnLimit(a netip.Addr, key string, limit int) {
	p.warn.warn(warnKey{a, "limit"}, "connection limit reached", "source", a.String(), "limit", key, "max", strconv.Itoa(limit))
}

// attemptTimeout is proxy.interface_attempt_timeout of the configuration
// in effect (design 0008 D8).
func (p *Proxy) attemptTimeout() time.Duration {
	if s := p.snap.Load(); s != nil {
		return s.cfg.Proxy.InterfaceAttemptTimeout
	}
	return 3 * time.Second
}
