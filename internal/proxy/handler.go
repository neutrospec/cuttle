package proxy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"squid/internal/authority"
	"squid/internal/logx"
	"squid/internal/netx"
	"squid/internal/policy"
	"squid/internal/stats"
)

// Outcomes logged and counted (design 0002 D6 and D7).
const (
	OutcomeOK             = "ok"
	OutcomeBadRequest     = "bad_request"
	OutcomeBlocked        = "blocked"
	OutcomeDNSError       = "dns_error"
	OutcomeProhibited     = "prohibited"
	OutcomeRefused        = "refused"
	OutcomeTimeout        = "timeout"
	OutcomeUpstreamStatus = "upstream_status"
	OutcomeUpstreamProto  = "upstream_protocol"
	OutcomeLoop           = "loop"
	OutcomeUnavailable    = "unavailable"
	OutcomeInternal       = "internal"
	OutcomeClientClosed   = "client_closed"
	OutcomeShutdown       = "shutdown"
)

// exchange is the state of one decision, from the request head to its
// decision line.
type exchange struct {
	id     string
	start  time.Time
	client string
	source policy.SourceClass
	// unclassified marks a request on a connection that the listener did
	// not classify; its decision line says source=unknown.
	unclassified bool
	kind         policy.Kind
	snap         *snapshot

	target  string // normalized authority, empty when the target was refused
	http    authority.HTTPTarget
	d       policy.Decision
	decided bool
	// ticket is the admission of a plain-HTTP request to its upstream, and
	// relayed records that a response from the next hop reached the client.
	ticket  *ticket
	relayed bool
	// out is the outbound connection of a plain-HTTP exchange, which counts
	// up (design 0002 D7).
	out atomic.Pointer[outConn]
	// writes counts socket writes in progress on the exchange's outbound
	// connection and on its client connection after a 101, so the decision
	// line of an upgraded connection waits for their counts.
	writes atomic.Int64

	outcome string
	status  int
	// upstreamStatus is the status the next hop answered with, when
	// cuttle's own status was mapped from it: a non-2xx CONNECT answer, or
	// a plain-HTTP 407 from an upstream or an origin (design 0002 D8).
	// Every decision with outcome upstream_status carries it.
	upstreamStatus int
	errType        string
	detail         string
	up             atomic.Int64
	down           atomic.Int64
}

type exchangeKey struct{}

func exchangeFrom(ctx context.Context) *exchange {
	ex, _ := ctx.Value(exchangeKey{}).(*exchange)
	return ex
}

func newID() string {
	var b [4]byte
	v := rand.Uint32()
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
	return hex.EncodeToString(b[:])
}

// ServeHTTP is the root handler: no ServeMux (design 0002 D2).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ex := &exchange{
		id:     newID(),
		start:  time.Now(),
		client: r.RemoteAddr,
		snap:   p.snap.Load(),
	}
	ex.kind = policy.KindHTTP
	if r.Method == http.MethodConnect {
		ex.kind = policy.KindConnect
	}
	tc, ok := r.Context().Value(connKey{}).(*trackedConn)
	if !ok {
		// Every served connection comes through listener.Accept, which
		// classifies its source. Without a class the request is refused,
		// never treated as local, and logged as an internal error.
		ex.unclassified = true
		p.log.Warn("request on a connection without a source class; refused", "client", r.RemoteAddr)
		p.reply(w, ex, http.StatusInternalServerError, "proxy_internal_error", "unknown source",
			"internal error: unknown source", OutcomeInternal)
		return
	}
	ex.source = tc.source
	if _, ok := classify(tc.peer, ex.snap.cfg.Proxy.Remote.Sources); !ok {
		// A reload removed the source after the connection was accepted.
		// The request gets no response and the connection closes; tunnels
		// are never re-evaluated (design 0003 D10, 0004 D6).
		p.warnSource(tc.peer)
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			c.Close()
			return
		}
		panic(http.ErrAbortHandler)
	}
	if ex.kind == policy.KindConnect {
		p.serveConnect(w, r, ex)
		return
	}
	p.serveHTTP(w, r, ex)
}

// decide runs the policy and records the decision on the exchange.
func (p *Proxy) decide(ex *exchange, t authority.Authority) {
	ex.target = t.String()
	ex.d = ex.snap.policy.Decide(t, ex.kind, ex.source)
	ex.decided = true
}

func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request, ex *exchange) {
	t, err := authority.ParseConnect(r.RequestURI)
	if err != nil {
		p.badRequest(w, ex, err)
		return
	}
	p.decide(ex, t)
	if p.loopDetected(w, r, ex) {
		return
	}
	switch ex.d.Action {
	case policy.ActionBlock:
		p.block(w, ex)
	case policy.ActionDirect:
		p.connectDirect(w, r, ex)
	default:
		p.connectUpstream(w, r, ex, ex.snap.upstreams[ex.d.Upstream])
	}
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request, ex *exchange) {
	t, err := authority.ParseHTTP(r.RequestURI)
	if err != nil {
		p.badRequest(w, ex, err)
		return
	}
	ex.http = t
	p.decide(ex, t.Authority)
	if p.loopDetected(w, r, ex) {
		return
	}
	switch ex.d.Action {
	case policy.ActionBlock:
		p.block(w, ex)
	case policy.ActionDirect:
		p.forward(w, r, ex, ex.snap.rp[ex.source])
	default:
		u := ex.snap.upstreams[ex.d.Upstream]
		// The hold is checked here, as for CONNECT, so a held upstream
		// answers at once even when the transport holds a pooled
		// connection (design 0003 D4, D8).
		probe, held := u.hold.admit()
		if held != nil {
			p.replyHeld(w, ex, held)
			return
		}
		ex.ticket = &ticket{probe: probe}
		defer u.settle(ex)
		p.forward(w, r, ex, u.rp)
	}
}

// loopDetected answers 502 proxy_loop_detected when a Via member names this
// instance, unless the policy blocks the request (design 0002 D4 step 6,
// 0003 D2).
func (p *Proxy) loopDetected(w http.ResponseWriter, r *http.Request, ex *exchange) bool {
	if ex.d.Action == policy.ActionBlock || !viaNames(r.Header, ex.snap.cfg.Name) {
		return false
	}
	p.reply(w, ex, http.StatusBadGateway, "proxy_loop_detected", "Via names this instance",
		"loop detected ("+ex.target+")", OutcomeLoop)
	return true
}

func (p *Proxy) badRequest(w http.ResponseWriter, ex *exchange, err error) {
	reason := "invalid request target"
	var ae *authority.Error
	if errors.As(err, &ae) {
		reason = ae.Reason
	}
	p.reply(w, ex, http.StatusBadRequest, "http_request_error", reason, "bad request: "+reason, OutcomeBadRequest)
}

func (p *Proxy) block(w http.ResponseWriter, ex *exchange) {
	p.reply(w, ex, http.StatusForbidden, "http_request_denied", "blocked by rule "+ex.d.Rule,
		fmt.Sprintf("blocked by rule %q (%s)", ex.d.Rule, ex.target), OutcomeBlocked)
}

// dialFailure answers a failed direct dial (design 0002 D6). A dial
// abandoned because the client left is logged as client_closed and gets no
// response.
func (p *Proxy) dialFailure(w http.ResponseWriter, ex *exchange, err error) {
	var de *netx.DialError
	if !errors.As(err, &de) {
		p.reply(w, ex, http.StatusInternalServerError, "proxy_internal_error", "dial failed", "internal error", OutcomeInternal)
		return
	}
	if de.ClientGone {
		ex.outcome, ex.detail = OutcomeClientClosed, de.Detail
		p.finish(ex)
		return
	}
	switch de.Type {
	case netx.ErrDNS:
		p.reply(w, ex, http.StatusBadGateway, de.Type, de.Detail, "cannot resolve "+ex.target, OutcomeDNSError)
	case netx.ErrProhibited:
		p.reply(w, ex, http.StatusBadGateway, de.Type, de.Detail, "destination address prohibited ("+ex.target+")", OutcomeProhibited)
	case netx.ErrTimeout:
		p.reply(w, ex, http.StatusGatewayTimeout, de.Type, de.Detail, "connection to "+ex.target+" timed out", OutcomeTimeout)
	default:
		p.reply(w, ex, http.StatusBadGateway, netx.ErrRefused, de.Detail, "connection refused ("+ex.target+")", OutcomeRefused)
	}
}

// reply writes a response generated by cuttle (design 0002 D6) and logs the
// decision. Only the normalized authority, the rule name, and upstream
// names are reflected.
func (p *Proxy) reply(w http.ResponseWriter, ex *exchange, status int, errType, details, body, outcome string) {
	p.replyHop(w, ex, status, errType, "", details, body, outcome)
}

// replyHop is reply with a next-hop parameter naming the upstream, for
// upstream failures (design 0002 D6, 0003 D6).
func (p *Proxy) replyHop(w http.ResponseWriter, ex *exchange, status int, errType, nextHop, details, body, outcome string) {
	member := ex.snap.cfg.Name + "; error=" + errType
	if nextHop != "" {
		member += "; next-hop=" + sfString(nextHop)
	}
	if details != "" {
		member += "; details=" + sfString(details)
	}
	p.writeGenerated(w, status, member, body)
	ex.status, ex.outcome, ex.errType, ex.detail = status, outcome, errType, details
	p.finish(ex)
}

func (p *Proxy) writeGenerated(w http.ResponseWriter, status int, proxyStatus, body string) {
	if cw, ok := w.(*countingWriter); ok {
		cw.generated = true
	}
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("Proxy-Status", proxyStatus)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Connection", "close")
	w.WriteHeader(status)
	fmt.Fprintf(w, "cuttle: %s\n", body)
}

// sfString quotes s as a structured-field string (RFC 8941), dropping bytes
// it cannot carry.
func sfString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// finish counts the decision and writes its decision line (design 0002 D8).
// The line never contains paths, queries, header values, or bodies.
func (p *Proxy) finish(ex *exchange) {
	action := "none"
	if ex.decided {
		action = ex.d.Action.String()
	}
	p.live.Add(1, stats.DecisionsTotal, "kind", ex.kind.String(), "action", action)
	p.live.Add(1, stats.OutcomesTotal, "outcome", ex.outcome)
	if ex.decided && ex.d.Action == policy.ActionUpstream {
		p.live.Add(1, stats.UpstreamOutcomesTotal, "upstream", ex.d.Upstream, "outcome", ex.outcome)
	}
	if !ex.decided {
		action = ""
	}
	attrs := []slog.Attr{
		slog.String("id", ex.id),
		slog.String("client", ex.client),
		slog.String("source", sourceLabel(ex)),
		slog.String("kind", ex.kind.String()),
		slog.String("target", ex.target),
		slog.String("rule", ex.d.Rule),
		slog.String("action", action),
		slog.String("upstream", ex.d.Upstream),
		slog.String("outcome", ex.outcome),
		slog.Int("status", ex.status),
	}
	if ex.upstreamStatus != 0 {
		attrs = append(attrs, slog.Int("upstream_status", ex.upstreamStatus))
	}
	attrs = append(attrs,
		slog.Int64("up", ex.up.Load()),
		slog.Int64("down", ex.down.Load()),
		slog.String("dur", logx.Duration(time.Since(ex.start))),
	)
	if ex.errType != "" {
		attrs = append(attrs, slog.String("error", ex.errType), slog.String("detail", ex.detail))
	}
	p.log.LogAttrs(context.Background(), slog.LevelInfo, "decision", attrs...)
}

// addBytes counts relayed bytes for the exchange and the live counters as
// each write completes.
func (p *Proxy) addBytes(ex *exchange, direction string, n int) {
	if n <= 0 {
		return
	}
	if direction == "up" {
		ex.up.Add(int64(n))
	} else {
		ex.down.Add(int64(n))
	}
	p.live.Add(int64(n), stats.BytesTotal, "action", ex.d.Action.String(), "direction", direction)
}

func sourceLabel(ex *exchange) string {
	if ex.unclassified {
		return "unknown"
	}
	return ex.source.String()
}
