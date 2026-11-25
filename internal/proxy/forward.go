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
	"syscall"

	"squid/internal/policy"
	"squid/internal/stats"
)

// errProxyAuth marks an origin's 407, which never reaches a client.
var errProxyAuth = errors.New("next hop answered 407")

// forward relays a plain-HTTP request through httputil.ReverseProxy: on a
// direct route with the direct transport of the client's class, on an
// upstream route with the upstream's transport (design 0002 D5, 0003 D4).
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, ex *exchange, rp *httputil.ReverseProxy) {
	r = r.WithContext(context.WithValue(r.Context(), exchangeKey{}, ex))
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &countingBody{ReadCloser: r.Body, p: p, ex: ex}
	}
	cw := &countingWriter{ResponseWriter: w, p: p, ex: ex}
	p.live.Add(1, stats.HTTPRequestsActive)
	defer p.live.Add(-1, stats.HTTPRequestsActive)
	defer func() {
		if v := recover(); v != nil {
			// ReverseProxy aborts with http.ErrAbortHandler when copying the
			// response fails after the head was sent. Log the status sent and
			// the bytes relayed, then let the server close the connection
			// (design 0002 Failure behavior).
			if ex.outcome == "" {
				ex.status = cw.status
				ex.outcome = OutcomeUpstreamProto
				if cw.writeErr != nil || r.Context().Err() != nil {
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
		p.finish(ex)
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
func modifyResponse(res *http.Response) error {
	if res.StatusCode == http.StatusProxyAuthRequired {
		return errProxyAuth
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

// countingBody counts request body bytes forwarded, after transfer
// decoding.
type countingBody struct {
	io.ReadCloser
	p  *Proxy
	ex *exchange
}

func (b *countingBody) Read(buf []byte) (int, error) {
	n, err := b.ReadCloser.Read(buf)
	b.p.addBytes(b.ex, "up", n)
	return n, err
}

// countingWriter counts response body bytes written to the client and
// records the status. After a 101 it counts the bytes relayed in each
// direction on the client side.
type countingWriter struct {
	http.ResponseWriter
	p         *Proxy
	ex        *exchange
	status    int
	generated bool
	writeErr  error // a write or flush to the client failed
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
// then copies through the returned connection, which counts.
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
	return &countingConn{Conn: conn, p: c.p, ex: c.ex}, brw, nil
}

type countingConn struct {
	net.Conn
	p  *Proxy
	ex *exchange
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.p.addBytes(c.ex, "up", n)
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.p.addBytes(c.ex, "down", n)
	return n, err
}
