package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/neutrospec/cuttle/internal/stats"
	"github.com/neutrospec/cuttle/internal/testenv"
)

// viaConfig routes cuttle.test names and h to the upstream rec at
// 10.8.0.6:3128 with disclosure = "via"; blocked.cuttle.test stays
// blocked.
const viaConfig = baseConfig + `
[[upstream]]
name = "rec"
address = "10.8.0.6:3128"
disclosure = "via"

[[rule]]
name = "restricted"
hosts = ["h"]
domains = ["cuttle.test"]
action = "upstream:rec"
`

// recorder starts a recording upstream whose network reaches the env's
// origins under the origin names and extra, and routes the test address at
// to it.
func (e *env) recorder(at string, opt testenv.UpstreamOptions, extra ...string) *testenv.RecordingUpstream {
	e.t.Helper()
	if opt.Net == nil {
		opt.Net = e.originNet()
	}
	for _, name := range extra {
		opt.Net.Map(name, "203.0.113.10")
	}
	rec := testenv.NewRecordingUpstream(e.t, opt)
	e.net.Route(at, rec.Addr)
	return rec
}

// onlyDials checks that every dial of the test network went to one of the
// upstream addresses: an upstream failure never falls back to direct.
func onlyDials(t *testing.T, n *testenv.Net, upstreams ...string) {
	t.Helper()
	for _, d := range n.Dials() {
		if !slices.Contains(upstreams, d.String()) {
			t.Errorf("dial to %s, not an upstream", d)
		}
	}
}

func dialsTo(n *testenv.Net, addr string) int {
	c := 0
	for _, d := range n.Dials() {
		if d.String() == addr {
			c++
		}
	}
	return c
}

// cuttleVia counts the cuttle Via entries among values.
func cuttleVia(values []string) int {
	c := 0
	for _, v := range values {
		for _, m := range splitVia(v) {
			if strings.HasSuffix(strings.TrimSpace(m), "(cuttle)") {
				c++
			}
		}
	}
	return c
}

// hopFields are hop-by-hop fields a client sends; none may reach the
// upstream (design 0003 D3).
var hopFields = []string{"Proxy-Connection", "Connection", "X-Hop", "Keep-Alive", "Proxy-Authorization", "Te", "Trailer", "Upgrade"}

// forwardedFields are disclosure fields that pass unchanged on via routes
// (OI-0006 V2).
var forwardedFields = [][2]string{
	{"Forwarded", "for=198.51.100.7"},
	{"X-Forwarded-For", "198.51.100.7"},
	{"X-Forwarded-Host", "front.example"},
	{"X-Forwarded-Proto", "https"},
	{"X-Forwarded-Port", "443"},
}

func forwardedHead() string {
	var b strings.Builder
	for _, f := range forwardedFields {
		b.WriteString(f[0] + ": " + f[1] + "\r\n")
	}
	return b.String()
}

// checkForwardedPass checks that the client's Forwarded and X-Forwarded-*
// fields arrived unchanged.
func checkForwardedPass(t *testing.T, r testenv.ProxyRequest) {
	t.Helper()
	for _, f := range forwardedFields {
		if got := r.Values(f[0]); len(got) != 1 || got[0] != f[1] {
			t.Errorf("%s: upstream received %q, want %q", f[0], got, f[1])
		}
	}
}

// TestUpstreamConnectVia checks U1 and the CONNECT half of OI-0006 V2:
// through a via upstream, the recording upstream receives the target as
// sent, Host equal to it, the client's User-Agent and Forwarded and
// X-Forwarded-* fields unchanged, no hop-by-hop field, and exactly one
// cuttle Via entry after the client's own. For upstream answers 200, 201,
// 299, and 200 with Content-Length: 3, the client receives exactly the 200
// head and completes TLS with the origin.
func TestUpstreamConnectVia(t *testing.T) {
	e := newEnv(t, viaConfig)
	rec := e.recorder("10.8.0.6:3128", testenv.UpstreamOptions{}, "s201.cuttle.test", "s299.cuttle.test", "cl3.cuttle.test")
	rec.SetMode("s201.cuttle.test", testenv.PassStatus(201, ""))
	rec.SetMode("s299.cuttle.test", testenv.PassStatus(299, ""))
	rec.SetMode("cl3.cuttle.test", testenv.PassStatus(200, "Content-Length: 3\r\n"))

	fields := "User-Agent: test-agent/1.0\r\nProxy-Connection: keep-alive\r\nConnection: keep-alive, X-Hop\r\nX-Hop: secret\r\n" +
		"Keep-Alive: timeout=5\r\nProxy-Authorization: Basic dXNlcjpwYXNz\r\nTe: trailers\r\nTrailer: X-T\r\nUpgrade: h2c\r\n" +
		"Via: 1.1 lab-gw\r\n" + forwardedHead() + "X-Custom: kept\r\n"
	for _, target := range []string{"Origin.Cuttle.TEST:443", "s201.cuttle.test:443", "s299.cuttle.test:443", "cl3.cuttle.test:443"} {
		t.Run(target, func(t *testing.T) {
			rec.Reset()
			c := e.dial()
			defer c.Close() // ends the tunnel, which logs its decision
			io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n"+fields+"\r\n")
			got := make([]byte, len(connectEstablished))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != connectEstablished {
				t.Fatalf("got %q, %v", got, err)
			}
			name := strings.ToLower(strings.TrimSuffix(target, ":443"))
			if res, body := tlsGet(t, c, e, name, "/echo"); res.StatusCode != 200 || !bytes.Contains(body, []byte(`"uri":"/echo"`)) {
				t.Errorf("through the tunnel: %d %s", res.StatusCode, body)
			}
			reqs := rec.Requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream received %d requests", len(reqs))
			}
			r := reqs[0]
			if line := firstLine(string(r.Head)); line != "CONNECT "+target+" HTTP/1.1" {
				t.Errorf("request line %q", line)
			}
			if h := r.Values("Host"); len(h) != 1 || h[0] != target {
				t.Errorf("Host %q", h)
			}
			if ua := r.Values("User-Agent"); len(ua) != 1 || ua[0] != "test-agent/1.0" {
				t.Errorf("User-Agent %q", ua)
			}
			for _, h := range hopFields {
				if v := r.Values(h); len(v) > 0 {
					t.Errorf("hop-by-hop %s reached the upstream: %q", h, v)
				}
			}
			if via := r.Values("Via"); len(via) != 2 || via[0] != "1.1 lab-gw" || via[1] != "1.1 cuttle-test (cuttle)" || cuttleVia(via) != 1 {
				t.Errorf("Via %q", via)
			}
			checkForwardedPass(t, r)
			if r.Get("X-Custom") != "kept" {
				t.Errorf("X-Custom %q", r.Get("X-Custom"))
			}
			// Nothing else is written: Host, the client's end-to-end
			// fields, and the two Via lines.
			if len(r.Fields) != 1+1+2+len(forwardedFields)+1 {
				t.Errorf("fields %v", r.Fields)
			}
		})
	}

	t.Run("Go client", func(t *testing.T) {
		client, tr := e.goClient()
		res, err := client.Get("https://origin.cuttle.test/echo")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		tr.CloseIdleConnections()
		if res.StatusCode != 200 {
			t.Errorf("status %d", res.StatusCode)
		}
	})

	ds := e.waitDecisions(5)
	for _, d := range ds {
		checkDecision(t, d, map[string]string{"kind": "connect", "rule": "restricted", "action": "upstream", "upstream": "rec",
			"outcome": "ok", "status": "200"})
	}
	if got := e.p.Live().Get(stats.UpstreamOutcomesTotal, "upstream", "rec", "outcome", "ok"); got != 5 {
		t.Errorf("upstream_outcomes_total ok %d", got)
	}
	onlyDials(t, e.net, "10.8.0.6:3128")
	if l := e.net.Lookups(); len(l) != 0 {
		t.Errorf("lookups %v", l)
	}
}

// TestUpstreamBufferedBytes checks U2: bytes the upstream sends right after
// its 2xx head reach the client after cuttle's 200, and bytes the client
// pipelined after its CONNECT head reach the upstream. Both count as
// tunnel bytes.
func TestUpstreamBufferedBytes(t *testing.T) {
	e := newEnv(t, viaConfig)
	rec := e.recorder("10.8.0.6:3128", testenv.UpstreamOptions{})
	rec.SetMode("early.cuttle.test", testenv.Buffered([]byte("EARLY-BYTES")))

	c := e.dial()
	io.WriteString(c, "CONNECT early.cuttle.test:443 HTTP/1.1\r\nHost: early.cuttle.test:443\r\n\r\nPIPELINED")
	want := connectEstablished + "EARLY-BYTES" + "PIPELINED"
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != want {
		t.Fatalf("got %q, %v", got, err)
	}
	io.WriteString(c, "MORE")
	got = make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "MORE" {
		t.Fatalf("echo %q, %v", got, err)
	}
	c.Close()
	d := e.waitDecisions(1)[0]
	checkDecision(t, d, map[string]string{"outcome": "ok", "status": "200", "up": "13", "down": "24"})
	raw := string(rec.Raw(1))
	if !strings.HasSuffix(raw, "\r\n\r\nPIPELINEDMORE") {
		t.Errorf("upstream received %q", raw)
	}
}

// TestUpstreamStatusMapping checks U3: upstream 403, 407, and 502 reach the
// client as 502 with received-status, 504 as 504, with the upstream's
// Proxy-Status members first; no Proxy-Authenticate is relayed; no direct
// dial is made. Checked with Go (OnProxyConnectResponse), curl, and a raw
// client.
func TestUpstreamStatusMapping(t *testing.T) {
	e := newEnv(t, viaConfig)
	rec := e.recorder("10.8.0.6:3128", testenv.UpstreamOptions{Allow407: true})
	rec.SetMode("s403.cuttle.test", testenv.Status(403, ""))
	rec.SetMode("s407.cuttle.test", testenv.Status(407, ""))
	rec.SetMode("s502.cuttle.test", testenv.Status(502, "Proxy-Status: mock-egress; error=connection_refused\r\n"))
	rec.SetMode("s504.cuttle.test", testenv.Status(504, ""))
	cases := []struct {
		name   string
		status int
		ps     string
	}{
		{"s403.cuttle.test", 502, `cuttle-test; received-status=403; next-hop="rec"`},
		{"s407.cuttle.test", 502, `cuttle-test; received-status=407; next-hop="rec"`},
		{"s502.cuttle.test", 502, `mock-egress; error=connection_refused, cuttle-test; received-status=502; next-hop="rec"`},
		{"s504.cuttle.test", 504, `cuttle-test; received-status=504; next-hop="rec"`},
	}

	client, tr := e.goClient()
	var mu sync.Mutex
	var captured *http.Response
	tr.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
		mu.Lock()
		defer mu.Unlock()
		captured = res
		return nil
	}
	ca := e.ca.WritePEM(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := client.Get("https://" + c.name + "/"); err == nil {
				t.Error("Go client succeeded")
			}
			mu.Lock()
			res := captured
			captured = nil
			mu.Unlock()
			if res == nil || res.StatusCode != c.status || res.Header.Get("Proxy-Status") != c.ps || res.Header.Get("Proxy-Authenticate") != "" {
				t.Errorf("Go: %+v", res)
			}

			r := testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "--cacert", ca, "-w", "%{http_connect}", "-o", "/dev/null",
				"https://"+c.name+"/")
			if r.Stdout != strconv.Itoa(c.status) || r.Code != 56 {
				t.Errorf("curl: exit %d out %q err %q", r.Code, r.Stdout, r.Stderr)
			}

			res, _ = e.raw("CONNECT " + c.name + ":443 HTTP/1.1\r\nHost: " + c.name + ":443\r\n\r\n")
			generatedHeaders(t, res, c.status, c.ps)
			if _, ok := res.Header["Proxy-Authenticate"]; ok {
				t.Error("Proxy-Authenticate relayed")
			}
		})
	}
	ds := e.waitDecisions(12)
	for _, d := range ds {
		// The decision line keeps the upstream's status, so an egress 403,
		// 407, and 502 can be told apart (design 0002 D8).
		code := strings.TrimSuffix(strings.TrimPrefix(d["target"], "s"), ".cuttle.test:443")
		want := "502"
		if code == "504" {
			want = "504"
		}
		checkDecision(t, d, map[string]string{"action": "upstream", "upstream": "rec", "outcome": "upstream_status",
			"status": want, "upstream_status": code})
	}
	if line := e.logs.String(); !strings.Contains(line, "outcome=upstream_status status=502 upstream_status=407 up=0 down=0") {
		t.Errorf("decision line layout:\n%s", line)
	}
	onlyDials(t, e.net, "10.8.0.6:3128")
	if l := e.net.Lookups(); len(l) != 0 {
		t.Errorf("lookups %v", l)
	}
}

// TestUpstreamRefusedAndTimeout checks U4: a refused upstream gives 502
// connection_refused; one that accepts and never answers gives 504
// http_response_timeout within response_timeout + 1 s; for CONNECT and
// plain HTTP, without a direct dial.
func TestUpstreamRefusedAndTimeout(t *testing.T) {
	cfg := baseConfig + `
[[upstream]]
name = "closed"
address = "10.8.0.6:8081"
disclosure = "none"
unreachable_hold = "0s"

[[upstream]]
name = "silent"
address = "10.8.0.6:8082"
disclosure = "none"
response_timeout = "500ms"

[[rule]]
name = "closed"
hosts = ["closed.cuttle.test"]
action = "upstream:closed"

[[rule]]
name = "silent"
hosts = ["silent.cuttle.test"]
action = "upstream:silent"
`
	e := newEnv(t, cfg)
	e.net.Route("10.8.0.6:8081", closedAddr(t))
	rec := e.recorder("10.8.0.6:8082", testenv.UpstreamOptions{})
	rec.SetMode("silent.cuttle.test", testenv.Silent())

	for _, c := range []struct {
		req    string
		status int
		ps     string
		max    time.Duration
	}{
		{"CONNECT closed.cuttle.test:443 HTTP/1.1\r\nHost: closed.cuttle.test:443\r\n\r\n", 502,
			`cuttle-test; error=connection_refused; next-hop="closed"; details="connect failed: connection refused"`, time.Second},
		{"GET http://closed.cuttle.test/ HTTP/1.1\r\nHost: closed.cuttle.test\r\n\r\n", 502,
			`cuttle-test; error=connection_refused; next-hop="closed"; details="connect failed: connection refused"`, time.Second},
		{"CONNECT silent.cuttle.test:443 HTTP/1.1\r\nHost: silent.cuttle.test:443\r\n\r\n", 504,
			`cuttle-test; error=http_response_timeout; next-hop="silent"; details="no response head within 500ms"`, 1500 * time.Millisecond},
		{"GET http://silent.cuttle.test/ HTTP/1.1\r\nHost: silent.cuttle.test\r\n\r\n", 504,
			`cuttle-test; error=http_response_timeout; next-hop="silent"; details="no response head within 500ms"`, 1500 * time.Millisecond},
	} {
		start := time.Now()
		res, _ := e.raw(c.req)
		generatedHeaders(t, res, c.status, c.ps)
		if el := time.Since(start); el > c.max {
			t.Errorf("%s: took %v", firstLine(c.req), el)
		}
	}
	ds := e.waitDecisions(4)
	for i, want := range []string{"refused", "refused", "timeout", "timeout"} {
		checkDecision(t, ds[i], map[string]string{"action": "upstream", "outcome": want})
	}
	onlyDials(t, e.net, "10.8.0.6:8081", "10.8.0.6:8082")
	if l := e.net.Lookups(); len(l) != 0 {
		t.Errorf("lookups %v", l)
	}
}

// closedAddr returns a loopback address whose port the test bound and
// closed, so a dial to it is refused (design 0007 D2).
func closedAddr(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().(*net.TCPAddr).AddrPort()
	ln.Close()
	return a
}

// TestUpstreamHTTP checks U5 and the plain-HTTP half of OI-0006 V2: plain
// HTTP through a via upstream arrives with the client's raw absolute-form
// target, the client's Forwarded and X-Forwarded-* fields unchanged, one
// cuttle Via entry after the client's own, and the client's body bytes; an
// upstream 407 becomes 502; a 403 page is relayed unchanged; an
// absolute-form WebSocket upgrade completes an echo round trip.
func TestUpstreamHTTP(t *testing.T) {
	e := newEnv(t, viaConfig)
	rec := e.recorder("10.8.0.6:3128", testenv.UpstreamOptions{Allow407: true})
	rec.SetMode("h", testenv.Pass())
	rec.SetMode("s407.cuttle.test", testenv.Status(407, ""))
	rec.SetMode("s403.cuttle.test", testenv.Status(403, ""))

	t.Run("raw target and fields", func(t *testing.T) {
		for _, target := range []string{"http://h/a{b}|c?x=1;y=2&z=%zz", "http://h/q?b=2&a=1;c=3", "http://h/p?"} {
			rec.Reset()
			res, _ := e.raw("GET " + target + " HTTP/1.1\r\nHost: h\r\nUser-Agent: raw/1\r\nVia: 1.1 lab-gw\r\n" + forwardedHead() +
				"Connection: close\r\n\r\n")
			if res.StatusCode != 200 {
				t.Errorf("%s: status %d", target, res.StatusCode)
			}
			reqs := rec.Requests()
			if len(reqs) != 1 {
				t.Fatalf("%s: upstream received %d requests", target, len(reqs))
			}
			r := reqs[0]
			if r.Method != "GET" || r.Target != target || r.Get("Host") != "h" || r.Get("User-Agent") != "raw/1" {
				t.Errorf("%s: got %s %s Host %q UA %q", target, r.Method, r.Target, r.Get("Host"), r.Get("User-Agent"))
			}
			if via := r.Values("Via"); len(via) != 2 || via[0] != "1.1 lab-gw" || via[1] != "1.1 cuttle-test (cuttle)" {
				t.Errorf("%s: Via %q", target, via)
			}
			checkForwardedPass(t, r)
			for _, k := range []string{"Accept-Encoding", "Proxy-Status", "X-Real-Ip"} {
				if v := r.Values(k); len(v) > 0 {
					t.Errorf("%s: %s added: %q", target, k, v)
				}
			}
		}
	})

	t.Run("fields named in Connection", func(t *testing.T) {
		// A Forwarded or X-Forwarded-* field the client names in Connection
		// is hop-by-hop: it is removed on the plain-HTTP path as in the
		// CONNECT head (TASK-0006 U-R1, RFC 9110 section 7.6.1).
		for _, kind := range []string{"plain", "connect"} {
			rec.Reset()
			head := "Host: h\r\nConnection: forwarded, X-FORWARDED-FOR , x-forwarded-port\r\n" + forwardedHead()
			if kind == "plain" {
				if res, _ := e.raw("GET http://h/conn HTTP/1.1\r\n" + head + "\r\n"); res.StatusCode != 200 {
					t.Fatalf("status %d", res.StatusCode)
				}
			} else {
				c := e.dial()
				io.WriteString(c, "CONNECT origin.cuttle.test:443 HTTP/1.1\r\n"+head+"\r\n")
				got := make([]byte, len(connectEstablished))
				if _, err := io.ReadFull(c, got); err != nil || string(got) != connectEstablished {
					t.Fatalf("CONNECT: %q %v", got, err)
				}
				c.Close()
			}
			r := rec.Requests()[0]
			for _, k := range []string{"Connection", "Forwarded", "X-Forwarded-For", "X-Forwarded-Port"} {
				if v := r.Values(k); len(v) > 0 {
					t.Errorf("%s: %s named in Connection arrived: %q", kind, k, v)
				}
			}
			for _, k := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := r.Values(k); len(v) != 1 {
					t.Errorf("%s: %s not named in Connection: %q", kind, k, v)
				}
			}
		}
	})

	t.Run("bodies", func(t *testing.T) {
		client, _ := e.goClient()
		body := bytes.Repeat([]byte("0123456789"), 3000)
		for _, chunked := range []bool{false, true} {
			rec.Reset()
			var rd io.Reader = bytes.NewReader(body)
			if chunked {
				rd = io.MultiReader(rd)
			}
			res, err := client.Post("http://plain.cuttle.test/echo", "application/octet-stream", rd)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			r := rec.Requests()[0]
			if res.StatusCode != 200 || r.Method != "POST" || !bytes.Equal(r.Body, body) || cuttleVia(r.Values("Via")) != 1 {
				t.Errorf("chunked=%v: %d %s body %d bytes Via %q", chunked, res.StatusCode, r.Method, len(r.Body), r.Values("Via"))
			}
			if chunked != (r.Get("Transfer-Encoding") == "chunked") {
				t.Errorf("chunked=%v: framing %v", chunked, r.Fields)
			}
		}
	})

	t.Run("407 and 403", func(t *testing.T) {
		res, _ := e.raw("GET http://s407.cuttle.test/x HTTP/1.1\r\nHost: s407.cuttle.test\r\n\r\n")
		generatedHeaders(t, res, 502, `cuttle-test; received-status=407; next-hop="rec"`)
		if _, ok := res.Header["Proxy-Authenticate"]; ok {
			t.Error("Proxy-Authenticate relayed")
		}
		res, body := e.raw("GET http://s403.cuttle.test/x HTTP/1.1\r\nHost: s403.cuttle.test\r\n\r\n")
		if res.StatusCode != 403 || string(body) != "recording upstream: 403" || res.Header.Get("X-Mock") != "recording-upstream" ||
			res.Header.Get("Proxy-Status") != "" || res.Header.Get("Via") != "" {
			t.Errorf("403 page changed: %d %q %v", res.StatusCode, body, res.Header)
		}
		// The mapped 407 keeps the upstream's status in the decision line;
		// the relayed 403 has no such field (design 0002 D8, TASK-0006
		// V-R1).
		lines := func(target string) []map[string]string {
			var out []map[string]string
			for _, d := range e.logs.decisions() {
				if d["kind"] == "http" && d["target"] == target {
					out = append(out, d)
				}
			}
			return out
		}
		waitFor(t, "decision lines", func() bool { return len(lines("s407.cuttle.test:80")) == 1 && len(lines("s403.cuttle.test:80")) == 1 })
		checkDecision(t, lines("s407.cuttle.test:80")[0], map[string]string{"action": "upstream", "upstream": "rec",
			"outcome": "upstream_status", "status": "502", "upstream_status": "407"})
		checkDecision(t, lines("s403.cuttle.test:80")[0], map[string]string{"action": "upstream", "upstream": "rec",
			"outcome": "ok", "status": "403", "upstream_status": ""})
	})

	t.Run("WebSocket", func(t *testing.T) {
		rec.Reset()
		c := e.dial()
		loc := &url.URL{Scheme: "http", Opaque: "//ws.cuttle.test/ws", Host: "ws.cuttle.test"}
		origin, _ := url.Parse("http://ws.cuttle.test/")
		ws, err := websocket.NewClient(&websocket.Config{Location: loc, Origin: origin, Version: websocket.ProtocolVersionHybi13}, c)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		if err := websocket.Message.Send(ws, "hello upstream"); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := websocket.Message.Receive(ws, &got); err != nil || got != "hello upstream" {
			t.Fatalf("echo %q %v", got, err)
		}
		r := rec.Requests()[0]
		if r.Target != "http://ws.cuttle.test/ws" || r.Get("Upgrade") != "websocket" || cuttleVia(r.Values("Via")) != 1 {
			t.Errorf("upgrade request %s %v", r.Target, r.Fields)
		}
	})

	onlyDials(t, e.net, "10.8.0.6:3128")
}

// TestUpstreamSelfDial checks the dial half of U9: an upstream name that
// the test network resolves to the instance's own listener fails at dial
// time with 502 proxy_loop_detected, without a dial.
func TestUpstreamSelfDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := baseConfig + fmt.Sprintf(`
[[upstream]]
name = "self"
address = "self.cuttle.test:%d"
disclosure = "via"

[[rule]]
name = "self"
domains = ["loop.cuttle.test"]
action = "upstream:self"
`, port)
	e := newEnvWith(t, cfg, envOptions{listener: ln})
	e.net.Map("self.cuttle.test", "127.0.0.1")
	ps := fmt.Sprintf(`cuttle-test; error=proxy_loop_detected; next-hop="self"; details="upstream address 127.0.0.1:%d is this instance's own listener"`, port)
	res, _ := e.raw("CONNECT loop.cuttle.test:443 HTTP/1.1\r\nHost: loop.cuttle.test:443\r\n\r\n")
	generatedHeaders(t, res, 502, ps)
	res, _ = e.raw("GET http://loop.cuttle.test/ HTTP/1.1\r\nHost: loop.cuttle.test\r\n\r\n")
	generatedHeaders(t, res, 502, ps)
	for _, d := range e.waitDecisions(2) {
		checkDecision(t, d, map[string]string{"action": "upstream", "upstream": "self", "outcome": "loop", "error": "proxy_loop_detected"})
	}
	if d := e.net.Dials(); len(d) != 0 {
		t.Errorf("dials %v", d)
	}
	if st := e.p.Upstreams()[0]; st.State != StateUp {
		t.Errorf("a loop held the upstream: %+v", st)
	}
}

// fakeClock is an injected clock for the unreachable hold.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const holdConfig = baseConfig + `
[[upstream]]
name = "hole"
address = "10.8.0.6:8081"
disclosure = "none"
connect_timeout = "300ms"
unreachable_hold = "1s"

[[upstream]]
name = "plain"
address = "10.8.0.6:8082"
disclosure = "none"
connect_timeout = "300ms"
unreachable_hold = "1s"

[[upstream]]
name = "nohold"
address = "10.8.0.6:8083"
disclosure = "none"
connect_timeout = "300ms"
unreachable_hold = "0s"

[[upstream]]
name = "silent"
address = "10.8.0.6:8084"
disclosure = "none"
response_timeout = "300ms"
unreachable_hold = "1s"

[[upstream]]
name = "bad"
address = "10.8.0.6:8085"
disclosure = "none"
unreachable_hold = "1s"

[[upstream]]
name = "nodns"
address = "gone.internal.example:8080"
disclosure = "none"
unreachable_hold = "1s"

[[upstream]]
name = "abandon"
address = "10.8.0.6:8086"
disclosure = "none"
connect_timeout = "2s"
unreachable_hold = "1s"

[[rule]]
name = "hole"
hosts = ["hole.cuttle.test"]
action = "upstream:hole"

[[rule]]
name = "nodns"
hosts = ["nodns.cuttle.test"]
action = "upstream:nodns"

[[rule]]
name = "abandon"
hosts = ["abandon.cuttle.test"]
action = "upstream:abandon"

[[rule]]
name = "plain"
hosts = ["plain-up.cuttle.test"]
action = "upstream:plain"

[[rule]]
name = "nohold"
hosts = ["nohold.cuttle.test"]
action = "upstream:nohold"

[[rule]]
name = "silent"
hosts = ["silent.cuttle.test"]
action = "upstream:silent"

[[rule]]
name = "bad"
hosts = ["bad.cuttle.test"]
action = "upstream:bad"
`

// TestUpstreamUnreachableHold checks U11 and OI-0006 V3 with an injected
// clock. Each subtest uses its own upstreams, so none depends on another's
// order.
func TestUpstreamUnreachableHold(t *testing.T) {
	clock := newFakeClock()
	e := newEnvWith(t, holdConfig, envOptions{clock: clock.Now})
	rec := e.recorder("10.8.0.6:8081", testenv.UpstreamOptions{}, "hole.cuttle.test", "plain-up.cuttle.test", "abandon.cuttle.test")
	rec.SetMode("silent.cuttle.test", testenv.Silent())
	rec.SetMode("bad.cuttle.test", testenv.Status(502, ""))
	closed := closedAddr(t)
	e.net.Blackhole("10.8.0.6:8081")
	e.net.Blackhole("10.8.0.6:8082")
	e.net.Route("10.8.0.6:8082", rec.Addr)
	e.net.Route("10.8.0.6:8083", closed)
	e.net.Route("10.8.0.6:8084", rec.Addr)
	e.net.Route("10.8.0.6:8085", rec.Addr)

	connect := func(host string) (*http.Response, time.Duration) {
		t.Helper()
		start := time.Now()
		res, _ := e.raw("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n")
		return res, time.Since(start)
	}
	get := func(host string) (*http.Response, time.Duration) {
		t.Helper()
		start := time.Now()
		res, _ := e.raw("GET http://" + host + "/echo HTTP/1.1\r\nHost: " + host + "\r\n\r\n")
		return res, time.Since(start)
	}
	state := func(name string) UpstreamStatus {
		for _, st := range e.p.Upstreams() {
			if st.Name == name {
				return st
			}
		}
		t.Fatalf("no upstream %s", name)
		return UpstreamStatus{}
	}
	gauge := func(name, s string) int64 { return e.p.Live().Get(stats.UpstreamState, "upstream", name, "state", s) }
	held := func(name, age string) string {
		return `cuttle-test; error=destination_unavailable; next-hop="` + name + `"; details="unreachable for ` + age + `"`
	}
	const fast = 250 * time.Millisecond

	t.Run("CONNECT hold, probe, and recovery", func(t *testing.T) {
		res, _ := connect("hole.cuttle.test")
		generatedHeaders(t, res, 504, `cuttle-test; error=connection_timeout; next-hop="hole"; details="connect timed out after 300ms"`)
		if n := dialsTo(e.net, "10.8.0.6:8081"); n != 1 {
			t.Fatalf("dials %d", n)
		}
		if st := state("hole"); st.State != StateDown || st.LastError != "connection_timeout" || gauge("hole", "down") != 1 || gauge("hole", "up") != 0 {
			t.Fatalf("state %+v", st)
		}
		if !strings.Contains(e.logs.String(), `msg="upstream unreachable" upstream=hole address=10.8.0.6:8081 error=connection_timeout hold=1s`) {
			t.Errorf("no warning:\n%s", e.logs.String())
		}
		for i := 1; i <= 4; i++ {
			clock.Advance(200 * time.Millisecond)
			res, el := connect("hole.cuttle.test")
			generatedHeaders(t, res, 503, held("hole", fmt.Sprintf("%dms", 200*i)))
			if el > fast {
				t.Errorf("held request took %v", el)
			}
		}
		if n := dialsTo(e.net, "10.8.0.6:8081"); n != 1 {
			t.Fatalf("dials during the hold: %d", n)
		}

		// The hold expires: one probe dials while a concurrent request
		// fails at once.
		clock.Advance(300 * time.Millisecond)
		probe := make(chan int, 1)
		go func() {
			probe <- rawStatus(e.addr, "CONNECT hole.cuttle.test:443 HTTP/1.1\r\nHost: hole.cuttle.test:443\r\n\r\n")
		}()
		waitFor(t, "probe dial", func() bool { return dialsTo(e.net, "10.8.0.6:8081") == 2 })
		// While probing, the upstream has been down since the first failure
		// of the outage, 1.1 s ago on the injected clock, not since the probe
		// started (cuttle status down_s, design 0006 D4).
		if st := state("hole"); st.State != StateProbing || gauge("hole", "probing") != 1 || st.DownFor != 1100*time.Millisecond {
			t.Errorf("state during the probe %+v", st)
		}
		res, el := connect("hole.cuttle.test")
		generatedHeaders(t, res, 503, held("hole", "1s"))
		if el > fast {
			t.Errorf("request during the probe took %v", el)
		}
		if code := <-probe; code != 504 {
			t.Errorf("probe got %d", code)
		}
		if n := dialsTo(e.net, "10.8.0.6:8081"); n != 2 {
			t.Errorf("dials after the probe: %d", n)
		}
		if st := state("hole"); st.State != StateDown {
			t.Errorf("state after the failed probe %+v", st)
		}

		// The address now routes to the recording upstream: the next probe
		// succeeds and the upstream is up for everyone.
		e.net.Unblackhole("10.8.0.6:8081")
		clock.Advance(1100 * time.Millisecond)
		c := e.connectRaw("hole.cuttle.test:443")
		c.Close()
		if st := state("hole"); st.State != StateUp || gauge("hole", "up") != 1 || gauge("hole", "down") != 0 {
			t.Errorf("state after recovery %+v", st)
		}
		c = e.connectRaw("hole.cuttle.test:443")
		c.Close()
		if n := dialsTo(e.net, "10.8.0.6:8081"); n != 4 {
			t.Errorf("dials after recovery: %d", n)
		}
		logs := e.logs.String()
		if n := strings.Count(logs, `msg="upstream recovered" upstream=hole`); n != 1 ||
			!strings.Contains(logs, `msg="upstream recovered" upstream=hole address=10.8.0.6:8081 down=2.2s failed_fast=5`) {
			t.Errorf("recovery lines %d:\n%s", n, logs)
		}
		if n := strings.Count(logs, `msg="upstream unreachable" upstream=hole`); n != 1 {
			t.Errorf("%d unreachable warnings", n)
		}
		if got := e.p.Live().Get(stats.UpstreamOutcomesTotal, "upstream", "hole", "outcome", OutcomeUnavailable); got != 5 {
			t.Errorf("unavailable outcomes %d", got)
		}
	})

	t.Run("plain HTTP hold and pooled connections", func(t *testing.T) {
		res, _ := get("plain-up.cuttle.test")
		generatedHeaders(t, res, 504, `cuttle-test; error=connection_timeout; next-hop="plain"; details="connect timed out after 300ms"`)
		if st := state("plain"); st.State != StateDown || dialsTo(e.net, "10.8.0.6:8082") != 1 {
			t.Fatalf("state %+v", st)
		}
		res, el := get("plain-up.cuttle.test")
		generatedHeaders(t, res, 503, held("plain", "0s"))
		res2, el2 := connect("plain-up.cuttle.test")
		generatedHeaders(t, res2, 503, held("plain", "0s"))
		if el > fast || el2 > fast || dialsTo(e.net, "10.8.0.6:8082") != 1 {
			t.Errorf("held requests took %v and %v; dials %d", el, el2, dialsTo(e.net, "10.8.0.6:8082"))
		}

		// The probe goes through the transport's dial function.
		e.net.Unblackhole("10.8.0.6:8082")
		clock.Advance(1100 * time.Millisecond)
		if res, _ := get("plain-up.cuttle.test"); res.StatusCode != 200 {
			t.Fatalf("probe status %d", res.StatusCode)
		}
		if st := state("plain"); st.State != StateUp || dialsTo(e.net, "10.8.0.6:8082") != 2 {
			t.Fatalf("after the probe: %+v, dials %d", st, dialsTo(e.net, "10.8.0.6:8082"))
		}
		// The transport now holds an idle connection to the upstream. When
		// the upstream goes down, that connection is closed, so the next
		// probe dials instead of reusing it.
		if res, _ := get("plain-up.cuttle.test"); res.StatusCode != 200 || dialsTo(e.net, "10.8.0.6:8082") != 2 {
			t.Fatalf("pooled request: %d, dials %d", res.StatusCode, dialsTo(e.net, "10.8.0.6:8082"))
		}
		conns := rec.Connections()
		e.net.Route("10.8.0.6:8082", closed)
		res, _ = connect("plain-up.cuttle.test")
		generatedHeaders(t, res, 502, `cuttle-test; error=connection_refused; next-hop="plain"; details="connect failed: connection refused"`)
		if st := state("plain"); st.State != StateDown {
			t.Fatalf("state %+v", st)
		}
		e.net.Route("10.8.0.6:8082", rec.Addr)
		clock.Advance(1100 * time.Millisecond)
		if res, _ := get("plain-up.cuttle.test"); res.StatusCode != 200 {
			t.Fatalf("second probe status %d", res.StatusCode)
		}
		if n := dialsTo(e.net, "10.8.0.6:8082"); n != 4 || rec.Connections() != conns+1 {
			t.Errorf("the probe reused a pooled connection: dials %d, upstream connections %d then %d", n, conns, rec.Connections())
		}
		if st := state("plain"); st.State != StateUp {
			t.Errorf("state %+v", st)
		}

		// The handler checks the hold before ReverseProxy, so a request
		// during the hold never reaches the transport. Without that check,
		// a held request would queue on the transport, which makes Go keep
		// connections that become idle again; a connection busy when the
		// upstream went down would then return to the pool and carry the
		// next request during the hold.
		slow := make(chan int, 1)
		go func() {
			slow <- rawStatus(e.addr, "GET http://plain-up.cuttle.test/slow?ms=800 HTTP/1.1\r\nHost: plain-up.cuttle.test\r\n\r\n")
		}()
		waitFor(t, "slow request at the upstream", func() bool {
			reqs := rec.Requests()
			return len(reqs) > 0 && strings.Contains(reqs[len(reqs)-1].Target, "/slow")
		})
		e.net.Route("10.8.0.6:8082", closed)
		res, _ = connect("plain-up.cuttle.test")
		generatedHeaders(t, res, 502, `cuttle-test; error=connection_refused; next-hop="plain"; details="connect failed: connection refused"`)
		before := len(rec.Requests())
		res, el = get("plain-up.cuttle.test")
		generatedHeaders(t, res, 503, held("plain", "0s"))
		if code := <-slow; code != 200 {
			t.Fatalf("slow request %d", code)
		}
		res2, el2 = get("plain-up.cuttle.test")
		generatedHeaders(t, res2, 503, held("plain", "0s"))
		if el > fast || el2 > fast || len(rec.Requests()) != before {
			t.Errorf("held requests took %v and %v and reached the upstream %d times", el, el2, len(rec.Requests())-before)
		}
	})

	t.Run("silent and 502 upstreams are never held", func(t *testing.T) {
		for range 2 {
			res, _ := connect("silent.cuttle.test")
			generatedHeaders(t, res, 504, `cuttle-test; error=http_response_timeout; next-hop="silent"; details="no response head within 300ms"`)
			res, _ = get("silent.cuttle.test")
			generatedHeaders(t, res, 504, `cuttle-test; error=http_response_timeout; next-hop="silent"; details="no response head within 300ms"`)
			res, _ = connect("bad.cuttle.test")
			generatedHeaders(t, res, 502, `cuttle-test; received-status=502; next-hop="bad"`)
		}
		if n := dialsTo(e.net, "10.8.0.6:8084"); n != 4 {
			t.Errorf("silent dials %d", n)
		}
		if n := dialsTo(e.net, "10.8.0.6:8085"); n != 2 {
			t.Errorf("502 dials %d", n)
		}
		if st := state("silent"); st.State != StateUp {
			t.Errorf("silent %+v", st)
		}
		if st := state("bad"); st.State != StateUp {
			t.Errorf("bad %+v", st)
		}
	})

	t.Run("upstream name not resolvable", func(t *testing.T) {
		// A failure to resolve the upstream's own name answers 502
		// dns_error and starts the hold (design 0003 D6, D8).
		e.net.MapError("gone.internal.example", &net.DNSError{Err: "server misbehaving", Name: "gone.internal.example", IsTemporary: true})
		dials := len(e.net.Dials())
		res, _ := connect("nodns.cuttle.test")
		generatedHeaders(t, res, 502, `cuttle-test; error=dns_error; next-hop="nodns"; details="upstream lookup failed: server failure"`)
		if st := state("nodns"); st.State != StateDown || st.LastError != "dns_error" || gauge("nodns", "down") != 1 {
			t.Fatalf("state %+v", st)
		}
		res, el := connect("nodns.cuttle.test")
		generatedHeaders(t, res, 503, held("nodns", "0s"))
		res2, el2 := get("nodns.cuttle.test")
		generatedHeaders(t, res2, 503, held("nodns", "0s"))
		if el > fast || el2 > fast {
			t.Errorf("held requests took %v and %v", el, el2)
		}
		var lookups []string
		for _, l := range e.net.Lookups() {
			if l != "localhost" {
				lookups = append(lookups, l)
			}
		}
		if strings.Join(lookups, ",") != "gone.internal.example" || len(e.net.Dials()) != dials {
			t.Errorf("lookups %v, %d new dials", lookups, len(e.net.Dials())-dials)
		}
		if n := strings.Count(e.logs.String(), `msg="upstream unreachable" upstream=nodns address=gone.internal.example:8080 error=dns_error hold=1s`); n != 1 {
			t.Errorf("%d unreachable warnings", n)
		}
	})

	t.Run("abandoned dials", func(t *testing.T) {
		// A dial abandoned because the client left is not a failure: it
		// starts no hold, and an abandoned probe leaves the upstream due for
		// another probe at once (design 0003 D8).
		e.net.Blackhole("10.8.0.6:8086")
		// closedLines returns the decision lines of abandoned requests.
		closedLines := func() []map[string]string {
			var out []map[string]string
			for _, d := range e.logs.decisions() {
				if d["target"] == "abandon.cuttle.test:443" && d["outcome"] == OutcomeClientClosed {
					out = append(out, d)
				}
			}
			return out
		}
		abandon := func(wantDials int) {
			t.Helper()
			before := len(closedLines())
			start := time.Now()
			c := e.dial()
			io.WriteString(c, "CONNECT abandon.cuttle.test:443 HTTP/1.1\r\nHost: abandon.cuttle.test:443\r\n\r\n")
			waitFor(t, "dial to the upstream", func() bool { return dialsTo(e.net, "10.8.0.6:8086") == wantDials })
			c.Close()
			waitFor(t, "client_closed decision", func() bool { return len(closedLines()) == before+1 })
			checkDecision(t, closedLines()[before], map[string]string{"status": "0", "upstream": "abandon"})
			if el := time.Since(start); el > time.Second {
				t.Errorf("abandoned dial took %v; connect_timeout is 2s", el)
			}
		}
		abandon(1)
		if st := state("abandon"); st.State != StateUp || st.LastError != "" || gauge("abandon", "up") != 1 {
			t.Fatalf("an abandoned dial changed the state: %+v", st)
		}
		// Not held: the next request dials, and its refused connect starts
		// the hold.
		e.net.Unblackhole("10.8.0.6:8086")
		e.net.Route("10.8.0.6:8086", closed)
		res, _ := connect("abandon.cuttle.test")
		generatedHeaders(t, res, 502, `cuttle-test; error=connection_refused; next-hop="abandon"; details="connect failed: connection refused"`)
		if st := state("abandon"); st.State != StateDown || dialsTo(e.net, "10.8.0.6:8086") != 2 {
			t.Fatalf("state %+v", st)
		}

		// The probe after the hold is abandoned; the next request probes at
		// once, without waiting for another hold.
		e.net.Blackhole("10.8.0.6:8086")
		clock.Advance(1100 * time.Millisecond)
		abandon(3)
		if st := state("abandon"); st.State != StateDown {
			t.Fatalf("state after the abandoned probe %+v", st)
		}
		e.net.Unblackhole("10.8.0.6:8086")
		e.net.Route("10.8.0.6:8086", rec.Addr)
		c := e.connectRaw("abandon.cuttle.test:443")
		c.Close()
		if st := state("abandon"); st.State != StateUp || dialsTo(e.net, "10.8.0.6:8086") != 4 {
			t.Errorf("after the next probe: %+v, dials %d", st, dialsTo(e.net, "10.8.0.6:8086"))
		}
	})

	t.Run("hold disabled", func(t *testing.T) {
		for range 3 {
			res, _ := connect("nohold.cuttle.test")
			generatedHeaders(t, res, 502, `cuttle-test; error=connection_refused; next-hop="nohold"; details="connect failed: connection refused"`)
		}
		res, _ := get("nohold.cuttle.test")
		generatedHeaders(t, res, 502, `cuttle-test; error=connection_refused; next-hop="nohold"; details="connect failed: connection refused"`)
		if n := dialsTo(e.net, "10.8.0.6:8083"); n != 4 {
			t.Errorf("dials %d", n)
		}
		if st := state("nohold"); st.State != StateUp || st.LastError != "connection_refused" {
			t.Errorf("state %+v", st)
		}
	})
	onlyDials(t, e.net, "10.8.0.6:8081", "10.8.0.6:8082", "10.8.0.6:8083", "10.8.0.6:8084", "10.8.0.6:8085", "10.8.0.6:8086")
}

// rawStatus sends one request on a new connection to addr and returns the
// response status, or 0 on an error. It is safe outside the test
// goroutine.
func rawStatus(addr, req string) int {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return 0
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, req)
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// TestChainProxyStatus checks the member copy of design 0003 D6 without
// sockets: members keep their order, at most 1 KiB of field value is
// copied in whole members, unprintable bytes are dropped, and cuttle's own
// member comes last.
func TestChainProxyStatus(t *testing.T) {
	own := `cuttle-edge; received-status=502; next-hop="gateway"`
	if got := chainProxyStatus(nil, own); got != own {
		t.Errorf("no members: %s", got)
	}
	got := chainProxyStatus([]string{`egress; error=x; details="a, b"`, " cuttle-gateway; received-status=403\x01 "}, own)
	if want := `egress; error=x; details="a, b", cuttle-gateway; received-status=403, ` + own; got != want {
		t.Errorf("got %s", got)
	}
	// A member that ends inside an open quoted string is dropped, whatever
	// precedes it is kept, and cuttle's member stays a member of its own.
	for _, v := range []string{`egress; error=x; details="open, more`, `egress; details="esc\`, `egress; details="a\"b`} {
		if got := chainProxyStatus([]string{`first; error=y`, v}, own); got != `first; error=y, `+own {
			t.Errorf("unterminated %q: %s", v, got)
		}
	}
	long := strings.Repeat("m", 600)
	got = chainProxyStatus([]string{long + ", " + long + ", short"}, own)
	if got != long+", "+own {
		t.Errorf("1 KiB limit: %d bytes", len(got))
	}
}

// TestViaNames checks loop detection's Via parsing (design 0003 D2).
func TestViaNames(t *testing.T) {
	for _, c := range []struct {
		via  []string
		want bool
	}{
		{nil, false},
		{[]string{"1.1 cuttle-test (cuttle)"}, true},
		{[]string{"1.1 lab-gw, 1.1 CUTTLE-TEST"}, true},
		{[]string{"1.1 lab-gw (a, cuttle-test b), HTTP/1.1 other"}, false},
		{[]string{"1.1 lab-gw", "1.0 cuttle-test:3128"}, false},
		{[]string{"1.1 lab-gw", "1.1 cuttle-test (cuttle)"}, true}, // not the first Via line
		{[]string{"1.1 cuttle-testing (cuttle)"}, false},
	} {
		h := http.Header{"Via": c.via}
		if got := viaNames(h, "cuttle-test"); got != c.want {
			t.Errorf("%q: %v", c.via, got)
		}
	}
}

// TestConnectHead checks the CONNECT head writer without sockets (design
// 0003 D3): the target as sent, Host, sorted client fields minus hop-by-hop
// fields, and the disclosure rule of each mode.
func TestConnectHead(t *testing.T) {
	r := &http.Request{Method: "CONNECT", RequestURI: "Example.COM:443", Header: http.Header{
		"User-Agent":       {"ua"},
		"Connection":       {"X-Hop"},
		"X-Hop":            {"1"},
		"Proxy-Connection": {"keep-alive"},
		"Via":              {"1.1 lab-gw"},
		"X-Real-Ip":        {"198.51.100.7"},
	}}
	via := string(connectHead(r, "cuttle-edge", "via"))
	if want := "CONNECT Example.COM:443 HTTP/1.1\r\nHost: Example.COM:443\r\nUser-Agent: ua\r\nVia: 1.1 lab-gw\r\nVia: 1.1 cuttle-edge (cuttle)\r\nX-Real-Ip: 198.51.100.7\r\n\r\n"; via != want {
		t.Errorf("via:\n%q\nwant\n%q", via, want)
	}
	none := string(connectHead(r, "cuttle-edge", "none"))
	if want := "CONNECT Example.COM:443 HTTP/1.1\r\nHost: Example.COM:443\r\nUser-Agent: ua\r\n\r\n"; none != want {
		t.Errorf("none:\n%q", none)
	}
	if r.Header.Get("X-Hop") != "1" {
		t.Error("connectHead changed the request")
	}
}
