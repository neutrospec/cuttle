package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
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
		net:   testenv.NewNet(),
		ca:    ca,
		http:  testenv.NewOrigin(t, testenv.OriginOptions{Allow407: opt.allow407}),
		https: testenv.NewOrigin(t, testenv.OriginOptions{TLS: ca}),
		logs:  &logBuffer{},
	}
	n := e.net
	for _, name := range []string{"origin.cuttle.test", "plain.cuttle.test", "ws.cuttle.test", "h"} {
		n.Map(name, "203.0.113.10")
	}
	n.Map("localhost", "127.0.0.1", "::1")
	n.Map("blocked.cuttle.test", "203.0.113.99")
	n.Map("x.blocked.cuttle.test", "203.0.113.99")
	n.Route("203.0.113.10:80", e.http.Addr)
	n.Route("203.0.113.10:443", e.https.Addr)
	n.Route("203.0.113.99:80", e.http.Addr)
	n.Route("203.0.113.99:443", e.https.Addr)

	cfg, r := config.Parse([]byte(cfgText), "test.toml")
	if cfg == nil {
		t.Fatalf("config: %v", r.Err())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var w io.Writer = e.logs
	if opt.logWriter != nil {
		w = io.MultiWriter(e.logs, opt.logWriter)
	}
	p, err := New(cfg, Deps{
		Network:   n,
		SelfAddrs: testenv.SelfAddrs,
		Listeners: []net.Listener{ln},
		Logger:    logx.NewLogger(w, slog.LevelDebug),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { p.Serve(); close(done) }()
	t.Cleanup(func() {
		p.Close()
		<-done
	})
	e.p, e.addr = p, ln.Addr().String()
	return e
}

// goClient returns a Go client that uses the proxy and trusts only the test
// root. It sends no Accept-Encoding of its own.
func (e *env) goClient() (*http.Client, *http.Transport) {
	tr := &http.Transport{
		Proxy:              http.ProxyURL(&url.URL{Scheme: "http", Host: e.addr}),
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
	c, err := net.DialTimeout("tcp", e.addr, 5*time.Second)
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
	c := e.dial()
	if _, err := io.WriteString(c, req); err != nil {
		e.t.Fatal(err)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
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
	c := e.dial()
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
