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

	"squid/internal/stats"
)

// errProxyAuth marks an origin's 407, which never reaches a client.
var errProxyAuth = errors.New("next hop answered 407")

// forward relays a plain-HTTP request on a direct route through
// httputil.ReverseProxy (design 0002 D5).
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, ex *exchange) {
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
			// response fails. Log what was relayed, then let the server close
			// the connection.
			if ex.outcome == "" {
				ex.status = cw.status
				ex.outcome = OutcomeUpstreamProto
				if r.Context().Err() != nil {
					ex.outcome = OutcomeClientClosed
				}
				p.finish(ex)
			}
			panic(v)
		}
	}()
	ex.snap.rp.ServeHTTP(cw, r)
	if ex.outcome == "" {
		ex.status = cw.status
		ex.outcome = OutcomeOK
		p.finish(ex)
	}
}

// rewriteDirect prepares the outgoing request on a direct route. It keeps
// the client's Host and raw target, and removes every disclosure field.
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
}

// modifyResponse turns a 407 into cuttle's own 502: a 407 must never reach a
// client (design 0002 D5).
func modifyResponse(res *http.Response) error {
	if res.StatusCode == http.StatusProxyAuthRequired {
		return errProxyAuth
	}
	return nil
}

// forwardError answers failures of the direct transport (design 0002 D6).
func (p *Proxy) forwardError(w http.ResponseWriter, r *http.Request, err error) {
	ex := exchangeFrom(r.Context())
	if ex == nil || ex.outcome != "" {
		return
	}
	switch {
	case errors.Is(err, errProxyAuth):
		member := ex.snap.cfg.Name + "; received-status=407"
		p.writeGenerated(w, http.StatusBadGateway, member, "next hop answered 407 ("+ex.target+")")
		ex.status, ex.outcome = http.StatusBadGateway, OutcomeUpstreamStatus
		ex.detail = "received-status=407"
		p.finish(ex)
	case isDialError(err):
		p.dialFailure(w, ex, err)
	case errors.Is(err, context.Canceled):
		ex.outcome = OutcomeClientClosed
		p.finish(ex)
	case isTimeout(err):
		p.reply(w, ex, http.StatusGatewayTimeout, "http_response_timeout", "no response head within "+ex.snap.cfg.Proxy.ResponseTimeout.String(),
			"no response from "+ex.target, OutcomeTimeout)
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, io.EOF):
		p.reply(w, ex, http.StatusBadGateway, "connection_refused", "connection closed before the response",
			"connection closed by "+ex.target, OutcomeRefused)
	default:
		p.reply(w, ex, http.StatusBadGateway, "http_protocol_error", "unparsable response", "unparsable response from "+ex.target, OutcomeUpstreamProto)
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
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	if !c.generated {
		c.p.addBytes(c.ex, "down", n)
	}
	return n, err
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
