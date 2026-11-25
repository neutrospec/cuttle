// Package proxy serves the proxy port: the listener wrapper, the root
// handler, tunnels, plain-HTTP forwarding, cuttle's own responses, live
// counters, and the decision log (design 0002 D2-D8).
//
// M1.2 serves local clients and allowlisted remote clients with direct and
// block routes, on loopback and non-loopback listeners (design 0003 D9-D12).
// Upstream routes, reload, and the control socket arrive in later stages;
// New refuses configurations that need upstream routes.
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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"squid/internal/config"
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
)

// Deps are the proxy's injected dependencies. Network and SelfAddrs are
// required: internal packages have no default network (design 0007 D3).
type Deps struct {
	Network   netx.Network
	SelfAddrs func() []netip.Addr
	// Listeners are the proxy listeners, already bound.
	Listeners []net.Listener
	// Logger receives decision lines at info and server warnings.
	Logger *slog.Logger
	// Live receives the live counters; New creates one when nil.
	Live *stats.Live
}

// Proxy is one running proxy instance.
type Proxy struct {
	deps Deps
	log  *slog.Logger
	live *stats.Live
	snap atomic.Pointer[snapshot]
	srv  *http.Server

	conns connLimits
	warn  *warnLimiter

	mu       sync.Mutex
	tunnels  map[*tunnel]struct{}
	closing  bool
	serveErr chan error
}

// sourceClasses lists the client classes; each has its own direct
// transport.
var sourceClasses = [...]policy.SourceClass{policy.SourceLocal, policy.SourceRemote}

// snapshot is an immutable policy snapshot with a direct transport per
// client class, so a pooled connection is only reused for the same
// snapshot, class, host, and port, under the policy that allowed its dial
// (design 0002 D5). Without the class split, a connection dialed for a
// local client, for example to a private address through allow_private,
// would carry a remote client's request to the same host and port.
type snapshot struct {
	cfg    *config.Config
	policy *policy.Policy
	dialer *netx.Dialer
	direct [len(sourceClasses)]*http.Transport
	rp     [len(sourceClasses)]*httputil.ReverseProxy
}

// ErrUnsupported marks configurations that need a later stage.
var ErrUnsupported = errors.New("not supported in this version")

// New builds a proxy for cfg. It does not start serving.
func New(cfg *config.Config, deps Deps) (*Proxy, error) {
	if deps.Network == nil {
		return nil, errors.New("proxy: Deps.Network is required")
	}
	if deps.SelfAddrs == nil {
		return nil, errors.New("proxy: Deps.SelfAddrs is required")
	}
	if len(deps.Listeners) == 0 {
		return nil, errors.New("proxy: no listeners")
	}
	if err := CheckSupported(cfg); err != nil {
		return nil, err
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Live == nil {
		deps.Live = stats.NewLive()
	}
	p := &Proxy{
		deps:    deps,
		log:     deps.Logger,
		live:    deps.Live,
		tunnels: map[*tunnel]struct{}{},
		warn:    newWarnLimiter(deps.Logger),
	}
	p.snap.Store(p.newSnapshot(cfg))
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

// CheckSupported refuses settings that this version cannot serve: upstream
// routes need M1.3, for local and remote clients alike. A configuration may
// still declare upstreams that no route uses.
func CheckSupported(cfg *config.Config) error {
	if cfg.Proxy.Default.Action == policy.ActionUpstream {
		return fmt.Errorf("proxy.default: %s: upstream routes are %w", cfg.Proxy.Default, ErrUnsupported)
	}
	if cfg.Proxy.Remote.Default.Action == policy.ActionUpstream {
		return fmt.Errorf("proxy.remote.default: %s: upstream routes are %w", cfg.Proxy.Remote.Default, ErrUnsupported)
	}
	for i, r := range cfg.Rules {
		if r.Route.Action == policy.ActionUpstream {
			return fmt.Errorf("rule[%d].action: %s: upstream routes are %w", i+1, r.Route, ErrUnsupported)
		}
	}
	return nil
}

func (p *Proxy) newSnapshot(cfg *config.Config) *snapshot {
	s := &snapshot{
		cfg:    cfg,
		policy: policy.New(cfg.PolicySettings()),
		dialer: &netx.Dialer{
			Network:   p.deps.Network,
			SelfAddrs: p.deps.SelfAddrs,
			Timeout:   cfg.Proxy.ConnectTimeout,
			Stagger:   dialStagger,
		},
	}
	for _, class := range sourceClasses {
		s.direct[class] = &http.Transport{
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
				return s.dialer.Dial(ctx, ex.d)
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
		s.rp[class] = &httputil.ReverseProxy{
			Rewrite:        rewriteDirect,
			Transport:      s.direct[class],
			ModifyResponse: modifyResponse,
			ErrorHandler:   p.forwardError,
			ErrorLog:       slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
		}
	}
	return s
}

// closeIdle closes the idle pooled connections of every direct transport.
func (s *snapshot) closeIdle() {
	for _, t := range s.direct {
		t.CloseIdleConnections()
	}
}

// Live returns the live counters.
func (p *Proxy) Live() *stats.Live { return p.live }

// Serve serves every listener until Shutdown. It returns the first serving
// error other than http.ErrServerClosed.
func (p *Proxy) Serve() error {
	errc := make(chan error, len(p.deps.Listeners))
	for _, l := range p.deps.Listeners {
		go func(l net.Listener) { errc <- p.srv.Serve(&listener{Listener: l, p: p}) }(l)
	}
	var first error
	for range p.deps.Listeners {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) && first == nil {
			first = err
		}
	}
	return first
}

// Shutdown closes the listeners and idle connections, waits for in-flight
// plain-HTTP exchanges until ctx ends, then closes every tunnel and
// remaining connection with outcome shutdown (design 0006 D6).
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	err := p.srv.Shutdown(ctx)
	if err != nil {
		p.srv.Close()
	}
	p.mu.Lock()
	ts := make([]*tunnel, 0, len(p.tunnels))
	for t := range p.tunnels {
		ts = append(ts, t)
	}
	p.mu.Unlock()
	for _, t := range ts {
		t.shutdown()
	}
	p.snap.Load().closeIdle()
	p.warn.stop()
	return err
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
