package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/neutrospec/cuttle/internal/policy"
	"github.com/neutrospec/cuttle/internal/stats"
)

// errProxyAuth marks an origin's 407, which never reaches a client.
var errProxyAuth = errors.New("next hop answered 407")

// forward relays a plain-HTTP request through httputil.ReverseProxy: on a
// direct route with the direct transport of the client's class, on an
// upstream route with the upstream's transport (design 0002 D5, 0003 D4).
// The transports' outbound connections count up as their socket writes
// accept body bytes (outConn); countingWriter counts down.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, ex *exchange, rp *httputil.ReverseProxy) {
	r = r.WithContext(withOutConnTrace(context.WithValue(r.Context(), exchangeKey{}, ex), ex))
	cw := &countingWriter{ResponseWriter: w, p: p, ex: ex}
	p.rec.Open(ex)
	p.live.Add(1, stats.HTTPRequestsActive)
	defer p.live.Add(-1, stats.HTTPRequestsActive)
	defer cw.endUpgrade()
	defer func() {
		if v := recover(); v != nil {
			// ReverseProxy aborts with http.ErrAbortHandler when copying the
			// response fails after the head was sent. Log the status sent and
			// the bytes relayed, then let the server close the connection
			// (design 0002 Failure behavior).
			if ex.outcome == "" {
				ex.status = cw.status
				ex.outcome = OutcomeUpstreamProto
				switch {
				case p.forced.Load():
					ex.outcome = OutcomeShutdown
				case cw.writeErr != nil || r.Context().Err() != nil:
					ex.outcome = OutcomeClientClosed
				}
				p.finish(ex)
			}
			panic(v)
		}
	}()
	rp.ServeHTTP(cw, r)
	if ex.outcome == "" {
		ex.status = cw.status
		ex.outcome = OutcomeOK
		if u := cw.upgraded; u != nil {
			if u.wasShutdown.Load() {
				ex.outcome = OutcomeShutdown
			}
			settleWrites(ex)
		}
		p.finish(ex)
	}
}

// maxSettle bounds how long an upgraded connection's decision waits for
// socket writes still in progress. ReverseProxy has closed the client
// connection, so they end at once.
const maxSettle = time.Second

// settleWrites waits until the exchange has no socket write in progress,
// so its decision line has the bytes of every write that completed.
func settleWrites(ex *exchange) {
	deadline := time.Now().Add(maxSettle)
	for ex.writes.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// rewriteDirect prepares the outgoing request on a direct route. It keeps
// the client's Host and raw target, and removes every disclosure field from
// the header and the trailer.
// Without the raw target, ReverseProxy and the transport would re-escape the
// path and drop or rewrite the query (design 0002 D5).
func rewriteDirect(pr *httputil.ProxyRequest) {
	ex := exchangeFrom(pr.In.Context())
	t := ex.http
	u := pr.Out.URL
	u.Scheme = "http"
	u.Host = t.Authority.String() // dial and pool key; the dial uses the decision
	u.Path, u.RawPath = "", ""
	if strings.HasPrefix(t.RawPath, "//") {
		// RequestURI() would read //x as an authority; send absolute-form,
		// which origins must accept (RFC 9112 section 3.2.2).
		u.Opaque = "//" + t.RawAuthority + t.RawPath
	} else {
		u.Opaque = t.RawPath // empty means "/"
	}
	u.RawQuery = t.RawQuery
	u.ForceQuery = t.HasQuery && t.RawQuery == ""
	pr.Out.Host = pr.In.Host
	removeDisclosure(pr.Out.Header)
	removeDisclosure(pr.Out.Trailer)
}

// modifyResponse turns a 407 into cuttle's own 502: a 407 must never reach a
// client (design 0002 D5, 0003 D4). Every other response passes unchanged.
// After a 101 the outbound connection carries the client's upgraded
// stream, so every byte written to it counts as up (design 0002 D7).
func modifyResponse(res *http.Response) error {
	switch res.StatusCode {
	case http.StatusProxyAuthRequired:
		return errProxyAuth
	case http.StatusSwitchingProtocols:
		if ex := exchangeFrom(res.Request.Context()); ex != nil {
			if oc := ex.out.Load(); oc != nil {
				oc.upgrade()
			}
		}
	}
	return nil
}

// forwardError answers failures of the direct and upstream transports
// (design 0002 D6, 0003 D6). On upstream routes cuttle's member names the
// upstream as next-hop.
func (p *Proxy) forwardError(w http.ResponseWriter, r *http.Request, err error) {
	ex := exchangeFrom(r.Context())
	if ex == nil || ex.outcome != "" {
		return
	}
	nextHop, peer := "", ex.target
	timeout := ex.snap.cfg.Proxy.ResponseTimeout
	upstream := ex.d.Action == policy.ActionUpstream
	if upstream {
		nextHop, peer = ex.d.Upstream, "upstream "+ex.d.Upstream
		timeout = ex.snap.upstreams[ex.d.Upstream].cfg.ResponseTimeout
	}
	var held *heldError
	switch {
	case errors.Is(err, errProxyAuth):
		p.writeGenerated(w, http.StatusBadGateway, receivedMember(ex.snap.cfg.Name, http.StatusProxyAuthRequired, nextHop),
			"next hop answered 407 ("+ex.target+")")
		ex.status, ex.outcome, ex.upstreamStatus = http.StatusBadGateway, OutcomeUpstreamStatus, http.StatusProxyAuthRequired
		p.finish(ex)
	case upstream && (isDialError(err) || errors.As(err, &held)):
		p.upstreamDialFailure(w, ex, err)
	case isDialError(err):
		p.dialFailure(w, ex, err)
	case errors.Is(err, context.Canceled):
		ex.outcome = OutcomeClientClosed
		if p.forced.Load() {
			ex.outcome = OutcomeShutdown
		}
		p.finish(ex)
	case isTimeout(err):
		p.replyHop(w, ex, http.StatusGatewayTimeout, "http_response_timeout", nextHop, "no response head within "+timeout.String(),
			"no response from "+peer, OutcomeTimeout)
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, io.EOF):
		p.replyHop(w, ex, http.StatusBadGateway, "connection_refused", nextHop, "connection closed before the response",
			"connection closed by "+peer, OutcomeRefused)
	default:
		p.replyHop(w, ex, http.StatusBadGateway, "http_protocol_error", nextHop, "unparsable response",
			"unparsable response from "+peer, OutcomeUpstreamProto)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// countingWriter counts response body bytes written to the client and
// records the status. After a 101 it counts the bytes written to the
// client, and tracks the upgraded connection like a tunnel.
type countingWriter struct {
	http.ResponseWriter
	p         *Proxy
	ex        *exchange
	status    int
	generated bool
	writeErr  error     // a write or flush to the client failed
	upgraded  *upgraded // set after a 101
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		c.status = code
	}
	if !c.generated {
		c.ex.relayed = true
	}
	c.ResponseWriter.WriteHeader(code)
}

// Write passes body bytes to the client and flushes them through the
// server's buffers, so down counts only bytes the client socket accepted,
// never bytes still buffered (design 0002 D7). A failed write or flush
// counts nothing for that call and marks the client side as failed.
func (c *countingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.generated {
		return c.ResponseWriter.Write(b)
	}
	n, err := c.ResponseWriter.Write(b)
	if err == nil {
		err = http.NewResponseController(c.ResponseWriter).Flush()
	}
	if err != nil {
		c.writeErr = err
		return n, err
	}
	c.p.addBytes(c.ex, "down", n)
	return n, nil
}

// Unwrap lets http.ResponseController reach Flush on the server's writer.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Hijack is used by ReverseProxy after a 101. It writes the 101 head through
// the returned bufio.Writer, which goes to the connection uncounted, and
// then copies through the returned connection, which counts down. Up is
// counted on the outbound connection. The upgraded connection is tracked
// like a tunnel: Shutdown closes it with outcome shutdown, and
// proxy.idle_timeout closes it when no byte moves in either direction
// (design 0002 D5, 0006 D6).
func (c *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(c.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// ReverseProxy hijacks only to relay a 101, whose head it writes itself.
	if c.status == 0 {
		c.status = http.StatusSwitchingProtocols
	}
	c.ex.relayed = true
	u := &upgraded{conn: conn, ex: c.ex, stopIdle: func() {}}
	u.last.Store(time.Now().UnixNano())
	c.upgraded = u
	if c.p.track(u) {
		u.stopIdle = watchIdle(&u.last, c.ex.snap.cfg.Proxy.IdleTimeout, u.close)
	} else {
		u.shutdown() // shutdown has started; the copy fails at once
	}
	return &countingConn{Conn: conn, p: c.p, ex: c.ex, last: &u.last}, brw, nil
}

// endUpgrade stops tracking the upgraded connection once ReverseProxy has
// finished with it.
func (c *countingWriter) endUpgrade() {
	if u := c.upgraded; u != nil {
		u.stopIdle()
		c.p.untrack(u)
	}
}

// upgraded is a client connection upgraded after a 101.
type upgraded struct {
	conn        net.Conn
	ex          *exchange
	last        atomic.Int64 // unix nanoseconds of the last read or write
	stopIdle    func()
	wasShutdown atomic.Bool
}

// close closes the client connection; ReverseProxy then ends the copy and
// closes the connection to the next hop.
func (u *upgraded) close() { u.conn.Close() }

func (u *upgraded) shutdown() {
	u.wasShutdown.Store(true)
	u.close()
}

func (u *upgraded) status() TunnelStatus { return trackedStatus(u.ex, "upgrade") }

// countingConn is the client side of an upgraded connection. It counts the
// bytes written to the client and records activity for the idle limit.
type countingConn struct {
	net.Conn
	p    *Proxy
	ex   *exchange
	last *atomic.Int64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.ex.writes.Add(1)
	defer c.ex.writes.Add(-1)
	n, err := c.Conn.Write(b)
	c.p.addBytes(c.ex, "down", n)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}
