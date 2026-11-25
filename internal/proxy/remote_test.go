package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"squid/internal/stats"
	"squid/internal/testenv"
)

// remoteConfig serves remote clients from 10.8.0.1 with the default remote
// settings: ports 80 and 443, default block, and no direct routes.
const remoteConfig = baseConfig + `
[[rule]]
name = "open"
hosts = ["origin.cuttle.test", "plain.cuttle.test"]
action = "direct"

[proxy.remote]
sources = ["10.8.0.1/32"]
`

// remoteDirectConfig lets remote clients from 10.8.0.1 use direct routes on
// ports 443 and 8080, through rules that also carry allow_private and
// allow_loopback, which apply to local clients only.
const remoteDirectConfig = baseConfig + `
[[rule]]
name = "open"
hosts = ["origin.cuttle.test", "plain.cuttle.test"]
action = "direct"

[[rule]]
name = "lan"
domains = ["lan.cuttle.test"]
cidrs = ["10.20.0.0/16"]
action = "direct"
allow_private = true

[[rule]]
name = "own"
hosts = ["self.cuttle.test", "lo.cuttle.test"]
cidrs = ["192.0.2.50/32"]
action = "direct"
allow_loopback = true

[proxy.remote]
sources = ["10.8.0.1/32"]
ports = [443, 8080]
allow_direct = true
`

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ioCountListener counts the reads and writes the proxy makes on the
// connections it accepts.
type ioCountListener struct {
	net.Listener
	accepted, reads, writes atomic.Int64
}

func (l *ioCountListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	return &ioCountConn{Conn: c, l: l}, nil
}

type ioCountConn struct {
	net.Conn
	l *ioCountListener
}

func (c *ioCountConn) Read(b []byte) (int, error) {
	c.l.reads.Add(1)
	return c.Conn.Read(b)
}

func (c *ioCountConn) Write(b []byte) (int, error) {
	c.l.writes.Add(1)
	return c.Conn.Write(b)
}

// expectClosed sends a request head on c and checks that the proxy closes
// the connection without writing a byte.
func expectClosed(t *testing.T, c net.Conn) {
	t.Helper()
	io.WriteString(c, "CONNECT origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c.Read(make([]byte, 64))
	var ne net.Error
	if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("connection not closed at once: read %d bytes, %v", n, err)
	}
}

// TestRemoteSourceRejected checks 0003 N2: a connection from a source
// outside the allowlist is closed with no byte read or written, increments
// connections_rejected_total{reason="source"}, logs the source address once
// per minute, and produces no decision, while an allowlisted source is
// served as a remote client. Without proxy.remote.sources, every
// non-loopback source is rejected.
func TestRemoteSourceRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	type instance struct {
		p        *Proxy
		logs     *logBuffer
		net      *testenv.Net
		addr     map[string]string
		counters map[string]*ioCountListener
	}
	start := func(cfg string, peers ...string) *instance {
		in := &instance{logs: &logBuffer{}, net: testenv.NewNet(), addr: map[string]string{}, counters: map[string]*ioCountListener{}}
		var listeners []net.Listener
		for _, peer := range peers {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			cl := &ioCountListener{Listener: ln}
			listeners = append(listeners, testenv.SourceListener(cl, netip.MustParseAddr(peer)))
			in.addr[peer], in.counters[peer] = ln.Addr().String(), cl
		}
		p, err := New(mustParse(t, cfg), Deps{
			Network:   in.net,
			SelfAddrs: testenv.SelfAddrs,
			Listeners: listeners,
			Logger:    newTestLogger(in.logs),
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
		in.p = p
		return in
	}
	dial := func(addr string) net.Conn {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	rejected := func(in *instance) int64 {
		return in.p.Live().Get(stats.ConnectionsRejectedTotal, "reason", "source")
	}

	t.Run("outside the allowlist", func(t *testing.T) {
		in := start(remoteConfig, "10.20.0.9", "10.8.0.1")
		for i := range 2 {
			expectClosed(t, dial(in.addr["10.20.0.9"]))
			waitFor(t, "rejection", func() bool { return rejected(in) == int64(i+1) })
		}
		cl := in.counters["10.20.0.9"]
		if cl.accepted.Load() != 2 || cl.reads.Load() != 0 || cl.writes.Load() != 0 {
			t.Errorf("rejected connections: accepted %d, reads %d, writes %d", cl.accepted.Load(), cl.reads.Load(), cl.writes.Load())
		}
		// The warning follows the counter, so wait for it.
		waitForLog(t, in.logs, `msg="rejected source" source=10.20.0.9 reason="not in proxy.remote.sources"`)
		if got := strings.Count(in.logs.String(), `msg="rejected source" source=10.20.0.9 `); got != 1 {
			t.Errorf("%d warnings for the source, want 1:\n%s", got, in.logs.String())
		}
		if len(in.logs.decisions()) != 0 || in.p.Live().Get(stats.ConnectionsTotal) != 0 || len(in.net.Events()) != 0 {
			t.Fatalf("a rejected source was served: %s %v", in.logs.String(), in.net.Events())
		}

		// The allowlisted source is accepted and served as a remote client.
		c := dial(in.addr["10.8.0.1"])
		io.WriteString(c, "CONNECT blocked.cuttle.test:443 HTTP/1.1\r\nHost: blocked.cuttle.test:443\r\n\r\n")
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if b, _ := io.ReadAll(c); !strings.HasPrefix(string(b), "HTTP/1.1 403 ") {
			t.Errorf("allowlisted source got %q", b)
		}
		ds := waitDecisionLines(t, in.logs, 1)
		checkDecision(t, ds[0], map[string]string{"source": "remote", "rule": "blocked", "outcome": "blocked"})
		if !strings.HasPrefix(ds[0]["client"], "10.8.0.1:") {
			t.Errorf("client %q", ds[0]["client"])
		}
		if in.p.Live().Get(stats.ConnectionsTotal) != 1 || rejected(in) != 2 {
			t.Errorf("connections_total %d, rejected %d", in.p.Live().Get(stats.ConnectionsTotal), rejected(in))
		}
	})

	t.Run("no allowlist", func(t *testing.T) {
		in := start(baseConfig, "10.8.0.1")
		expectClosed(t, dial(in.addr["10.8.0.1"]))
		waitFor(t, "rejection", func() bool { return rejected(in) == 1 })
		cl := in.counters["10.8.0.1"]
		if cl.reads.Load() != 0 || cl.writes.Load() != 0 {
			t.Errorf("reads %d, writes %d", cl.reads.Load(), cl.writes.Load())
		}
		waitForLog(t, in.logs, `msg="rejected source" source=10.8.0.1 reason="not in proxy.remote.sources"`)
		if len(in.logs.decisions()) != 0 || in.p.Live().Get(stats.ConnectionsTotal) != 0 {
			t.Errorf("decision or accepted connection for a rejected source")
		}
	})
}

// waitForLog waits until logs contain want.
func waitForLog(t *testing.T, logs *logBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", want, logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitDecisionLines waits for at least n decision lines in logs.
func waitDecisionLines(t *testing.T, logs *logBuffer, n int) []map[string]string {
	t.Helper()
	waitFor(t, "decision lines", func() bool { return len(logs.decisions()) >= n })
	return logs.decisions()
}

// remoteBlocked checks a 403 that cuttle generated for a remote client and
// its decision line.
func remoteBlocked(t *testing.T, e *env, kind, target, rule string, res *http.Response) {
	t.Helper()
	generatedHeaders(t, res, http.StatusForbidden, `cuttle-test; error=http_request_denied; details="blocked by rule `+rule+`"`)
	d := byTarget(t, e.logs.decisions(), kind, target)
	checkDecision(t, d, map[string]string{"source": "remote", "rule": rule, "action": "block", "outcome": "blocked",
		"status": "403", "up": "0", "down": "0"})
	if !strings.HasPrefix(d["client"], "10.8.0.1:") {
		t.Errorf("%s: client %q", target, d["client"])
	}
}

// TestRemotePolicy checks 0003 N3 as far as M1.2 can show it: a remote
// client gets 403 with rule remote-default for unmatched names, with rule
// remote-direct for a direct rule, with rule port for port 22 or a port
// outside proxy.remote.ports, and a block rule's own 403, all without a
// lookup or dial. With allow_direct it reaches public test addresses on its
// ports and gets 502 destination_ip_prohibited for private, loopback, and
// self addresses, by name and as literals, even through rules with
// allow_private or allow_loopback, without a dial. Local clients reach the
// same targets through those rules. Remote upstream routes are checked by
// the chain tests of M1.3.
func TestRemotePolicy(t *testing.T) {
	t.Run("without allow_direct", func(t *testing.T) {
		e := newEnvWith(t, remoteConfig, envOptions{remote: []string{"10.8.0.1"}})
		cases := []struct{ kind, target, req, rule string }{
			{"connect", "unmatched.cuttle.test:443", "CONNECT unmatched.cuttle.test:443 HTTP/1.1\r\nHost: unmatched.cuttle.test:443\r\n\r\n", "remote-default"},
			{"connect", "origin.cuttle.test:443", "CONNECT origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n", "remote-direct"},
			{"http", "plain.cuttle.test:80", "GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n", "remote-direct"},
			{"connect", "127.0.0.1:443", "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n", "remote-direct"},
			{"connect", "origin.cuttle.test:22", "CONNECT origin.cuttle.test:22 HTTP/1.1\r\nHost: origin.cuttle.test:22\r\n\r\n", "port"},
			{"connect", "blocked.cuttle.test:443", "CONNECT blocked.cuttle.test:443 HTTP/1.1\r\nHost: blocked.cuttle.test:443\r\n\r\n", "blocked"},
		}
		for i, c := range cases {
			res, _ := e.rawFrom("10.8.0.1", c.req)
			e.waitDecisions(i + 1)
			remoteBlocked(t, e, c.kind, c.target, c.rule, res)
		}
		if ev := e.net.Events(); len(ev) != 0 {
			t.Errorf("network events for remote policy refusals: %v", ev)
		}
		if got := e.bytesTotal(); got != 0 {
			t.Errorf("bytes_total %d", got)
		}

		// A local client takes the direct rule that the remote client may not.
		c := e.connectRaw("origin.cuttle.test:443")
		if res, _ := tlsGet(t, c, e, "origin.cuttle.test", "/echo"); res.StatusCode != 200 {
			t.Errorf("local client: %d", res.StatusCode)
		}
		c.Close()
		ds := e.waitDecisions(len(cases) + 1)
		var local []map[string]string
		for _, d := range ds {
			if d["source"] == "local" {
				local = append(local, d)
			}
		}
		if len(local) != 1 {
			t.Fatalf("local decisions %v", local)
		}
		checkDecision(t, local[0], map[string]string{"target": "origin.cuttle.test:443", "rule": "open", "action": "direct", "outcome": "ok"})
	})

	t.Run("with allow_direct", func(t *testing.T) {
		e := newEnvWith(t, remoteDirectConfig, envOptions{remote: []string{"10.8.0.1"}})
		n := e.net
		n.Map("nas.lan.cuttle.test", "10.20.0.5")
		n.Map("lo.cuttle.test", "127.0.0.1")
		n.Map("self.cuttle.test", "192.0.2.50")
		// Every refused address routes to a live origin, so only the address
		// policy stops the dial.
		n.Route("203.0.113.10:8080", e.http.Addr)
		for _, a := range []string{"10.20.0.5", "127.0.0.1", "[::1]", "192.0.2.50"} {
			n.Route(a+":443", e.https.Addr)
			n.Route(a+":8080", e.http.Addr)
		}

		// Public test addresses on the remote ports: CONNECT and plain HTTP.
		c := e.connectRawFrom("10.8.0.1", "origin.cuttle.test:443")
		if res, _ := tlsGet(t, c, e, "origin.cuttle.test", "/echo"); res.StatusCode != 200 {
			t.Errorf("remote CONNECT: %d", res.StatusCode)
		}
		c.Close()
		client, tr := e.goClientFrom("10.8.0.1")
		res, err := client.Get("http://plain.cuttle.test:8080/echo")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("remote plain HTTP: %d", res.StatusCode)
		}
		tr.CloseIdleConnections()
		ds := e.waitDecisions(2)
		checkDecision(t, byTarget(t, ds, "connect", "origin.cuttle.test:443"), map[string]string{"source": "remote", "rule": "open", "action": "direct", "outcome": "ok", "status": "200"})
		checkDecision(t, byTarget(t, ds, "http", "plain.cuttle.test:8080"), map[string]string{"source": "remote", "rule": "open", "action": "direct", "outcome": "ok", "status": "200"})

		// proxy.remote.ports replaces the default ports: 80 is refused now.
		res, _ = e.rawFrom("10.8.0.1", "GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
		waitFor(t, "port decision", func() bool { return len(e.logs.decisions()) == 3 })
		remoteBlocked(t, e, "http", "plain.cuttle.test:80", "port", res)

		// Private, loopback, and self addresses, by name and as literals,
		// through rules with allow_private or allow_loopback.
		prohibited := []struct{ kind, target, req, rule string }{
			{"connect", "nas.lan.cuttle.test:443", "", "lan"},
			{"connect", "10.20.0.5:443", "", "lan"},
			{"connect", "lo.cuttle.test:443", "", "own"},
			{"connect", "127.0.0.1:443", "", "loopback"},
			{"connect", "localhost:443", "", "loopback"},
			{"connect", "[::1]:443", "", "loopback"},
			{"connect", "self.cuttle.test:443", "", "own"},
			{"connect", "192.0.2.50:443", "", "own"},
			{"http", "nas.lan.cuttle.test:8080", "GET http://nas.lan.cuttle.test:8080/echo HTTP/1.1\r\nHost: nas.lan.cuttle.test:8080\r\n\r\n", "lan"},
			{"http", "127.0.0.1:8080", "GET http://127.0.0.1:8080/echo HTTP/1.1\r\nHost: 127.0.0.1:8080\r\n\r\n", "loopback"},
		}
		n.Reset()
		for _, p := range prohibited {
			req := p.req
			if req == "" {
				req = "CONNECT " + p.target + " HTTP/1.1\r\nHost: " + p.target + "\r\n\r\n"
			}
			res, body := e.rawFrom("10.8.0.1", req)
			if res.StatusCode != 502 || !strings.HasPrefix(res.Header.Get("Proxy-Status"), "cuttle-test; error=destination_ip_prohibited") {
				t.Errorf("%s: %d %q %s", p.target, res.StatusCode, res.Header.Get("Proxy-Status"), body)
			}
		}
		if d := n.Dials(); len(d) != 0 {
			t.Errorf("remote client dialed %v", d)
		}
		ds = e.waitDecisions(3 + len(prohibited))
		for _, p := range prohibited {
			checkDecision(t, byTarget(t, ds, p.kind, p.target), map[string]string{"source": "remote", "rule": p.rule, "action": "direct",
				"outcome": "prohibited", "status": "502", "error": "destination_ip_prohibited", "up": "0", "down": "0"})
		}

		// A local client reaches the same targets through the same rules.
		n.Reset()
		for _, target := range []string{"nas.lan.cuttle.test:443", "lo.cuttle.test:443", "self.cuttle.test:443"} {
			c := e.connectRaw(target)
			if res, _ := tlsGet(t, c, e, "origin.cuttle.test", "/echo"); res.StatusCode != 200 {
				t.Errorf("local %s: %d", target, res.StatusCode)
			}
			c.Close()
		}
		want := []netip.AddrPort{
			netip.MustParseAddrPort("10.20.0.5:443"),
			netip.MustParseAddrPort("127.0.0.1:443"),
			netip.MustParseAddrPort("192.0.2.50:443"),
		}
		if got := n.Dials(); !slices.Equal(got, want) {
			t.Errorf("local dials %v, want %v", got, want)
		}
	})
}

// TestDirectPoolSeparation checks 0002 C15: a direct connection pooled for
// a local client is not reused for a remote client's request to the same
// host and port. The remote request makes its own dial to a public
// address, and for a private address, which only the local client's rule
// allows, it gets 502 destination_ip_prohibited instead of the local
// client's pooled connection. Within each class, pooling still works.
func TestDirectPoolSeparation(t *testing.T) {
	e := newEnvWith(t, remoteDirectConfig, envOptions{remote: []string{"10.8.0.1"}})
	n := e.net
	n.Map("nas.lan.cuttle.test", "10.20.0.5")
	n.Route("203.0.113.10:8080", e.http.Addr)
	n.Route("10.20.0.5:8080", e.http.Addr)
	local, localTr := e.goClient()
	remote, remoteTr := e.goClientFrom("10.8.0.1")
	get := func(c *http.Client, u string) int {
		t.Helper()
		res, err := c.Get(u)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	conns := func() []int {
		var out []int
		for _, r := range e.http.Requests() {
			out = append(out, r.Conn)
		}
		return out
	}

	const public = "http://plain.cuttle.test:8080/echo"
	// Two local requests share one pooled origin connection.
	for range 2 {
		if code := get(local, public); code != 200 {
			t.Fatalf("local: %d", code)
		}
	}
	if d := n.Dials(); len(d) != 1 || e.http.Connections() != 1 {
		t.Fatalf("local requests were not pooled: dials %v, origin connections %d", d, e.http.Connections())
	}
	// The remote request to the same host and port dials its own connection.
	if code := get(remote, public); code != 200 {
		t.Fatalf("remote: %d", code)
	}
	if d := n.Dials(); len(d) != 2 || e.http.Connections() != 2 {
		t.Fatalf("remote request reused the local connection: dials %v, origin connections %d", d, e.http.Connections())
	}
	// Each class keeps reusing its own connection.
	get(local, public)
	get(remote, public)
	if got, want := conns(), []int{1, 1, 2, 1, 2}; !slices.Equal(got, want) {
		t.Errorf("origin connections per request %v, want %v", got, want)
	}
	if d := n.Dials(); len(d) != 2 {
		t.Errorf("dials %v, want 2", d)
	}

	// A connection that only the local client's allow_private rule allowed
	// never carries a remote request.
	const private = "http://nas.lan.cuttle.test:8080/echo"
	if code := get(local, private); code != 200 {
		t.Fatalf("local private: %d", code)
	}
	before := len(e.http.Requests())
	n.Reset()
	if code := get(remote, private); code != http.StatusBadGateway {
		t.Errorf("remote private: %d, want 502", code)
	}
	if d := n.Dials(); len(d) != 0 || len(e.http.Requests()) != before {
		t.Errorf("remote private request reached the origin: dials %v, requests %d -> %d", d, before, len(e.http.Requests()))
	}
	localTr.CloseIdleConnections()
	remoteTr.CloseIdleConnections()
	ds := e.waitDecisions(7)
	var outcomes []string
	for _, d := range ds {
		outcomes = append(outcomes, d["source"]+" "+d["target"]+" "+d["outcome"])
	}
	want := []string{
		"local plain.cuttle.test:8080 ok", "local plain.cuttle.test:8080 ok", "remote plain.cuttle.test:8080 ok",
		"local plain.cuttle.test:8080 ok", "remote plain.cuttle.test:8080 ok",
		"local nas.lan.cuttle.test:8080 ok", "remote nas.lan.cuttle.test:8080 prohibited",
	}
	if !slices.Equal(outcomes, want) {
		t.Errorf("decisions %q, want %q", outcomes, want)
	}
}

// TestConnectionLimits checks 0003 N6: connections over
// proxy.max_connections or proxy.remote.max_connections_per_source are
// closed after accept without a byte written and counted with reason
// "limit". The caps count open connections: per remote source address,
// and in total for local and remote clients. Local clients have no
// per-source cap.
func TestConnectionLimits(t *testing.T) {
	cfg := strings.Replace(baseConfig, "default = \"direct\"\n", "default = \"direct\"\nmax_connections = 16\n", 1) + `
[proxy.remote]
sources = ["10.8.0.1/32", "10.8.0.6/32"]
max_connections_per_source = 2
`
	e := newEnvWith(t, cfg, envOptions{remote: []string{"10.8.0.1", "10.8.0.6"}})
	live := e.p.Live()
	limit := func() int64 { return live.Get(stats.ConnectionsRejectedTotal, "reason", "limit") }
	open := func(peer string) net.Conn {
		t.Helper()
		before := live.Get(stats.ConnectionsTotal)
		c := e.dialFrom(peer)
		waitFor(t, "accept from "+peer, func() bool { return live.Get(stats.ConnectionsTotal) == before+1 })
		return c
	}
	refused := func(peer string) {
		t.Helper()
		before := limit()
		expectClosed(t, e.dialFrom(peer))
		waitFor(t, "limit rejection from "+peer, func() bool { return limit() == before+1 })
	}

	a1 := open("10.8.0.1")
	open("10.8.0.1")
	refused("10.8.0.1") // per-source cap
	open("10.8.0.6")    // another source has its own count
	a1.Close()
	waitFor(t, "release", func() bool { return live.Get(stats.ConnectionsActive) == 2 })
	open("10.8.0.1") // a closed connection frees its slot

	var locals []net.Conn
	for range 13 { // local clients have no per-source cap
		locals = append(locals, open(""))
	}
	if got := live.Get(stats.ConnectionsActive); got != 16 {
		t.Fatalf("connections_active %d, want 16", got)
	}
	refused("")         // proxy.max_connections
	refused("10.8.0.6") // under its own cap, but over the total
	if got := limit(); got != 3 {
		t.Errorf("rejected for limit %d, want 3", got)
	}
	if got := live.Get(stats.ConnectionsTotal); got != 17 {
		t.Errorf("connections_total %d, want 17", got)
	}
	if got := live.Get(stats.ConnectionsRejectedTotal, "reason", "source"); got != 0 {
		t.Errorf("rejected for source %d", got)
	}
	if len(e.logs.decisions()) != 0 {
		t.Errorf("decisions for refused connections: %v", e.logs.decisions())
	}
	for _, want := range []string{
		`msg="connection limit reached" source=10.8.0.1 limit=proxy.remote.max_connections_per_source max=2`,
		`msg="connection limit reached" source=127.0.0.1 limit=proxy.max_connections max=16`,
		`msg="connection limit reached" source=10.8.0.6 limit=proxy.max_connections max=16`,
	} {
		waitForLog(t, e.logs, want)
	}

	// An accepted connection is served; when cuttle closes it after its
	// response, the slot is free again.
	c := locals[0]
	io.WriteString(c, "CONNECT blocked.cuttle.test:443 HTTP/1.1\r\nHost: blocked.cuttle.test:443\r\n\r\n")
	if b, _ := io.ReadAll(c); !strings.HasPrefix(string(b), "HTTP/1.1 403 ") {
		t.Errorf("served connection got %q", b)
	}
	waitFor(t, "release after response", func() bool { return live.Get(stats.ConnectionsActive) == 15 })
	open("10.8.0.6")
}

// TestCheckSupportedRemote checks that M1.2 serves non-loopback listeners
// and still refuses upstream routes for remote clients.
func TestCheckSupportedRemote(t *testing.T) {
	ok := mustParse(t, `name = "cuttle-test"
[proxy]
listen = ["127.0.0.1:18080", "10.8.0.6:18080"]
default = "direct"
[proxy.remote]
sources = ["10.8.0.1/32"]
allow_direct = true
default = "direct"
`)
	if err := CheckSupported(ok); err != nil {
		t.Errorf("remote listener: %v", err)
	}
	bad := mustParse(t, `name = "cuttle-test"
[proxy]
default = "direct"
[proxy.remote]
sources = ["10.8.0.1/32"]
default = "upstream:gw"
[[upstream]]
name = "gw"
address = "10.8.0.6:3128"
disclosure = "via"
`)
	if err := CheckSupported(bad); !errors.Is(err, ErrUnsupported) || !strings.HasPrefix(err.Error(), "proxy.remote.default: ") {
		t.Errorf("upstream remote default: %v", err)
	}
}
