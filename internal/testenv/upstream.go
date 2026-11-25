package testenv

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"squid/internal/netx"
)

// Field is one header field line as received: the name as sent and the
// value without surrounding whitespace.
type Field struct{ Name, Value string }

// ProxyRequest is one request as a mock proxy received it.
type ProxyRequest struct {
	Conn    int    // 1-based connection number
	Method  string // as sent
	Target  string // the request target exactly as sent
	Proto   string
	Fields  []Field // in received order
	Head    []byte  // raw head bytes, from the request line to the empty line
	Body    []byte  // body after transfer decoding
	RawBody []byte  // body bytes as received, chunk framing included
	Trailer []Field
}

// Values returns the values of every field named name, without regard to
// case, in received order.
func (r ProxyRequest) Values(name string) []string {
	var out []string
	for _, f := range r.Fields {
		if strings.EqualFold(f.Name, name) {
			out = append(out, f.Value)
		}
	}
	return out
}

// Get returns the first value of the field named name, or "".
func (r ProxyRequest) Get(name string) string {
	if v := r.Values(name); len(v) > 0 {
		return v[0]
	}
	return ""
}

type modeKind int

const (
	modePass modeKind = iota
	modeStatus
	modeBuffered
	modeSilent
	modeReset
)

// Mode is how the recording upstream answers requests for one target name
// (design 0007 D2).
type Mode struct {
	kind   modeKind
	status int
	header string
	early  []byte
}

// Pass tunnels a CONNECT after "200 Connection established" and forwards
// an absolute-form request, upgrades included, to the origin unchanged.
func Pass() Mode { return Mode{kind: modePass, status: http.StatusOK} }

// PassStatus is Pass with another 2xx status line and extra raw header
// lines, each ending in CRLF, such as "Content-Length: 3\r\n".
func PassStatus(code int, header string) Mode {
	return Mode{kind: modePass, status: code, header: header}
}

// Status answers with code, extra raw header lines, and a short body,
// without any outbound connection. A CONNECT connection is closed after
// the answer.
func Status(code int, header string) Mode {
	return Mode{kind: modeStatus, status: code, header: header}
}

// Buffered answers a CONNECT with 200 and the early bytes in one write, then
// echoes what the client sends.
func Buffered(early []byte) Mode { return Mode{kind: modeBuffered, early: early} }

// Silent reads the request and never answers.
func Silent() Mode { return Mode{kind: modeSilent} }

// Reset reads the request and resets the connection.
func Reset() Mode { return Mode{kind: modeReset} }

// UpstreamOptions configure a recording upstream.
type UpstreamOptions struct {
	// Net resolves and routes the targets the mock passes on.
	Net *Net
	// Allow407 lets Status(407) answer 407 with Proxy-Authenticate. Only
	// T1 tests whose clients are cuttle, Go, or /usr/bin/curl set it
	// (design 0007 D6); without it the mock answers 400 instead.
	Allow407 bool
}

// RecordingUpstream is a raw-socket HTTP proxy that records every request
// head and body byte for byte, per connection and in order (design 0007
// D2). Each target name gets its Mode; a name without one passes when it
// is cuttle.test or below it and gets 403 otherwise, as the research
// recorder did, so stray requests never leave the test network.
type RecordingUpstream struct {
	// Addr is the real listener address on 127.0.0.1.
	Addr netip.AddrPort

	opt UpstreamOptions
	ln  net.Listener

	mu    sync.Mutex
	modes map[string]Mode
	reqs  []ProxyRequest
	raw   map[int]*bytes.Buffer
	conns map[int]net.Conn
	// out holds the open connections to origins, which close also closes,
	// so a relay blocked on an origin cannot stall the test's cleanup.
	out map[net.Conn]bool
	n   int
	wg  sync.WaitGroup
}

// NewRecordingUpstream starts a recording upstream on 127.0.0.1:0 and stops
// it, with every connection it holds, when the test ends.
func NewRecordingUpstream(t testing.TB, opt UpstreamOptions) *RecordingUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &RecordingUpstream{
		Addr:  ln.Addr().(*net.TCPAddr).AddrPort(),
		opt:   opt,
		ln:    ln,
		modes: map[string]Mode{},
		raw:   map[int]*bytes.Buffer{},
		conns: map[int]net.Conn{},
		out:   map[net.Conn]bool{},
	}
	u.wg.Add(1)
	go u.accept()
	t.Cleanup(u.close)
	return u
}

// SetMode sets the mode for a target name, which is compared in lowercase
// without a port.
func (u *RecordingUpstream) SetMode(name string, m Mode) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.modes[strings.ToLower(name)] = m
}

// Requests returns the recorded requests in arrival order.
func (u *RecordingUpstream) Requests() []ProxyRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]ProxyRequest(nil), u.reqs...)
}

// Raw returns every byte read on connection id (1-based), tunnelled bytes
// included.
func (u *RecordingUpstream) Raw(id int) []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if b := u.raw[id]; b != nil {
		return bytes.Clone(b.Bytes())
	}
	return nil
}

// Connections returns how many connections the mock accepted.
func (u *RecordingUpstream) Connections() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// OpenConnections returns how many accepted connections the mock still
// serves; one counts until the peer closes it.
func (u *RecordingUpstream) OpenConnections() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.conns)
}

// Reset clears the recorded requests.
func (u *RecordingUpstream) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reqs = nil
}

func (u *RecordingUpstream) close() {
	u.ln.Close()
	u.mu.Lock()
	for _, c := range u.conns {
		c.Close()
	}
	for c := range u.out {
		c.Close()
	}
	u.mu.Unlock()
	u.wg.Wait()
}

// dialOrigin connects to an origin and tracks the connection until done.
func (u *RecordingUpstream) dialOrigin(host string, port uint16) (net.Conn, func(), error) {
	c, err := dialNet(u.opt.Net, host, port)
	if err != nil {
		return nil, nil, err
	}
	u.mu.Lock()
	u.out[c] = true
	u.mu.Unlock()
	return c, func() {
		c.Close()
		u.mu.Lock()
		delete(u.out, c)
		u.mu.Unlock()
	}, nil
}

func (u *RecordingUpstream) accept() {
	defer u.wg.Done()
	for {
		c, err := u.ln.Accept()
		if err != nil {
			return
		}
		u.mu.Lock()
		u.n++
		id := u.n
		buf := new(bytes.Buffer)
		u.raw[id] = buf
		rc := &tapConn{Conn: c, mu: &u.mu, buf: buf}
		u.conns[id] = rc
		u.mu.Unlock()
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			defer func() {
				rc.Close()
				u.mu.Lock()
				delete(u.conns, id)
				u.mu.Unlock()
			}()
			u.serve(rc, id)
		}()
	}
}

func (u *RecordingUpstream) mode(host string) Mode {
	u.mu.Lock()
	defer u.mu.Unlock()
	if m, ok := u.modes[host]; ok {
		return m
	}
	if host == "cuttle.test" || strings.HasSuffix(host, ".cuttle.test") {
		return Pass()
	}
	return Status(http.StatusForbidden, "")
}

func (u *RecordingUpstream) serve(c net.Conn, id int) {
	br := bufio.NewReader(c)
	for {
		c.SetReadDeadline(time.Now().Add(time.Minute))
		req, err := readProxyRequest(br)
		if err != nil {
			return
		}
		req.Conn = id
		u.mu.Lock()
		u.reqs = append(u.reqs, req)
		u.mu.Unlock()
		c.SetReadDeadline(time.Time{})
		host, port, err := targetHostPort(req.Method, req.Target)
		m := Status(http.StatusBadRequest, "")
		if err == nil {
			m = u.mode(host)
		}
		if m.kind == modeStatus && m.status == http.StatusProxyAuthRequired && !u.opt.Allow407 {
			// No 407 without the test's consent (design 0007 D6).
			writeStatus(c, http.StatusBadRequest, "", "testenv: 407 refused; only Go-only and curl T1 tests may enable it")
			return
		}
		switch m.kind {
		case modeSilent:
			io.Copy(io.Discard, br)
			return
		case modeReset:
			resetConn(c)
			return
		case modeStatus:
			body := fmt.Sprintf("recording upstream: %d", m.status)
			header := m.header
			if m.status == http.StatusProxyAuthRequired {
				header += "Proxy-Authenticate: Basic realm=\"mock\"\r\n"
			}
			if err := writeStatus(c, m.status, header, body); err != nil || req.Method == http.MethodConnect {
				return
			}
			continue
		}
		if req.Method == http.MethodConnect {
			u.tunnel(c, br, m, host, port)
			return
		}
		if m.kind != modePass || !u.forward(c, br, req, host, port) {
			return
		}
	}
}

// tunnel answers a CONNECT in Pass or Buffered mode.
func (u *RecordingUpstream) tunnel(c net.Conn, br *bufio.Reader, m Mode, host string, port uint16) {
	if m.kind == modeBuffered {
		if _, err := c.Write(append([]byte("HTTP/1.1 200 Connection established\r\n\r\n"), m.early...)); err != nil {
			return
		}
		io.Copy(c, br) // echo, pipelined bytes first
		return
	}
	origin, done, err := u.dialOrigin(host, port)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "", "recording upstream: "+err.Error())
		return
	}
	defer done()
	if _, err := io.WriteString(c, fmt.Sprintf("HTTP/1.1 %d %s\r\n%s\r\n", m.status, reason(m.status), m.header)); err != nil {
		return
	}
	splice(c, br, origin, nil)
}

// forward passes an absolute-form request to the origin unchanged and
// relays the origin's response heads and body unchanged: informational
// heads first, then the final response. After a 101 it splices the
// connections. It reports whether the client connection can
// carry another request.
func (u *RecordingUpstream) forward(c net.Conn, br *bufio.Reader, req ProxyRequest, host string, port uint16) bool {
	origin, done, err := u.dialOrigin(host, port)
	if err != nil {
		writeStatus(c, http.StatusBadGateway, "", "recording upstream: "+err.Error())
		return false
	}
	defer done()
	if _, err := origin.Write(append(bytes.Clone(req.Head), req.RawBody...)); err != nil {
		return false
	}
	obr := bufio.NewReader(origin)
	var status int
	var fields []Field
	for {
		head, st, fs, err := readHead(obr)
		if err != nil {
			writeStatus(c, http.StatusBadGateway, "", "recording upstream: no response from origin")
			return false
		}
		if _, err := c.Write(head); err != nil {
			return false
		}
		status, fields = st, fs
		// Informational heads, such as 100 Continue and 103 Early Hints,
		// come before the final response; a 101 ends HTTP on the
		// connection.
		if status/100 != 1 || status == http.StatusSwitchingProtocols {
			break
		}
	}
	if status == http.StatusSwitchingProtocols {
		splice(c, br, origin, obr)
		return false
	}
	resp := ProxyRequest{Fields: fields}
	if req.Method == http.MethodHead || status == http.StatusNoContent || status == http.StatusNotModified {
		return !closes(req) && !closes(resp)
	}
	switch {
	case hasChunked(resp):
		raw, _, _, err := readChunked(obr)
		c.Write(raw)
		if err != nil {
			return false
		}
	case resp.Get("Content-Length") != "":
		n, err := strconv.ParseInt(resp.Get("Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return false
		}
		if _, err := io.CopyN(c, obr, n); err != nil {
			return false
		}
	default:
		io.Copy(c, obr) // delimited by close
		return false
	}
	return !closes(req) && !closes(resp)
}

func closes(r ProxyRequest) bool {
	for _, v := range r.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "close") {
				return true
			}
		}
	}
	return false
}

func hasChunked(r ProxyRequest) bool {
	te := r.Values("Transfer-Encoding")
	if len(te) == 0 {
		return false
	}
	codings := strings.Split(te[len(te)-1], ",")
	return strings.EqualFold(strings.TrimSpace(codings[len(codings)-1]), "chunked")
}

// splice relays bytes both ways between a client and an outbound
// connection, starting with bytes already buffered on either side, and
// half-closes each direction at EOF.
func splice(c net.Conn, cbr *bufio.Reader, o net.Conn, obr *bufio.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(o, cbr)
		closeWrite(o)
	}()
	go func() {
		defer wg.Done()
		var src io.Reader = o
		if obr != nil {
			src = obr
		}
		io.Copy(c, src)
		closeWrite(c)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

func resetConn(c net.Conn) {
	if tc, ok := c.(*tapConn); ok {
		c = tc.Conn
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetLinger(0)
	}
	c.Close()
}

// writeStatus writes a complete plain-text response.
func writeStatus(w io.Writer, code int, header, body string) error {
	_, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nX-Mock: recording-upstream\r\n%s\r\n%s",
		code, reason(code), len(body), header, body)
	return err
}

func reason(code int) string {
	if code == http.StatusOK {
		return "Connection established"
	}
	if t := http.StatusText(code); t != "" {
		return t
	}
	return "Status"
}

// dialNet resolves host through the test network and dials it.
func dialNet(n *Net, host string, port uint16) (net.Conn, error) {
	if n == nil {
		return nil, errors.New("no test network")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := netip.ParseAddr(host)
	if err != nil {
		addrs, err := n.LookupNetIP(ctx, host)
		if err != nil {
			return nil, err
		}
		a = addrs[0]
	}
	return n.DialContext(ctx, netip.AddrPortFrom(a, port), netx.Binding{})
}

// targetHostPort returns the lowercase host and the port of a CONNECT
// authority or an absolute-form http:// target.
func targetHostPort(method, target string) (string, uint16, error) {
	auth, def := target, ""
	if method != http.MethodConnect {
		rest, ok := strings.CutPrefix(target, "http://")
		if !ok {
			return "", 0, errors.New("not an absolute-form http:// target")
		}
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			rest = rest[:i]
		}
		auth, def = rest, "80"
	}
	host, port, err := net.SplitHostPort(auth)
	if err != nil {
		if def == "" {
			return "", 0, err
		}
		host, port = strings.Trim(auth, "[]"), def
	}
	if port == "" {
		port = def
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSuffix(strings.ToLower(host), "."), uint16(p), nil
}

// maxHead bounds a head read by the mocks.
const maxHead = 1 << 20

// readHead reads one message head exactly as received and parses its first
// line's status code, when it is a response, and its fields.
func readHead(br *bufio.Reader) (head []byte, status int, fields []Field, err error) {
	var lines []string
	for {
		line, err := br.ReadString('\n')
		head = append(head, line...)
		if err != nil {
			return nil, 0, nil, err
		}
		if len(head) > maxHead {
			return nil, 0, nil, errors.New("head too large")
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, 0, nil, errors.New("empty head")
	}
	if strings.HasPrefix(lines[0], "HTTP/") {
		if f := strings.Fields(lines[0]); len(f) >= 2 {
			status, _ = strconv.Atoi(f[1])
		}
	}
	for _, l := range lines[1:] {
		name, value, _ := strings.Cut(l, ":")
		fields = append(fields, Field{Name: name, Value: strings.Trim(value, " \t")})
	}
	return head, status, fields, nil
}

// readProxyRequest reads one request head and its body exactly as
// received.
func readProxyRequest(br *bufio.Reader) (ProxyRequest, error) {
	head, _, fields, err := readHead(br)
	if err != nil {
		return ProxyRequest{}, err
	}
	first, _, _ := strings.Cut(string(head), "\r\n")
	parts := strings.SplitN(strings.TrimRight(first, "\n"), " ", 3)
	if len(parts) != 3 {
		return ProxyRequest{}, fmt.Errorf("malformed request line %q", first)
	}
	r := ProxyRequest{Method: parts[0], Target: parts[1], Proto: parts[2], Fields: fields, Head: head}
	if r.Method == http.MethodConnect {
		return r, nil
	}
	switch {
	case hasChunked(r):
		r.RawBody, r.Body, r.Trailer, err = readChunked(br)
	case r.Get("Content-Length") != "":
		n, perr := strconv.ParseInt(r.Get("Content-Length"), 10, 64)
		if perr != nil || n < 0 || n > maxHead*64 {
			return r, fmt.Errorf("bad Content-Length %q", r.Get("Content-Length"))
		}
		r.RawBody = make([]byte, n)
		_, err = io.ReadFull(br, r.RawBody)
		r.Body = r.RawBody
	}
	return r, err
}

// readChunked reads a chunked body: the raw bytes with their framing, the
// decoded body, and the trailer fields.
func readChunked(br *bufio.Reader) (raw, body []byte, trailer []Field, err error) {
	for {
		line, err := br.ReadString('\n')
		raw = append(raw, line...)
		if err != nil {
			return raw, body, nil, err
		}
		size, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		n, err := strconv.ParseInt(strings.TrimSpace(size), 16, 64)
		if err != nil || n < 0 || n > maxHead*64 {
			return raw, body, nil, fmt.Errorf("bad chunk size %q", line)
		}
		if n == 0 {
			for {
				tl, err := br.ReadString('\n')
				raw = append(raw, tl...)
				if err != nil {
					return raw, body, trailer, err
				}
				tl = strings.TrimRight(tl, "\r\n")
				if tl == "" {
					return raw, body, trailer, nil
				}
				name, value, _ := strings.Cut(tl, ":")
				trailer = append(trailer, Field{Name: name, Value: strings.Trim(value, " \t")})
			}
		}
		chunk := make([]byte, n+2)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return raw, body, nil, err
		}
		raw = append(raw, chunk...)
		body = append(body, chunk[:n]...)
	}
}

// tapConn records the bytes read from a connection.
type tapConn struct {
	net.Conn
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (c *tapConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.buf.Write(p[:n])
		c.mu.Unlock()
	}
	return n, err
}

func (c *tapConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
