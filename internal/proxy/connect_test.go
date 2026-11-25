package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"squid/internal/stats"
	"squid/internal/testenv"
)

// tlsGet runs one HTTPS GET over an established tunnel.
func tlsGet(t *testing.T, c net.Conn, e *env, serverName, path string) (*http.Response, []byte) {
	t.Helper()
	tc := tls.Client(c, &tls.Config{ServerName: serverName, RootCAs: e.ca.Pool()})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS through the tunnel: %v", err)
	}
	io.WriteString(tc, "GET "+path+" HTTP/1.1\r\nHost: "+serverName+"\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, body
}

// TestConnectDirect checks C1: CONNECT on a direct rule reaches the HTTPS
// origin, and Go and curl verify the test-CA certificate.
func TestConnectDirect(t *testing.T) {
	e := newEnv(t, baseConfig)

	client, tr := e.goClient()
	res, err := client.Get("https://origin.cuttle.test/echo")
	if err != nil {
		t.Fatal(err)
	}
	var rec testenv.Request
	json.NewDecoder(res.Body).Decode(&rec)
	res.Body.Close()
	if res.StatusCode != 200 || res.TLS == nil || rec.URI != "/echo" || rec.Host != "origin.cuttle.test" {
		t.Fatalf("Go: status %d TLS %v echo %+v", res.StatusCode, res.TLS != nil, rec)
	}
	if got := res.TLS.PeerCertificates[0].Issuer.CommonName; got != "cuttle test origin root" {
		t.Errorf("issuer %q", got)
	}

	// A Go client that trusts another root fails verification.
	other := testenv.NewCA(t, "unrelated root")
	untrusting := &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: e.addr}),
		TLSClientConfig: &tls.Config{RootCAs: other.Pool()},
	}
	defer untrusting.CloseIdleConnections()
	if _, err := (&http.Client{Transport: untrusting, Timeout: 10 * time.Second}).Get("https://origin.cuttle.test/echo"); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Errorf("untrusted root: %v", err)
	}

	// curl with the test root succeeds; with another root it fails
	// verification (exit 60).
	ca := e.ca.WritePEM(t)
	r := testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "--cacert", ca,
		"-w", "%{http_connect} %{http_code}", "-o", "/dev/null", "https://origin.cuttle.test/echo")
	if r.Code != 0 || r.Stdout != "200 200" {
		t.Errorf("curl: exit %d out %q err %q", r.Code, r.Stdout, r.Stderr)
	}
	r = testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "--cacert", other.WritePEM(t),
		"-o", "/dev/null", "https://origin.cuttle.test/echo")
	if r.Code != 60 {
		t.Errorf("curl with another root: exit %d err %q", r.Code, r.Stderr)
	}

	tr.CloseIdleConnections()
	ds := e.waitDecisions(4)
	for _, d := range ds {
		checkDecision(t, d, map[string]string{"kind": "connect", "target": "origin.cuttle.test:443",
			"rule": "default", "action": "direct", "source": "local", "outcome": "ok", "status": "200"})
	}
}

// TestConnectExact200 checks C1: a raw client receives exactly the 200 head
// and nothing else before origin bytes.
func TestConnectExact200(t *testing.T) {
	e := newEnv(t, baseConfig)
	c := e.dial()
	io.WriteString(c, "CONNECT origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n")
	got := make([]byte, len(connectEstablished))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "HTTP/1.1 200 Connection established\r\n\r\n" {
		t.Fatalf("got %q", got)
	}
	// Nothing follows until the client speaks; the origin waits for the
	// ClientHello.
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var one [1]byte
	if n, err := c.Read(one[:]); n != 0 || !errors.Is(err, errTimeout(err)) {
		t.Fatalf("extra bytes after the 200: %d %v", n, err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	res, body := tlsGet(t, c, e, "origin.cuttle.test", "/echo")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"uri":"/echo"`) {
		t.Fatalf("through the tunnel: %d %s", res.StatusCode, body)
	}
}

// errTimeout returns err when it is a timeout, so errors.Is matches only
// timeouts.
func errTimeout(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return err
	}
	return errors.New("not a timeout")
}

// pipelineConn sends a CONNECT head together with the first bytes the TLS
// client writes, and strips the exact 200 before the first read.
type pipelineConn struct {
	net.Conn
	t         *testing.T
	head      string
	wroteHead bool
	readHead  bool
}

func (c *pipelineConn) Write(b []byte) (int, error) {
	if !c.wroteHead {
		c.wroteHead = true
		if _, err := c.Conn.Write(append([]byte(c.head), b...)); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *pipelineConn) Read(b []byte) (int, error) {
	if !c.readHead {
		c.readHead = true
		got := make([]byte, len(connectEstablished))
		if _, err := io.ReadFull(c.Conn, got); err != nil || string(got) != connectEstablished {
			return 0, errors.New("pipelined CONNECT: got " + string(got))
		}
	}
	return c.Conn.Read(b)
}

// TestConnectPipelinedBytes checks C2: bytes sent right after the CONNECT
// head, before the 200, reach the origin.
func TestConnectPipelinedBytes(t *testing.T) {
	e := newEnv(t, baseConfig)

	t.Run("plain request", func(t *testing.T) {
		c := e.dial()
		io.WriteString(c, "CONNECT plain.cuttle.test:80 HTTP/1.1\r\nHost: plain.cuttle.test:80\r\n\r\n"+
			"GET /echo HTTP/1.1\r\nHost: plain.cuttle.test\r\nX-Pipelined: yes\r\n\r\n")
		got := make([]byte, len(connectEstablished))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != connectEstablished {
			t.Fatalf("got %q %v", got, err)
		}
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 || !strings.Contains(string(body), `"X-Pipelined":["yes"]`) {
			t.Fatalf("status %d body %s", res.StatusCode, body)
		}
	})

	t.Run("TLS ClientHello", func(t *testing.T) {
		pc := &pipelineConn{Conn: e.dial(), t: t, head: "CONNECT origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n"}
		res, body := tlsGet(t, pc, e, "origin.cuttle.test", "/echo")
		if res.StatusCode != 200 || !strings.Contains(string(body), `"uri":"/echo"`) {
			t.Fatalf("status %d body %s", res.StatusCode, body)
		}
	})
}

// TestConnectBracketlessIPv6Rejected checks the T1 part of C7: Go's server
// rejects CFNetwork's bracketless IPv6 CONNECT with its own 400 before any
// handler runs, so there is no Proxy-Status, no decision, and no lookup.
func TestConnectBracketlessIPv6Rejected(t *testing.T) {
	e := newEnv(t, baseConfig)
	res, _ := e.raw("CONNECT 2001:db8::1:443 HTTP/1.1\r\nHost: 2001:db8::1:443\r\n\r\n")
	if res.StatusCode != 400 || res.Header.Get("Proxy-Status") != "" {
		t.Fatalf("status %d Proxy-Status %q", res.StatusCode, res.Header.Get("Proxy-Status"))
	}
	e.noDecisionFor(200 * time.Millisecond)
	if ev := e.net.Events(); len(ev) != 0 {
		t.Fatalf("network events %v", ev)
	}
	// The bracketed form is cuttle's to route.
	e.net.Route("[2001:db8::1]:443", e.https.Addr)
	c := e.connectRaw("[2001:db8::1]:443")
	c.Close()
	checkDecision(t, e.waitDecisions(1)[0], map[string]string{"target": "[2001:db8::1]:443", "action": "direct"})
}

// TestLocalAnyPort checks C6: a local client reaches loopback services and a
// public test address on ports other than 80 and 443; local clients face no
// port policy.
func TestLocalAnyPort(t *testing.T) {
	e := newEnv(t, baseConfig)
	e.net.Route("127.0.0.1:3000", e.http.Addr)
	e.net.Route("127.0.0.1:8443", e.https.Addr)
	e.net.Route("203.0.113.20:8443", e.https.Addr)
	e.net.Route("203.0.113.20:22", e.http.Addr)

	client, tr := e.goClient()
	res, err := client.Get("http://localhost:3000/echo")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("localhost:3000: %d", res.StatusCode)
	}
	res, err = client.Get("https://127.0.0.1:8443/echo")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("127.0.0.1:8443: %d", res.StatusCode)
	}
	tr.CloseIdleConnections()
	c := e.connectRaw("203.0.113.20:8443")
	if res, _ := tlsGet(t, c, e, "origin.cuttle.test", "/echo"); res.StatusCode != 200 {
		t.Fatalf("203.0.113.20:8443: %d", res.StatusCode)
	}
	c.Close()
	c = e.connectRaw("203.0.113.20:22")
	c.Close()

	ds := e.waitDecisions(4)
	for _, w := range []struct{ kind, target, rule string }{
		{"http", "localhost:3000", "loopback"},
		{"connect", "127.0.0.1:8443", "loopback"},
		{"connect", "203.0.113.20:8443", "default"},
		{"connect", "203.0.113.20:22", "default"},
	} {
		checkDecision(t, byTarget(t, ds, w.kind, w.target), map[string]string{"rule": w.rule, "action": "direct", "status": "200", "outcome": "ok"})
	}
	wantDials := []netip.AddrPort{
		netip.MustParseAddrPort("127.0.0.1:3000"),
		netip.MustParseAddrPort("127.0.0.1:8443"),
		netip.MustParseAddrPort("203.0.113.20:8443"),
		netip.MustParseAddrPort("203.0.113.20:22"),
	}
	if got := e.net.Dials(); !slices.Equal(got, wantDials) {
		t.Errorf("dials %v, want %v", got, wantDials)
	}
}

// TestDirectAddressPolicy checks 0003 N4.
func TestDirectAddressPolicy(t *testing.T) {
	cfg := baseConfig + `
[[rule]]
name = "lan"
domains = ["lan.cuttle.test"]
action = "direct"
allow_private = true

[[rule]]
name = "link-local"
cidrs = ["169.254.0.0/16"]
action = "direct"
allow_loopback = true
allow_private = true
`
	e := newEnv(t, cfg)
	n := e.net
	n.Map("lo.cuttle.test", "127.0.0.1")
	n.Map("self.cuttle.test", "192.0.2.50")
	n.Map("ll.cuttle.test", "169.254.1.1")
	n.Map("ll6.cuttle.test", "fe80::1")
	n.Map("zero.cuttle.test", "0.0.0.0")
	n.Map("nas.cuttle.test", "10.20.0.5")
	n.Map("nas.lan.cuttle.test", "10.20.0.5")
	n.Route("10.20.0.5:443", e.https.Addr)
	n.Route("192.0.2.50:443", e.https.Addr)
	n.Route("127.0.0.1:443", e.https.Addr)
	n.Route("127.0.0.1:3000", e.http.Addr)
	n.Route("127.0.0.1:8443", e.https.Addr)

	for _, name := range []string{"lo", "self", "ll", "ll6", "zero", "nas"} {
		n.Reset()
		target := name + ".cuttle.test:443"
		res, body := e.raw("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")
		if res.StatusCode != 502 || !strings.HasPrefix(res.Header.Get("Proxy-Status"), "cuttle-test; error=destination_ip_prohibited") {
			t.Errorf("%s: %d %q %s", target, res.StatusCode, res.Header.Get("Proxy-Status"), body)
		}
		if d := n.Dials(); len(d) != 0 {
			t.Errorf("%s: dials %v", target, d)
		}
		if l := n.Lookups(); len(l) != 1 {
			t.Errorf("%s: lookups %v", target, l)
		}
	}
	ds := e.waitDecisions(6)
	for _, d := range ds {
		checkDecision(t, d, map[string]string{"outcome": "prohibited", "status": "502", "error": "destination_ip_prohibited", "up": "0", "down": "0"})
	}
	// Unusable literals are refused even through the link-local rule, which
	// has both flags.
	n.Reset()
	if res, _ := e.raw("CONNECT 169.254.1.1:443 HTTP/1.1\r\nHost: x\r\n\r\n"); res.StatusCode != 502 {
		t.Errorf("link-local literal: %d", res.StatusCode)
	}
	if d := n.Dials(); len(d) != 0 {
		t.Errorf("link-local literal dialed %v", d)
	}

	// Allowed: a private name through allow_private, private and self
	// literals without any flag, and loopback through the loopback rule.
	n.Reset()
	for _, target := range []string{"nas.lan.cuttle.test:443", "10.20.0.5:443", "192.0.2.50:443", "127.0.0.1:443"} {
		c := e.connectRaw(target)
		if res, _ := tlsGet(t, c, e, "origin.cuttle.test", "/echo"); res.StatusCode != 200 {
			t.Errorf("%s: %d", target, res.StatusCode)
		}
		c.Close()
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("10.20.0.5:443"),
		netip.MustParseAddrPort("10.20.0.5:443"),
		netip.MustParseAddrPort("192.0.2.50:443"),
		netip.MustParseAddrPort("127.0.0.1:443"),
	}
	if got := n.Dials(); !slices.Equal(got, want) {
		t.Errorf("dials %v, want %v", got, want)
	}

	// The starter loopback rule: localhost:3000 and 127.0.0.1:8443.
	client, tr := e.goClient()
	for _, u := range []string{"http://localhost:3000/echo", "https://127.0.0.1:8443/echo"} {
		res, err := client.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("%s: %d", u, res.StatusCode)
		}
	}
	tr.CloseIdleConnections()
	all := e.waitDecisions(6 + 1 + 4 + 2)
	checkDecision(t, byTarget(t, all, "http", "localhost:3000"), map[string]string{"rule": "loopback", "outcome": "ok", "status": "200"})
	checkDecision(t, byTarget(t, all, "connect", "127.0.0.1:8443"), map[string]string{"rule": "loopback", "outcome": "ok", "status": "200"})
	checkDecision(t, byTarget(t, all, "connect", "nas.lan.cuttle.test:443"), map[string]string{"rule": "lan", "outcome": "ok"})
	checkDecision(t, byTarget(t, all, "connect", "nas.cuttle.test:443"), map[string]string{"rule": "default", "outcome": "prohibited"})
}

// TestUpstreamRoutesAccepted checks that New, which refused upstream routes
// before M1.3, accepts them for local and remote clients alike, together
// with non-loopback listeners.
func TestUpstreamRoutesAccepted(t *testing.T) {
	for _, c := range []struct{ name, cfg string }{
		{"upstream remote default", `name = "cuttle-test"
[proxy]
listen = ["127.0.0.1:18080", "10.8.0.6:18080"]
default = "direct"
[proxy.remote]
sources = ["10.8.0.1/32"]
default = "upstream:gw"
[[upstream]]
name = "gw"
address = "10.8.0.6:3128"
disclosure = "via"
`},
		{"upstream default", `name = "cuttle-test"
[proxy]
default = "upstream:gw"
[[upstream]]
name = "gw"
address = "10.8.0.6:3128"
disclosure = "via"
`},
		{"upstream rule", `name = "cuttle-test"
[proxy]
default = "direct"
[[upstream]]
name = "gw"
address = "proxy.internal.example:8080"
disclosure = "none"
[[rule]]
domains = ["internal.example"]
action = "upstream:gw"
`},
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p, err := New(mustParse(t, c.cfg), Deps{Network: testenv.NewNet(), SelfAddrs: testenv.SelfAddrs, Listeners: []net.Listener{ln}})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if st := p.Upstreams(); len(st) != 1 || st[0].Name != "gw" || st[0].State != StateUp {
			t.Errorf("%s: upstream states %+v", c.name, st)
		}
		ln.Close()
	}
}

// TestWebSocket checks C9: a WebSocket echo round trip over CONNECT to port
// 80, over CONNECT to port 443 (wss), and over an absolute-form upgrade on a
// direct route.
func TestWebSocket(t *testing.T) {
	e := newEnv(t, baseConfig)
	echo := func(t *testing.T, ws *websocket.Conn) {
		t.Helper()
		msg := "hello " + t.Name()
		if err := websocket.Message.Send(ws, msg); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := websocket.Message.Receive(ws, &got); err != nil {
			t.Fatal(err)
		}
		if got != msg {
			t.Fatalf("echo %q", got)
		}
	}
	origin, _ := url.Parse("http://ws.cuttle.test/")

	t.Run("CONNECT port 80", func(t *testing.T) {
		c := e.connectRaw("ws.cuttle.test:80")
		loc, _ := url.Parse("ws://ws.cuttle.test/ws")
		ws, err := websocket.NewClient(&websocket.Config{Location: loc, Origin: origin, Version: websocket.ProtocolVersionHybi13}, c)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		echo(t, ws)
	})

	t.Run("CONNECT port 443", func(t *testing.T) {
		c := e.connectRaw("ws.cuttle.test:443")
		tc := tls.Client(c, &tls.Config{ServerName: "ws.cuttle.test", RootCAs: e.ca.Pool()})
		loc, _ := url.Parse("wss://ws.cuttle.test/ws")
		ws, err := websocket.NewClient(&websocket.Config{Location: loc, Origin: origin, Version: websocket.ProtocolVersionHybi13}, tc)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		echo(t, ws)
	})

	t.Run("absolute-form upgrade", func(t *testing.T) {
		before := len(e.http.Requests())
		c := e.dial()
		// RequestURI() of this URL is the absolute form http://ws.cuttle.test/ws.
		loc := &url.URL{Scheme: "http", Opaque: "//ws.cuttle.test/ws", Host: "ws.cuttle.test"}
		ws, err := websocket.NewClient(&websocket.Config{Location: loc, Origin: origin, Version: websocket.ProtocolVersionHybi13}, c)
		if err != nil {
			t.Fatal(err)
		}
		echo(t, ws)
		ws.Close()
		reqs := e.http.Requests()[before:]
		if len(reqs) == 0 || reqs[0].URI != "/ws" || reqs[0].Header.Get("Upgrade") != "websocket" {
			t.Fatalf("origin saw %+v", reqs)
		}
		raw := string(e.http.Raw(reqs[0].Conn))
		if !strings.HasPrefix(raw, "GET /ws HTTP/1.1\r\n") {
			t.Errorf("origin request line %q", firstLine(raw))
		}
	})

	ds := e.waitDecisions(3)
	checkDecision(t, byTarget(t, ds, "connect", "ws.cuttle.test:80"), map[string]string{"outcome": "ok", "status": "200"})
	checkDecision(t, byTarget(t, ds, "connect", "ws.cuttle.test:443"), map[string]string{"outcome": "ok", "status": "200"})
	checkDecision(t, byTarget(t, ds, "http", "ws.cuttle.test:80"), map[string]string{"status": "101", "outcome": "ok"})
}

// TestTunnelIdleTimeout checks the relay's idle limit (design 0002 D5): a
// tunnel with no bytes in either direction for proxy.idle_timeout closes,
// and an active one stays open.
func TestTunnelIdleTimeout(t *testing.T) {
	cfg := strings.Replace(baseConfig, `default = "direct"`, "default = \"direct\"\nidle_timeout = \"300ms\"", 1)
	e := newEnv(t, cfg)

	active := e.connectRaw("plain.cuttle.test:80")
	br := bufio.NewReader(active)
	for range 4 {
		io.WriteString(active, "GET /echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("active tunnel closed early: %v", err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		time.Sleep(150 * time.Millisecond)
	}

	idle := e.connectRaw("plain.cuttle.test:80")
	start := time.Now()
	idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := idle.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("idle tunnel read %d %v", n, err)
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 2*time.Second {
		t.Errorf("idle tunnel closed after %v", el)
	}
	active.Close()
	ds := e.waitDecisions(2)
	for _, d := range ds {
		checkDecision(t, d, map[string]string{"outcome": "ok", "status": "200"})
	}
}

// TestShutdownClosesTunnels checks that Shutdown closes an open tunnel and
// logs it with outcome shutdown (design 0006 D6).
func TestShutdownClosesTunnels(t *testing.T) {
	e := newEnv(t, baseConfig)
	c := e.connectRaw("plain.cuttle.test:80")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.p.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("tunnel still open: %d %v", n, err)
	}
	checkDecision(t, e.waitDecisions(1)[0], map[string]string{"outcome": "shutdown", "status": "200"})
	if got := e.p.Live().Get(stats.TunnelsActive); got != 0 {
		t.Errorf("tunnels_active %d", got)
	}
}

// TestDirectFailures checks the design 0002 D6 mapping of direct-route
// failures for CONNECT and plain HTTP.
func TestDirectFailures(t *testing.T) {
	cfg := strings.Replace(baseConfig, `default = "direct"`,
		"default = \"direct\"\nconnect_timeout = \"300ms\"\nresponse_timeout = \"300ms\"", 1)
	e := newEnv(t, cfg)
	e.net.Map("refused.cuttle.test", "203.0.113.40")
	e.net.Map("hole.cuttle.test", "203.0.113.41")
	e.net.Blackhole("203.0.113.41")
	e.net.Map("bad.cuttle.test", "203.0.113.42")
	e.net.Route("203.0.113.42:80", newBadOrigin(t))
	e.net.Map("lo.cuttle.test", "127.0.0.1")

	for _, c := range []struct {
		req, status, errType, outcome string
	}{
		{"CONNECT refused.cuttle.test:443 HTTP/1.1\r\nHost: x\r\n\r\n", "502", "connection_refused", "refused"},
		{"CONNECT hole.cuttle.test:443 HTTP/1.1\r\nHost: x\r\n\r\n", "504", "connection_timeout", "timeout"},
		{"CONNECT nowhere.cuttle.test:443 HTTP/1.1\r\nHost: x\r\n\r\n", "502", "dns_error", "dns_error"},
		{"GET http://refused.cuttle.test/ HTTP/1.1\r\nHost: refused.cuttle.test\r\n\r\n", "502", "connection_refused", "refused"},
		{"GET http://hole.cuttle.test/ HTTP/1.1\r\nHost: hole.cuttle.test\r\n\r\n", "504", "connection_timeout", "timeout"},
		{"GET http://nowhere.cuttle.test/ HTTP/1.1\r\nHost: nowhere.cuttle.test\r\n\r\n", "502", "dns_error", "dns_error"},
		{"GET http://plain.cuttle.test/slow?ms=3000 HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n", "504", "http_response_timeout", "timeout"},
		{"GET http://bad.cuttle.test/eof HTTP/1.1\r\nHost: bad.cuttle.test\r\n\r\n", "502", "connection_refused", "refused"},
		{"GET http://bad.cuttle.test/reset HTTP/1.1\r\nHost: bad.cuttle.test\r\n\r\n", "502", "connection_refused", "refused"},
		{"GET http://bad.cuttle.test/garbage HTTP/1.1\r\nHost: bad.cuttle.test\r\n\r\n", "502", "http_protocol_error", "upstream_protocol"},
		{"GET http://lo.cuttle.test/ HTTP/1.1\r\nHost: lo.cuttle.test\r\n\r\n", "502", "destination_ip_prohibited", "prohibited"},
	} {
		start := time.Now()
		res, _ := e.raw(c.req)
		if strconv.Itoa(res.StatusCode) != c.status || !strings.HasPrefix(res.Header.Get("Proxy-Status"), "cuttle-test; error="+c.errType) {
			t.Errorf("%s: %d %q", firstLine(c.req), res.StatusCode, res.Header.Get("Proxy-Status"))
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("%s: took %v", firstLine(c.req), el)
		}
	}
	ds := e.waitDecisions(11)
	for i, want := range []string{"refused", "timeout", "dns_error", "refused", "timeout", "dns_error", "timeout",
		"refused", "refused", "upstream_protocol", "prohibited"} {
		checkDecision(t, ds[i], map[string]string{"outcome": want, "up": "0", "down": "0"})
	}
	// The prohibited name was looked up but never dialed.
	for _, d := range e.net.Dials() {
		if d.Addr() == netip.MustParseAddr("127.0.0.1") {
			t.Errorf("dialed %s", d)
		}
	}

	// A body that fails after the head: the client keeps the status and the
	// bytes the socket accepted, and the decision logs exactly those bytes
	// with outcome upstream_protocol (design 0002 Failure behavior).
	c := e.dial()
	io.WriteString(c, "GET http://bad.cuttle.test/midbody HTTP/1.1\r\nHost: bad.cuttle.test\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != "0123456789" || err == nil {
		t.Errorf("mid-body failure: %d %q %v", res.StatusCode, body, err)
	}
	d := e.waitDecisions(12)[11]
	checkDecision(t, d, map[string]string{"target": "bad.cuttle.test:80", "outcome": "upstream_protocol", "status": "200",
		"down": strconv.Itoa(len(body))})
}
