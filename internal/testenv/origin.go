package testenv

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// OriginOptions configure a mock origin.
type OriginOptions struct {
	// TLS, when set, serves HTTPS with a leaf from this CA.
	TLS *CA
	// Names are the leaf's SANs. The default is *.cuttle.test, cuttle.test,
	// localhost, and 127.0.0.1.
	Names []string
	// Allow407 lets /status?code=407 answer 407. Only Go-only and
	// /usr/bin/curl T1 tests set it (design 0007 D6); without it the origin
	// refuses that status.
	Allow407 bool
}

// Request is one request as the origin received it.
type Request struct {
	Conn    int         `json:"conn"`
	Method  string      `json:"method"`
	URI     string      `json:"uri"` // the request target exactly as sent
	Proto   string      `json:"proto"`
	Host    string      `json:"host"`
	Header  http.Header `json:"header"`
	Body    []byte      `json:"body"`
	Trailer http.Header `json:"trailer,omitempty"`
}

// Origin is a mock HTTP or HTTPS origin (design 0007 D2). Endpoints:
//
//	/bytes?n=N    N bytes of a repeating pattern
//	/sink         counts the uploaded body
//	/slow?ms=N    answers after N ms
//	/chunked      a chunked body with a trailer
//	/ws           WebSocket echo
//	/ws.html      a page that opens ws://<host>/ws and sends one message
//	/status?code= answers with that status; 407 only with Allow407
//	/hints        a 103 Early Hints response, then 200
//	anything else echoes the request as JSON (/echo included)
//
// Every request is recorded, and the raw bytes read on each connection are
// kept.
type Origin struct {
	// Addr is the real listener address on 127.0.0.1.
	Addr netip.AddrPort

	opt OriginOptions
	srv *http.Server

	mu    sync.Mutex
	reqs  []Request
	raw   map[int]*bytes.Buffer
	conns int
	open  int // connections not yet closed or hijacked
}

type connIDKey struct{}

// NewOrigin starts an origin on 127.0.0.1:0 and stops it when the test ends.
func NewOrigin(t testing.TB, opt OriginOptions) *Origin {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o := &Origin{opt: opt, raw: map[int]*bytes.Buffer{}}
	o.Addr = ln.Addr().(*net.TCPAddr).AddrPort()
	if opt.TLS != nil {
		names := opt.Names
		if len(names) == 0 {
			names = []string{"*.cuttle.test", "cuttle.test", "localhost", "127.0.0.1"}
		}
		leaf := opt.TLS.Leaf(t, names...)
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{leaf}, NextProtos: []string{"http/1.1"}})
	}
	o.srv = &http.Server{
		Handler:           o,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connIDKey{}, c.(*recordingConn).id)
		},
		ConnState: func(_ net.Conn, st http.ConnState) {
			o.mu.Lock()
			defer o.mu.Unlock()
			switch st {
			case http.StateNew:
				o.open++
			case http.StateClosed, http.StateHijacked:
				o.open--
			}
		},
		ErrorLog: newDiscardLogger(),
	}
	go o.srv.Serve(&recordingListener{Listener: ln, o: o})
	t.Cleanup(func() { o.srv.Close() })
	return o
}

// Requests returns the recorded requests in arrival order.
func (o *Origin) Requests() []Request {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Request(nil), o.reqs...)
}

// Connections returns how many connections the origin accepted.
func (o *Origin) Connections() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.conns
}

// OpenConnections returns how many connections the origin holds open, not
// counting hijacked ones.
func (o *Origin) OpenConnections() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.open
}

// Raw returns the bytes read on connection id (1-based), after TLS
// decryption for an HTTPS origin.
func (o *Origin) Raw(id int) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if b := o.raw[id]; b != nil {
		return bytes.Clone(b.Bytes())
	}
	return nil
}

// Reset clears the recorded requests.
func (o *Origin) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reqs = nil
}

func (o *Origin) record(r Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reqs = append(o.reqs, r)
}

func (o *Origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, _ := r.Context().Value(connIDKey{}).(int)
	if r.URL.Path == "/ws" {
		o.record(Request{Conn: id, Method: r.Method, URI: r.RequestURI, Proto: r.Proto, Host: r.Host, Header: r.Header.Clone()})
		websocket.Handler(func(ws *websocket.Conn) {
			var msg []byte
			for {
				if err := websocket.Message.Receive(ws, &msg); err != nil {
					return
				}
				o.record(Request{Conn: id, Method: "WS", URI: r.RequestURI, Body: bytes.Clone(msg)})
				if err := websocket.Message.Send(ws, msg); err != nil {
					return
				}
			}
		}).ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	rec := Request{Conn: id, Method: r.Method, URI: r.RequestURI, Proto: r.Proto, Host: r.Host,
		Header: r.Header.Clone(), Body: body, Trailer: r.Trailer.Clone()}
	o.record(rec)
	q := r.URL.Query()
	switch r.URL.Path {
	case "/bytes":
		n, _ := strconv.Atoi(q.Get("n"))
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.Write(Pattern(n))
	case "/sink":
		fmt.Fprintf(w, "%d", len(body))
	case "/slow":
		ms, _ := strconv.Atoi(q.Get("ms"))
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "slow")
	case "/chunked":
		w.Header().Set("Trailer", "X-Checksum")
		f := http.NewResponseController(w)
		for i := range 3 {
			fmt.Fprintf(w, "chunk-%d;", i)
			f.Flush()
		}
		w.Header().Set("X-Checksum", "done")
	case "/hints":
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		io.WriteString(w, "hints")
	case "/ws.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, wsPage)
	case "/status":
		code, err := strconv.Atoi(q.Get("code"))
		if err != nil || code < 200 || code > 599 {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		if code == http.StatusProxyAuthRequired {
			if !o.opt.Allow407 {
				http.Error(w, "testenv: 407 refused; only Go-only and curl T1 tests may enable it", http.StatusBadRequest)
				return
			}
			w.Header().Set("Proxy-Authenticate", `Basic realm="mock"`)
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "status %d", code)
	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rec)
	}
}

// Pattern returns n bytes of a repeating pattern.
func Pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

const wsPage = `<!doctype html><html><body><pre id="out">waiting</pre><script>
const ws = new WebSocket("ws://" + location.host + "/ws");
ws.onopen = () => ws.send("hello from ws.html");
ws.onmessage = (e) => { document.getElementById("out").textContent = "echo: " + e.data; };
ws.onerror = () => { document.getElementById("out").textContent = "error"; };
</script></body></html>
`

type recordingListener struct {
	net.Listener
	o *Origin
}

func (l *recordingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.o.mu.Lock()
	l.o.conns++
	id := l.o.conns
	buf := new(bytes.Buffer)
	l.o.raw[id] = buf
	l.o.mu.Unlock()
	return &recordingConn{Conn: c, id: id, o: l.o, buf: buf}, nil
}

type recordingConn struct {
	net.Conn
	id  int
	o   *Origin
	buf *bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.o.mu.Lock()
		c.buf.Write(p[:n])
		c.o.mu.Unlock()
	}
	return n, err
}

// CloseWrite half-closes plain TCP connections.
func (c *recordingConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
