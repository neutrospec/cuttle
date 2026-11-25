package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"squid/internal/authority"
	"squid/internal/config"
	"squid/internal/logx"
	"squid/internal/netx"
	"squid/internal/stats"
)

// Upstream states (design 0003 D8).
const (
	StateUp      = "up"
	StateDown    = "down"
	StateProbing = "probing"
)

var upstreamStates = [...]string{StateUp, StateDown, StateProbing}

// maxCopiedProxyStatus bounds the Proxy-Status field value copied from an
// upstream's CONNECT answer (design 0003 D6).
const maxCopiedProxyStatus = 1 << 10

// upstream is the runtime state of one [[upstream]]: its dialer, its
// unreachable hold, and the transport that pools plain-HTTP connections to
// it across clients (design 0003 D1, D4, D8).
type upstream struct {
	cfg       config.Upstream
	target    authority.Authority
	dialer    *netx.UpstreamDialer
	hold      *hold
	transport *http.Transport
	rp        *httputil.ReverseProxy
}

func (p *Proxy) newUpstream(u config.Upstream) (*upstream, error) {
	target, err := authority.ParseConnect(u.Address)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: address %q: %w", u.Name, u.Address, err)
	}
	up := &upstream{
		cfg:    u,
		target: target,
		dialer: &netx.UpstreamDialer{
			Network: p.deps.Network,
			Timeout: u.ConnectTimeout,
			Stagger: dialStagger,
			Self:    p.isListener,
		},
	}
	up.hold = &hold{
		name:    u.Name,
		address: u.Address,
		period:  u.UnreachableHold,
		now:     p.now,
		log:     p.log,
		live:    p.live,
		state:   StateUp,
		since:   p.now(),
	}
	up.hold.publishLocked()
	up.transport = &http.Transport{
		// Fixed to the upstream: the transport writes absolute-form and
		// pools connections to the upstream across clients and hosts. The
		// URL carries no userinfo, so no Proxy-Authorization is sent.
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: u.Address}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return up.dialHTTP(ctx)
		},
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: u.ResponseTimeout,
		ExpectContinueTimeout: time.Second,
	}
	// A pooled connection to an upstream that went down would only fail
	// later requests slowly (design 0003 D4, D8).
	up.hold.onDown = up.transport.CloseIdleConnections
	up.rp = &httputil.ReverseProxy{
		Rewrite:        rewriteUpstream,
		Transport:      up.transport,
		ModifyResponse: modifyResponse,
		ErrorHandler:   p.forwardError,
		ErrorLog:       slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
	}
	return up, nil
}

// dialHTTP is the plain-HTTP transport's dial function. It applies the
// hold: the handler already admitted the request, and a probe ticket lets
// exactly one dial through after a hold; any other dial is admitted again,
// because the upstream may have gone down since (design 0003 D4, D8).
func (u *upstream) dialHTTP(ctx context.Context) (net.Conn, error) {
	ex := exchangeFrom(ctx)
	if ex == nil {
		return nil, errors.New("proxy: upstream dial without a decision")
	}
	probe := false
	if tk := ex.ticket; tk != nil && tk.probe && tk.used.CompareAndSwap(false, true) {
		probe = true
	} else {
		var held *heldError
		if probe, held = u.hold.admit(); held != nil {
			return nil, held
		}
	}
	c, err := u.dialer.Dial(ctx, u.target)
	u.hold.done(probe, err)
	return c, err
}

// settle resolves a plain-HTTP probe that never dialed, because the
// transport served it on a pooled connection. A relayed response shows that
// the upstream is reachable; otherwise the upstream is due for another
// probe.
func (u *upstream) settle(ex *exchange) {
	if tk := ex.ticket; tk != nil && tk.probe && tk.used.CompareAndSwap(false, true) {
		u.hold.settle(ex.relayed)
	}
}

// rewriteUpstream prepares a plain-HTTP request for an upstream. The
// upstream transport writes the client's absolute-form target unchanged.
// Toward disclosure = "none" the disclosure filter runs; toward
// disclosure = "via" the client's fields pass and cuttle adds its Via entry
// (design 0002 D5, 0003 D2, D4, D5).
func rewriteUpstream(pr *httputil.ProxyRequest) {
	ex := exchangeFrom(pr.In.Context())
	t := ex.http
	u := pr.Out.URL
	u.Scheme = "http"
	u.Host = t.Authority.String()
	u.Path, u.RawPath = "", ""
	// RequestURI() yields "http:" + Opaque for a "//" Opaque value, so the
	// transport writes http://<raw authority><raw path>?<raw query>.
	u.Opaque = "//" + t.RawAuthority + t.RawPath
	u.RawQuery = t.RawQuery
	u.ForceQuery = t.HasQuery && t.RawQuery == ""
	pr.Out.Host = pr.In.Host
	up := ex.snap.upstreams[ex.d.Upstream]
	if up.cfg.Disclosure == config.DisclosureNone {
		removeDisclosure(pr.Out.Header)
		return
	}
	// ReverseProxy drops these client fields before Rewrite; on via routes
	// they pass as on the CONNECT path.
	for _, k := range rewriteDropped {
		if vs, ok := pr.In.Header[k]; ok {
			pr.Out.Header[k] = append([]string(nil), vs...)
		}
	}
	pr.Out.Header.Add("Via", viaEntry(ex.snap.cfg.Name))
}

// rewriteDropped are the fields httputil.ReverseProxy removes before calling
// Rewrite (reverseproxy.go, Go 1.27.4).
var rewriteDropped = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}

// connectUpstream serves a CONNECT routed to an upstream (design 0003 D3):
// check the hold, dial, write the head, read the answer, and either
// establish the tunnel with exactly 200 or answer per D6.
func (p *Proxy) connectUpstream(w http.ResponseWriter, r *http.Request, ex *exchange, u *upstream) {
	probe, held := u.hold.admit()
	if held != nil {
		p.replyHeld(w, ex, held)
		return
	}
	server, err := u.dialer.Dial(r.Context(), u.target)
	u.hold.done(probe, err)
	if err != nil {
		p.upstreamDialFailure(w, ex, err)
		return
	}
	head := connectHead(r, ex.snap.cfg.Name, u.cfg.Disclosure)
	br := bufio.NewReader(server)
	server.SetDeadline(time.Now().Add(u.cfg.ResponseTimeout))
	// A client that leaves while the upstream is silent ends the wait.
	stop := context.AfterFunc(r.Context(), func() { server.SetDeadline(time.Unix(1, 0)) })
	var res *http.Response
	if _, err = server.Write(head); err == nil {
		res, err = http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	}
	stop()
	if r.Context().Err() != nil {
		server.Close()
		ex.outcome = OutcomeClientClosed
		p.finish(ex)
		return
	}
	if err != nil {
		server.Close()
		p.upstreamResponseFailure(w, ex, u, err)
		return
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		server.Close()
		p.upstreamStatus(w, ex, u, res)
		return
	}
	server.SetDeadline(time.Time{})
	// Any 2xx: ignore Content-Length and Transfer-Encoding and never read a
	// body. Bytes the upstream sent after its head go to the client first.
	var early []byte
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		early = bytes.Clone(b)
	}
	p.establish(w, ex, server, early)
}

// connectHead writes the upstream CONNECT head from the client's request
// (design 0003 D3): the target as the client sent it, Host set to it, and
// the client's other fields minus hop-by-hop fields, with the disclosure
// rule of the upstream's mode applied.
func connectHead(r *http.Request, name, disclosure string) []byte {
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	removeHopByHop(h)
	delete(h, "Host")
	if disclosure == config.DisclosureNone {
		removeDisclosure(h)
	} else {
		h.Add("Via", viaEntry(name))
	}
	var b bytes.Buffer
	b.WriteString("CONNECT " + r.RequestURI + " HTTP/1.1\r\nHost: " + r.RequestURI + "\r\n")
	h.Write(&b) // sorted by name
	b.WriteString("\r\n")
	return b.Bytes()
}

// hopByHop are the fields httputil.ReverseProxy removes as hop-by-hop
// (reverseproxy.go, Go 1.27.4), in canonical form.
var hopByHop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade"}

// removeHopByHop deletes Connection, the fields it names, and the other
// hop-by-hop fields, as ReverseProxy does.
func removeHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, f := range strings.Split(v, ",") {
			if f = textproto.TrimString(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

// upstreamDialFailure answers a failed upstream dial (design 0003 D6). A
// dial abandoned because the client left gets no response.
func (p *Proxy) upstreamDialFailure(w http.ResponseWriter, ex *exchange, err error) {
	var held *heldError
	if errors.As(err, &held) {
		p.replyHeld(w, ex, held)
		return
	}
	var de *netx.DialError
	if !errors.As(err, &de) {
		p.reply(w, ex, http.StatusInternalServerError, "proxy_internal_error", "upstream dial failed", "internal error", OutcomeInternal)
		return
	}
	if de.ClientGone {
		ex.outcome, ex.detail = OutcomeClientClosed, de.Detail
		p.finish(ex)
		return
	}
	up := ex.d.Upstream
	switch de.Type {
	case netx.ErrDNS:
		p.replyHop(w, ex, http.StatusBadGateway, de.Type, up, de.Detail, "cannot resolve upstream "+up, OutcomeDNSError)
	case netx.ErrLoop:
		p.replyHop(w, ex, http.StatusBadGateway, de.Type, up, de.Detail, "upstream "+up+" is this instance", OutcomeLoop)
	case netx.ErrTimeout:
		p.replyHop(w, ex, http.StatusGatewayTimeout, de.Type, up, de.Detail, "connection to upstream "+up+" timed out", OutcomeTimeout)
	default:
		p.replyHop(w, ex, http.StatusBadGateway, netx.ErrRefused, up, de.Detail, "connection to upstream "+up+" refused", OutcomeRefused)
	}
}

// upstreamResponseFailure answers a failure after the upstream connected
// and before its CONNECT answer was read (design 0003 D6).
func (p *Proxy) upstreamResponseFailure(w http.ResponseWriter, ex *exchange, u *upstream, err error) {
	up := u.cfg.Name
	switch {
	case isTimeout(err):
		p.replyHop(w, ex, http.StatusGatewayTimeout, "http_response_timeout", up, "no response head within "+u.cfg.ResponseTimeout.String(),
			"no response from upstream "+up, OutcomeTimeout)
	case isConnClosed(err):
		p.replyHop(w, ex, http.StatusBadGateway, netx.ErrRefused, up, "connection closed before the response",
			"connection closed by upstream "+up, OutcomeRefused)
	default:
		p.replyHop(w, ex, http.StatusBadGateway, "http_protocol_error", up, "unparsable response",
			"unparsable response from upstream "+up, OutcomeUpstreamProto)
	}
}

func isConnClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}

// upstreamStatus answers a non-2xx CONNECT answer: 504 stays 504, anything
// else, 407 included, becomes 502. The upstream's Proxy-Status members are
// copied and cuttle's own member comes last (design 0003 D6). Nothing else
// of the answer is relayed, so no Proxy-Authenticate reaches the client.
func (p *Proxy) upstreamStatus(w http.ResponseWriter, ex *exchange, u *upstream, res *http.Response) {
	status := http.StatusBadGateway
	if res.StatusCode == http.StatusGatewayTimeout {
		status = http.StatusGatewayTimeout
	}
	own := receivedMember(ex.snap.cfg.Name, res.StatusCode, u.cfg.Name)
	member := chainProxyStatus(res.Header.Values("Proxy-Status"), own)
	p.writeGenerated(w, status, member, fmt.Sprintf("upstream %s answered %d (%s)", u.cfg.Name, res.StatusCode, ex.target))
	ex.status, ex.outcome = status, OutcomeUpstreamStatus
	ex.detail = fmt.Sprintf("received-status=%d", res.StatusCode)
	p.finish(ex)
}

// receivedMember is cuttle's Proxy-Status member for a status received from
// the next hop; nextHop is empty for an origin.
func receivedMember(name string, code int, nextHop string) string {
	m := fmt.Sprintf("%s; received-status=%d", name, code)
	if nextHop != "" {
		m += "; next-hop=" + sfString(nextHop)
	}
	return m
}

// chainProxyStatus returns the Proxy-Status value for a CONNECT failure:
// the upstream's members in their order, as many whole members as fit in
// 1 KiB of field value, then cuttle's own member last, as RFC 9209 lists
// the member closest to the origin first (design 0003 D6).
func chainProxyStatus(upstream []string, own string) string {
	var members []string
	n := 0
copy:
	for _, v := range upstream {
		for _, m := range splitMembers(v) {
			add := len(m)
			if len(members) > 0 {
				add += 2 // ", "
			}
			if n+add > maxCopiedProxyStatus {
				break copy
			}
			members = append(members, m)
			n += add
		}
	}
	return strings.Join(append(members, own), ", ")
}

// splitMembers splits a structured-field list on commas outside quoted
// strings, trims each member, and drops bytes a field value cannot carry.
func splitMembers(v string) []string {
	var out []string
	var b strings.Builder
	quoted, escaped := false, false
	flush := func() {
		if m := strings.Trim(b.String(), " \t"); m != "" {
			out = append(out, m)
		}
		b.Reset()
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c > 0x7e {
			continue
		}
		switch {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			flush()
			continue
		}
		b.WriteByte(c)
	}
	flush()
	return out
}

// replyHeld answers a request to a held upstream at once (design 0003 D8).
func (p *Proxy) replyHeld(w http.ResponseWriter, ex *exchange, held *heldError) {
	up := ex.d.Upstream
	p.replyHop(w, ex, http.StatusServiceUnavailable, "destination_unavailable", up, "unreachable for "+holdAge(held.down),
		"upstream "+up+" is unreachable", OutcomeUnavailable)
}

// holdAge formats the time an upstream has been down, as in
// details="unreachable for 42s".
func holdAge(d time.Duration) string {
	if d < time.Second {
		return logx.Duration(d)
	}
	return d.Round(time.Second).String()
}

// heldError fails a request to a held upstream without a dial.
type heldError struct {
	down time.Duration // since the upstream was found down
}

func (e *heldError) Error() string { return "upstream held as unreachable" }

// ticket is a plain-HTTP request's admission to an upstream. A probe ticket
// is the single dial allowed after a hold expires.
type ticket struct {
	probe bool
	used  atomic.Bool
}

// hold is the unreachable hold of one upstream (design 0003 D8). Only a
// failure to reach the upstream itself starts it: a refused, reset,
// unreachable, or timed-out connect, or a failed lookup of the upstream's
// own name. During the hold every request fails at once. When it expires,
// the next request is the single probe while others keep failing; a
// successful connect, from any request, marks the upstream up.
type hold struct {
	name, address string
	period        time.Duration // 0 disables the hold
	now           func() time.Time
	log           *slog.Logger
	live          *stats.Live
	onDown        func()

	mu         sync.Mutex
	state      string
	since      time.Time // start of the current state
	downSince  time.Time // start of the outage
	until      time.Time // end of the current hold
	lastErr    string
	failedFast int64
}

// admit decides whether a request may dial. probe is true for the single
// request that probes after a hold expires; held is set when the request
// must fail at once.
func (h *hold) admit() (probe bool, held *heldError) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.period == 0 {
		return false, nil
	}
	now := h.now()
	switch h.state {
	case StateUp:
		return false, nil
	case StateDown:
		if !now.Before(h.until) {
			h.setLocked(StateProbing, now)
			return true, nil
		}
	}
	h.failedFast++
	return false, &heldError{down: now.Sub(h.downSince)}
}

// done records the result of a dial; probe tells whether it was the probe.
func (h *hold) done(probe bool, err error) {
	var closeIdle func()
	h.mu.Lock()
	defer func() {
		h.mu.Unlock()
		if closeIdle != nil {
			closeIdle()
		}
	}()
	now := h.now()
	if err == nil {
		h.upLocked(now)
		return
	}
	var de *netx.DialError
	unreachable := errors.As(err, &de) && !de.ClientGone && de.Type != netx.ErrLoop
	if unreachable {
		h.lastErr = de.Type
	}
	if !unreachable || h.period == 0 {
		// Not a reachability failure, or the hold is disabled: a probe that
		// ended this way leaves the upstream due for another probe.
		if probe && h.state == StateProbing {
			h.setLocked(StateDown, now)
			h.until = now
		}
		return
	}
	switch {
	case h.state == StateUp:
		h.downSince, h.until, h.failedFast = now, now.Add(h.period), 0
		h.setLocked(StateDown, now)
		h.log.Warn("upstream unreachable", "upstream", h.name, "address", h.address, "error", de.Type, "hold", h.period.String())
		closeIdle = h.onDown
	case probe && h.state == StateProbing:
		h.until = now.Add(h.period)
		h.setLocked(StateDown, now)
	}
	// A dial that started before the hold and failed during it changes
	// nothing.
}

// settle resolves a probe that never dialed.
func (h *hold) settle(reached bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != StateProbing {
		return
	}
	now := h.now()
	if reached {
		h.upLocked(now)
		return
	}
	h.setLocked(StateDown, now)
	h.until = now
}

func (h *hold) upLocked(now time.Time) {
	if h.state == StateUp {
		return
	}
	h.log.Info("upstream recovered", "upstream", h.name, "address", h.address,
		"down", logx.Duration(now.Sub(h.downSince)), "failed_fast", h.failedFast)
	h.setLocked(StateUp, now)
}

func (h *hold) setLocked(state string, now time.Time) {
	h.state, h.since = state, now
	h.publishLocked()
}

// publishLocked sets the upstream_state gauges: 1 for the current state and
// 0 for the others.
func (h *hold) publishLocked() {
	for _, s := range upstreamStates {
		v := int64(0)
		if s == h.state {
			v = 1
		}
		h.live.Set(v, stats.UpstreamState, "upstream", h.name, "state", s)
	}
}

// UpstreamStatus is one upstream's hold state for status displays (design
// 0003 D8).
type UpstreamStatus struct {
	Name, Address string
	State         string // StateUp, StateDown, or StateProbing
	Since         time.Time
	LastError     string // Proxy-Status error type of the last failed connect
	FailedFast    int64  // requests failed at once during the current or last outage
}

func (h *hold) status() UpstreamStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return UpstreamStatus{Name: h.name, Address: h.address, State: h.state, Since: h.since, LastError: h.lastErr, FailedFast: h.failedFast}
}
