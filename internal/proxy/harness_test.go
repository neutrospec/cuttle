package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"squid/internal/config"
	"squid/internal/logx"
	"squid/internal/stats"
	"squid/internal/testenv"
)

// baseConfig is the starter's routing with the test name, a block rule for
// blocked.cuttle.test, and no admin listener. Tests pass their own
// listeners, so proxy.listen is never bound.
const baseConfig = `
name = "cuttle-test"

[proxy]
default = "direct"

[admin]
listen = ""

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "blocked"
domains = ["blocked.cuttle.test"]
action = "block"
`

type envOptions struct {
	logWriter io.Writer
	allow407  bool
	// remote lists peer addresses, such as "10.8.0.1". Each gets its own
	// loopback listener wrapped by testenv.SourceListener, so the instance
	// sees its clients at that address (design 0007 D4).
	remote []string
	// local, when set, is the address the local listener reports for its
	// clients, such as a distinctive loopback address.
	local string
	// listener, when set, is the local listener, bound by the test.
	listener net.Listener
	// clock is the unreachable hold's clock; nil means time.Now.
	clock func() time.Time
	// interfaces, timers, and selfChanged are the interface source, the
	// timer of its monitor, and the hook called after each re-read (design
	// 0008 Interfaces); nil leaves them out.
	interfaces  *testenv.Interfaces
	timers      *testenv.Timers
	selfChanged func()
	// stats is the statistics recorder; nil records nothing (design 0005
	// D4).
	stats *stats.Recorder
	// live, when set, is the instance's live counter set.
	live *stats.Live
}

// env is one in-process cuttle instance with mock origins behind the test
// network (design 0007 D3, D4).
type env struct {
	t     *testing.T
	net   *testenv.Net
	ca    *testenv.CA
	http  *testenv.Origin
	https *testenv.Origin
	p     *Proxy
	addr  string
	logs  *logBuffer
	// remote maps each peer address of envOptions.remote to the real
	// address of its listener.
	remote map[string]string
}

func newEnv(t *testing.T, cfgText string) *env {
	return newEnvWith(t, cfgText, envOptions{})
}

func newEnvWith(t *testing.T, cfgText string, opt envOptions) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	ca := testenv.NewCA(t, "cuttle test origin root")
	e := &env{
		t:     t,
		ca:    ca,
		http:  testenv.NewOrigin(t, testenv.OriginOptions{Allow407: opt.allow407}),
		https: testenv.NewOrigin(t, testenv.OriginOptions{TLS: ca}),
		logs:  &logBuffer{},
	}
	e.net = e.originNet()
	e.start(cfgText, opt)
	return e
}

// originNames resolve to the mock origins in every test network of an env.
var originNames = []string{"origin.cuttle.test", "plain.cuttle.test", "ws.cuttle.test", "h"}

// originNet returns a new test network in which the origin names resolve
// to the env's mock origins, for an instance or a mock proxy.
func (e *env) originNet() *testenv.Net {
	n := testenv.NewNet()
	for _, name := range originNames {
		n.Map(name, "203.0.113.10")
	}
	n.Map("localhost", "127.0.0.1", "::1")
	n.Map("blocked.cuttle.test", "203.0.113.99")
	n.Map("x.blocked.cuttle.test", "203.0.113.99")
	n.Route("203.0.113.10:80", e.http.Addr)
	n.Route("203.0.113.10:443", e.https.Addr)
	n.Route("203.0.113.99:80", e.http.Addr)
	n.Route("203.0.113.99:443", e.https.Addr)
	return n
}

// peer starts another instance with its own test network, sharing the
// env's CA and origins, such as the gateway of a chain (design 0007 D4).
func (e *env) peer(cfgText string, opt envOptions) *env {
	e.t.Helper()
	f := &env{t: e.t, ca: e.ca, http: e.http, https: e.https, logs: &logBuffer{}}
	f.net = f.originNet()
	f.start(cfgText, opt)
	return f
}

// listenerAddr returns the real address of the listener that reports
// peer, for routing a test address to it.
func (e *env) listenerAddr(peer string) netip.AddrPort {
	e.t.Helper()
	return netip.MustParseAddrPort(e.proxyAddr(peer))
}

// start runs the instance for cfgText on the env's test network.
func (e *env) start(cfgText string, opt envOptions) {
	t := e.t
	t.Helper()
	n := e.net
	cfg, r := config.Parse([]byte(cfgText), "test.toml")
	if cfg == nil {
		t.Fatalf("config: %v", r.Err())
	}
	ln := opt.listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	addr := ln.Addr().String()
	if opt.local != "" {
		ln = testenv.SourceListener(ln, netip.MustParseAddr(opt.local))
	}
	listeners := []net.Listener{ln}
	e.remote = map[string]string{}
	for _, peer := range opt.remote {
		rl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, testenv.SourceListener(rl, netip.MustParseAddr(peer)))
		e.remote[peer] = rl.Addr().String()
	}
	var w io.Writer = e.logs
	if opt.logWriter != nil {
		w = io.MultiWriter(e.logs, opt.logWriter)
	}
	deps := Deps{
		Network:     n,
		SelfAddrs:   testenv.SelfAddrs,
		Listeners:   listeners,
		Logger:      logx.NewLogger(w, slog.LevelDebug),
		Clock:       opt.clock,
		SelfChanged: opt.selfChanged,
		Stats:       opt.stats,
		Live:        opt.live,
	}
	if opt.interfaces != nil {
		deps.Interfaces = opt.interfaces
	}
	if opt.timers != nil {
		deps.Timers = opt.timers
	}
	p, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { p.Serve(); close(done) }()
	t.Cleanup(func() {
		p.Close()
		<-done
	})
	e.p, e.addr = p, addr
}

// goClient returns a Go client that uses the proxy and trusts only the test
// root. It sends no Accept-Encoding of its own.
func (e *env) goClient() (*http.Client, *http.Transport) {
	return e.goClientFrom("")
}

// proxyAddr returns the listener a client at peer connects to: the local
// listener for "", or the listener that reports peer.
func (e *env) proxyAddr(peer string) string {
	e.t.Helper()
	if peer == "" {
		return e.addr
	}
	a, ok := e.remote[peer]
	if !ok {
		e.t.Fatalf("no listener reports peer %s", peer)
	}
	return a
}

// goClientFrom is goClient for a client that the instance sees at peer.
func (e *env) goClientFrom(peer string) (*http.Client, *http.Transport) {
	tr := &http.Transport{
		Proxy:              http.ProxyURL(&url.URL{Scheme: "http", Host: e.proxyAddr(peer)}),
		TLSClientConfig:    &tls.Config{RootCAs: e.ca.Pool()},
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	e.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}, tr
}

// dial opens a raw connection to the proxy.
func (e *env) dial() net.Conn {
	e.t.Helper()
	return e.dialFrom("")
}

// dialFrom opens a raw connection that the instance sees from peer.
func (e *env) dialFrom(peer string) net.Conn {
	e.t.Helper()
	c, err := net.DialTimeout("tcp", e.proxyAddr(peer), 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	e.t.Cleanup(func() { c.Close() })
	return c
}

// raw sends exact request bytes on a new connection and reads one response.
func (e *env) raw(req string) (*http.Response, []byte) {
	e.t.Helper()
	return e.rawFrom("", req)
}

// rawFrom is raw for a client that the instance sees at peer.
func (e *env) rawFrom(peer, req string) (*http.Response, []byte) {
	e.t.Helper()
	c := e.dialFrom(peer)
	if _, err := io.WriteString(c, req); err != nil {
		e.t.Fatal(err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	// Informational responses, such as 100 Continue, precede the final
	// response; 101 is final for this purpose.
	for err == nil && res.StatusCode/100 == 1 && res.StatusCode != http.StatusSwitchingProtocols {
		res, err = http.ReadResponse(br, nil)
	}
	if err != nil {
		e.t.Fatalf("reading response to %q: %v", firstLine(req), err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, body
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\r\n")
	return l
}

// waitDecisions waits until at least n decision lines are logged and returns
// them.
func (e *env) waitDecisions(n int) []map[string]string {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d := e.logs.decisions()
		if len(d) >= n {
			return d
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("want %d decision lines, have %d:\n%s", n, len(d), e.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// noDecisionFor asserts that no decision line appears for a short while.
func (e *env) noDecisionFor(d time.Duration) {
	e.t.Helper()
	time.Sleep(d)
	if got := e.logs.decisions(); len(got) != 0 {
		e.t.Fatalf("unexpected decision lines: %v", got)
	}
}

func (e *env) bytesTotal() int64 { return e.p.Live().Sum(stats.BytesTotal) }

// logBuffer collects log lines safely.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) decisions() []map[string]string {
	var out []map[string]string
	for _, line := range strings.Split(l.String(), "\n") {
		if m := parseLogLine(line); m["msg"] == "decision" {
			out = append(out, m)
		}
	}
	return out
}

// parseLogLine parses one slog text line into its fields.
func parseLogLine(s string) map[string]string {
	m := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ")
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return m
		}
		key := s[:eq]
		s = s[eq+1:]
		if strings.HasPrefix(s, `"`) {
			q, err := strconv.QuotedPrefix(s)
			if err != nil {
				return m
			}
			m[key], _ = strconv.Unquote(q)
			s = s[len(q):]
			continue
		}
		sp := strings.IndexByte(s, ' ')
		if sp < 0 {
			m[key] = s
			return m
		}
		m[key], s = s[:sp], s[sp:]
	}
}

// checkDecision compares selected fields of a decision line.
func checkDecision(t *testing.T, d map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if d[k] != v {
			t.Errorf("decision %s=%q, want %q (line %v)", k, d[k], v, d)
		}
	}
}

// generatedHeaders checks the headers of a response cuttle generated
// (design 0002 D6).
func generatedHeaders(t *testing.T, res *http.Response, status int, proxyStatus string) {
	t.Helper()
	if res.StatusCode != status {
		t.Errorf("status %d, want %d", res.StatusCode, status)
	}
	if got := res.Header.Get("Proxy-Status"); got != proxyStatus {
		t.Errorf("Proxy-Status %q, want %q", got, proxyStatus)
	}
	for k, v := range map[string]string{
		"Content-Type":           "text/plain; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s %q, want %q", k, got, v)
		}
	}
	if !res.Close {
		t.Errorf("response does not close the connection")
	}
}

// connectRaw sends CONNECT target on a new connection and checks the exact
// 200.
func (e *env) connectRaw(target string) net.Conn {
	e.t.Helper()
	return e.connectRawFrom("", target)
}

// connectRawFrom is connectRaw for a client that the instance sees at peer.
func (e *env) connectRawFrom(peer, target string) net.Conn {
	e.t.Helper()
	c := e.dialFrom(peer)
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	got := make([]byte, len(connectEstablished))
	if _, err := io.ReadFull(c, got); err != nil {
		e.t.Fatalf("CONNECT %s: %v (got %q)", target, err, got)
	}
	if string(got) != connectEstablished {
		e.t.Fatalf("CONNECT %s: got %q", target, got)
	}
	return c
}

// byTarget returns the only decision line with this kind and target.
func byTarget(t *testing.T, ds []map[string]string, kind, target string) map[string]string {
	t.Helper()
	var found []map[string]string
	for _, d := range ds {
		if d["kind"] == kind && d["target"] == target {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s decision for %s, have %d: %v", kind, target, len(found), ds)
	}
	return found[0]
}

func mustParse(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, r := config.Parse([]byte(text), "test.toml")
	if cfg == nil {
		t.Fatalf("config: %v", r.Err())
	}
	return cfg
}

func newTestLogger(w io.Writer) *slog.Logger { return logx.NewLogger(w, slog.LevelDebug) }

// newBadOrigin starts an origin that misbehaves by path: /eof closes before a
// response head, /reset resets the connection, /garbage sends an unparsable
// head, and /midbody announces 100 body bytes, sends 10, and closes.
func newBadOrigin(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				switch req.URL.Path {
				case "/reset":
					c.(*net.TCPConn).SetLinger(0)
				case "/garbage":
					io.WriteString(c, "NOT-HTTP garbage\r\n\r\n")
				case "/midbody":
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789")
					time.Sleep(100 * time.Millisecond)
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort()
}
