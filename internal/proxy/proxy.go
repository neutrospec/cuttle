// Package proxy serves the proxy port: the listener wrapper, the root
// handler, tunnels, plain-HTTP forwarding, cuttle's own responses, live
// counters, and the decision log (design 0002 D2-D8).
//
// M1.1 serves local clients on loopback listeners with direct and block
// routes. Remote sources, upstream routes, reload, and the control socket
// arrive in later stages; New refuses configurations that need them.
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

	mu       sync.Mutex
	tunnels  map[*tunnel]struct{}
	warned   map[netip.Addr]time.Time
	closing  bool
	serveErr chan error
}

// snapshot is an immutable policy snapshot with its own direct transport,
// so a pooled connection is only reused under the policy that allowed its
// dial (design 0002 D5).
type snapshot struct {
	cfg    *config.Config
	policy *policy.Policy
	dialer *netx.Dialer
	direct *http.Transport
	rp     *httputil.ReverseProxy
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
		warned:  map[netip.Addr]time.Time{},
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

// CheckSupported refuses settings that this version cannot serve:
// non-loopback listeners need the remote-client policy of M1.2, and upstream
// routes need M1.3. A configuration may still declare upstreams that no
// local route uses.
func CheckSupported(cfg *config.Config) error {
	for _, l := range cfg.Proxy.Listen {
		if !netx.IsLoopbackListener(l) {
			return fmt.Errorf("proxy.listen: %s: listeners for remote clients are %w", l, ErrUnsupported)
		}
	}
	if cfg.Proxy.Default.Action == policy.ActionUpstream {
		return fmt.Errorf("proxy.default: %s: upstream routes are %w", cfg.Proxy.Default, ErrUnsupported)
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
	s.direct = &http.Transport{
		Proxy: nil, // never http.ProxyFromEnvironment
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ex := exchangeFrom(ctx)
			if ex == nil {
				return nil, errors.New("proxy: dial without a decision")
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
	s.rp = &httputil.ReverseProxy{
		Rewrite:        rewriteDirect,
		Transport:      s.direct,
		ModifyResponse: modifyResponse,
		ErrorHandler:   p.forwardError,
		ErrorLog:       slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
	}
	return s
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
	p.snap.Load().direct.CloseIdleConnections()
	return err
}

// Close stops the proxy at once.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return p.Shutdown(ctx)
}

// warnSource logs a rejected source at most once per minute per address.
func (p *Proxy) warnSource(a netip.Addr) {
	p.mu.Lock()
	last, seen := p.warned[a]
	now := time.Now()
	if seen && now.Sub(last) < time.Minute {
		p.mu.Unlock()
		return
	}
	p.warned[a] = now
	p.mu.Unlock()
	p.log.Warn("rejected source", "source", a.String(), "reason", "remote clients are not supported in this version")
}
