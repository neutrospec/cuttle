package testenv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// InspectingOptions configure an inspecting egress mock.
type InspectingOptions struct {
	// Root is the inspection root CA that signs the leaves the mock
	// presents to clients.
	Root *CA
	// Origins holds the roots the mock trusts toward origins.
	Origins *x509.CertPool
	// Net resolves and routes origin names.
	Net *Net
	// Refuse lists names whose CONNECT gets 403.
	Refuse []string
}

// Decrypted is one request the mock read inside a tunnel after
// terminating TLS.
type Decrypted struct {
	Conn   int
	Method string
	URI    string
	Host   string
	Header http.Header
}

// InspectingProxy is the inspecting egress mock (design 0007 D2): a
// plain-HTTP proxy that answers CONNECT with 200, terminates TLS with a leaf
// for the SNI name signed by the inspection root CA, and forwards the
// decrypted HTTP/1.1 requests to the origin, trusting the origin roots.
// Absolute-form http:// requests are proxied normally.
type InspectingProxy struct {
	// Addr is the real listener address on 127.0.0.1.
	Addr netip.AddrPort

	opt InspectingOptions
	ln  net.Listener
	tr  *http.Transport

	mu     sync.Mutex
	reqs   []ProxyRequest
	dec    []Decrypted
	leaves map[string]*tls.Certificate
	conns  map[int]net.Conn
	n      int
	wg     sync.WaitGroup
}

// NewInspectingProxy starts the mock on 127.0.0.1:0 and stops it when the
// test ends.
func NewInspectingProxy(t testing.TB, opt InspectingOptions) *InspectingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &InspectingProxy{
		Addr:   ln.Addr().(*net.TCPAddr).AddrPort(),
		opt:    opt,
		ln:     ln,
		leaves: map[string]*tls.Certificate{},
		conns:  map[int]net.Conn{},
	}
	p.tr = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			n, err := strconv.ParseUint(port, 10, 16)
			if err != nil {
				return nil, err
			}
			return dialNet(opt.Net, host, uint16(n))
		},
		TLSClientConfig:    &tls.Config{RootCAs: opt.Origins},
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		ForceAttemptHTTP2:  false,
		DisableCompression: true,
	}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

// Requests returns the raw request heads the mock received outside TLS:
// every CONNECT and absolute-form request.
func (p *InspectingProxy) Requests() []ProxyRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProxyRequest(nil), p.reqs...)
}

// Decrypted returns the requests read inside tunnels.
func (p *InspectingProxy) Decrypted() []Decrypted {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Decrypted(nil), p.dec...)
}

// Leaves returns the leaves the mock issued, by name.
func (p *InspectingProxy) Leaves() map[string]*x509.Certificate {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]*x509.Certificate{}
	for k, v := range p.leaves {
		out[k] = v.Leaf
	}
	return out
}

func (p *InspectingProxy) close() {
	p.ln.Close()
	p.mu.Lock()
	for _, c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	p.tr.CloseIdleConnections()
}

func (p *InspectingProxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.n++
		id := p.n
		p.conns[id] = c
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() {
				c.Close()
				p.mu.Lock()
				delete(p.conns, id)
				p.mu.Unlock()
			}()
			p.serve(c, id)
		}()
	}
}

func (p *InspectingProxy) serve(c net.Conn, id int) {
	br := bufio.NewReader(c)
	for {
		c.SetReadDeadline(time.Now().Add(time.Minute))
		req, err := readProxyRequest(br)
		if err != nil {
			return
		}
		c.SetReadDeadline(time.Time{})
		req.Conn = id
		p.mu.Lock()
		p.reqs = append(p.reqs, req)
		p.mu.Unlock()
		if req.Method == http.MethodConnect {
			p.inspect(c, br, req, id)
			return
		}
		if !p.proxyHTTP(c, req) {
			return
		}
	}
}

func (p *InspectingProxy) refused(host string) bool {
	for _, r := range p.opt.Refuse {
		if strings.EqualFold(r, host) {
			return true
		}
	}
	return false
}

// leaf returns the leaf for name, signed by the inspection root.
func (p *InspectingProxy) leaf(name string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.leaves[name]; l != nil {
		return l, nil
	}
	l, err := p.opt.Root.Issue(name)
	if err != nil {
		return nil, err
	}
	p.leaves[name] = &l
	return &l, nil
}

func (p *InspectingProxy) inspect(c net.Conn, br *bufio.Reader, req ProxyRequest, id int) {
	host, port, err := targetHostPort(req.Method, req.Target)
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "", "inspecting mock: bad target")
		return
	}
	if p.refused(host) {
		writeStatus(c, http.StatusForbidden, "", "inspecting mock: refused")
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	tc := tls.Server(&readerConn{Conn: c, r: br}, &tls.Config{
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := h.ServerName
			if name == "" {
				name = host
			}
			return p.leaf(name)
		},
	})
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(time.Minute))
	if err := tc.Handshake(); err != nil {
		return
	}
	tbr := bufio.NewReader(tc)
	authority := net.JoinHostPort(host, strconv.Itoa(int(port)))
	for {
		r, err := http.ReadRequest(tbr)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.dec = append(p.dec, Decrypted{Conn: id, Method: r.Method, URI: r.RequestURI, Host: r.Host, Header: r.Header.Clone()})
		p.mu.Unlock()
		r.URL.Scheme, r.URL.Host, r.RequestURI = "https", authority, ""
		res, err := p.tr.RoundTrip(r)
		if err != nil {
			writeStatus(tc, http.StatusBadGateway, "", "inspecting mock: "+err.Error())
			return
		}
		err = res.Write(tc)
		res.Body.Close()
		if err != nil || r.Close || res.Close {
			return
		}
	}
}

// proxyHTTP forwards an absolute-form request and writes the response
// back. It reports whether the connection can carry another request.
func (p *InspectingProxy) proxyHTTP(c net.Conn, req ProxyRequest) bool {
	r, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(append(bytes.Clone(req.Head), req.RawBody...))))
	if err != nil || r.URL.Scheme != "http" {
		writeStatus(c, http.StatusBadRequest, "", "inspecting mock: bad request")
		return false
	}
	r.RequestURI = ""
	res, err := p.tr.RoundTrip(r)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "", "inspecting mock: "+err.Error())
		return false
	}
	defer res.Body.Close()
	return res.Write(c) == nil && !r.Close && !res.Close
}

// readerConn reads through a buffered reader that may already hold bytes
// the client sent after its CONNECT head.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(b []byte) (int, error) { return c.r.Read(b) }
