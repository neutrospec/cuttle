package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"squid/internal/stats"
	"squid/internal/testenv"
)

// reload parses text and applies it to the instance (design 0004 D6).
func (e *env) reload(text string) {
	e.t.Helper()
	if err := e.p.Reload(mustParse(e.t, text)); err != nil {
		e.t.Fatalf("reload: %v", err)
	}
}

// tunnelGET sends a GET for /echo through an open tunnel or kept-alive
// connection and returns the status, or an error when the connection
// ended.
func tunnelGET(c net.Conn, br *bufio.Reader, req string) (int, error) {
	if _, err := io.WriteString(c, req); err != nil {
		return 0, err
	}
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode, nil
}

const originGET = "GET /echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n"

// TestReloadKeepsTunnels checks K3: a tunnel opened before a reload keeps
// transferring after it, even though the new rules block its target, and
// requests after the reload follow the new rules.
func TestReloadKeepsTunnels(t *testing.T) {
	e := newEnv(t, baseConfig)
	c := e.connectRaw("plain.cuttle.test:80")
	br := bufio.NewReader(c)
	if code, err := tunnelGET(c, br, originGET); code != 200 {
		t.Fatalf("before reload: %d %v", code, err)
	}
	e.reload(baseConfig + `
[[rule]]
name = "now-blocked"
hosts = ["plain.cuttle.test"]
action = "block"
`)
	for i := range 3 {
		if code, err := tunnelGET(c, br, originGET); code != 200 {
			t.Fatalf("tunnel request %d after reload: %d %v", i, code, err)
		}
	}
	if got := e.p.Live().Get(stats.TunnelsActive); got != 1 {
		t.Errorf("tunnels_active %d", got)
	}
	want := `cuttle-test; error=http_request_denied; details="blocked by rule now-blocked"`
	res, _ := e.raw("CONNECT plain.cuttle.test:80 HTTP/1.1\r\nHost: plain.cuttle.test:80\r\n\r\n")
	generatedHeaders(t, res, http.StatusForbidden, want)
	res, _ = e.raw("GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
	generatedHeaders(t, res, http.StatusForbidden, want)
	c.Close()
	ds := e.waitDecisions(3)
	// The tunnel was decided before the reload and carried four requests.
	checkDecision(t, ds[2], map[string]string{"kind": "connect", "rule": "default", "action": "direct", "outcome": "ok",
		"status": "200", "up": strconv.Itoa(4 * len(originGET))})
}

// upstreamReloadConfig routes plain.cuttle.test to the upstream gw.
const upstreamReloadConfig = baseConfig + `
[[upstream]]
name = "gw"
address = "ADDRESS"
disclosure = "via"

[[rule]]
name = "via-gw"
hosts = ["plain.cuttle.test"]
action = "upstream:gw"
`

// TestReloadUpstreamChange checks K6: after a reload changes an upstream's
// address, an existing tunnel through the old address continues, and new
// requests use the new address.
func TestReloadUpstreamChange(t *testing.T) {
	e := newEnv(t, strings.Replace(upstreamReloadConfig, "ADDRESS", "10.8.0.6:3128", 1))
	oldUp := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: e.originNet()})
	newUp := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: e.originNet()})
	e.net.Route("10.8.0.6:3128", oldUp.Addr)
	e.net.Route("10.8.0.7:3128", newUp.Addr)

	c := e.connectRaw("plain.cuttle.test:80")
	br := bufio.NewReader(c)
	if code, err := tunnelGET(c, br, originGET); code != 200 {
		t.Fatalf("before reload: %d %v", code, err)
	}
	e.reload(strings.Replace(upstreamReloadConfig, "ADDRESS", "10.8.0.7:3128", 1))
	for i := range 3 {
		if code, err := tunnelGET(c, br, originGET); code != 200 {
			t.Fatalf("old tunnel request %d after reload: %d %v", i, code, err)
		}
	}
	n := e.connectRaw("plain.cuttle.test:80")
	if code, err := tunnelGET(n, bufio.NewReader(n), originGET); code != 200 {
		t.Fatalf("new tunnel: %d %v", code, err)
	}
	res, _ := e.raw("GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\nConnection: close\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("plain HTTP after reload: %d", res.StatusCode)
	}
	methods := func(u *testenv.RecordingUpstream) []string {
		var out []string
		for _, r := range u.Requests() {
			out = append(out, r.Method+" "+r.Target)
		}
		return out
	}
	if got := methods(oldUp); !slices.Equal(got, []string{"CONNECT plain.cuttle.test:80"}) {
		t.Errorf("old address received %v", got)
	}
	if got := methods(newUp); !slices.Equal(got, []string{"CONNECT plain.cuttle.test:80", "GET http://plain.cuttle.test/echo"}) {
		t.Errorf("new address received %v", got)
	}
	// The old tunnel's bytes still flow through the old address.
	if raw := string(oldUp.Raw(1)); strings.Count(raw, originGET) != 4 {
		t.Errorf("old upstream relayed %d tunnel requests", strings.Count(raw, originGET))
	}
}

// stateConfig has four upstreams: keep and change are unreachable with a
// long hold, pool reaches a recording upstream, and gone is removed by the
// reload.
const stateConfig = baseConfig + `
[[upstream]]
name = "keep"
address = "10.8.0.6:3128"
disclosure = "via"
unreachable_hold = "1m"

[[upstream]]
name = "change"
address = "10.8.0.7:3128"
disclosure = "via"
unreachable_hold = "1m"

[[upstream]]
name = "pool"
address = "10.8.0.8:3128"
disclosure = "via"

[[rule]]
name = "keep"
hosts = ["keep.cuttle.test"]
action = "upstream:keep"

[[rule]]
name = "change"
hosts = ["change.cuttle.test"]
action = "upstream:change"

[[rule]]
name = "pool"
hosts = ["pool.cuttle.test"]
action = "upstream:pool"
`

const goneConfig = `
[[upstream]]
name = "gone"
address = "10.8.0.9:3128"
disclosure = "via"

[[rule]]
name = "gone"
hosts = ["gone.cuttle.test"]
action = "upstream:gone"
`

// TestReloadUpstreamState checks design 0003 D8 and 0004 D6 across a
// reload: an upstream whose settings are unchanged keeps its unreachable
// hold and its pooled connections; a changed upstream starts afresh in
// state up; a removed one leaves the status and the gauges.
func TestReloadUpstreamState(t *testing.T) {
	e := newEnv(t, stateConfig+goneConfig)
	n := e.originNet()
	n.Map("pool.cuttle.test", "203.0.113.10")
	rec := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: n})
	e.net.Route("10.8.0.8:3128", rec.Addr)
	client, _ := e.goClient()

	connect := func(name string) *http.Response {
		t.Helper()
		res, _ := e.raw("CONNECT " + name + ":443 HTTP/1.1\r\nHost: " + name + ":443\r\n\r\n")
		return res
	}
	dials := func(addr string) int {
		return len(slices.DeleteFunc(e.net.Dials(), func(a netip.AddrPort) bool { return a.String() != addr }))
	}
	getPool := func() {
		t.Helper()
		res, err := client.Get("http://pool.cuttle.test/echo")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("pool: %d", res.StatusCode)
		}
	}
	status := func(name string) UpstreamStatus {
		for _, u := range e.p.Upstreams() {
			if u.Name == name {
				return u
			}
		}
		return UpstreamStatus{}
	}
	gauge := func(name, state string) int64 {
		return e.p.Live().Get(stats.UpstreamState, "upstream", name, "state", state)
	}

	for _, name := range []string{"keep", "change"} {
		if res := connect(name + ".cuttle.test"); res.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s: first request %d", name, res.StatusCode)
		}
		if res := connect(name + ".cuttle.test"); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: held request %d", name, res.StatusCode)
		}
	}
	getPool()
	if rec.Connections() != 1 {
		t.Fatalf("pool connections %d", rec.Connections())
	}
	keepDials := dials("10.8.0.6:3128")

	changed := strings.Replace(stateConfig, "address = \"10.8.0.7:3128\"\ndisclosure = \"via\"",
		"address = \"10.8.0.7:3128\"\ndisclosure = \"via\"\nresponse_timeout = \"30s\"", 1)
	if changed == stateConfig {
		t.Fatal("change pattern not found")
	}
	e.reload(changed)

	// The unchanged upstream is still held: it answers at once without a
	// dial, and its outage goes on counting.
	if st := status("keep"); st.State != StateDown || st.FailedFast != 1 || st.DownFor <= 0 {
		t.Errorf("keep after reload: %+v", st)
	}
	if gauge("keep", StateDown) != 1 || gauge("keep", StateUp) != 0 {
		t.Errorf("keep gauges up %d down %d", gauge("keep", StateUp), gauge("keep", StateDown))
	}
	if res := connect("keep.cuttle.test"); res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("keep after reload: %d", res.StatusCode)
	}
	if d := dials("10.8.0.6:3128"); d != keepDials {
		t.Errorf("keep dialed %d times after reload", d-keepDials)
	}
	if st := status("keep"); st.FailedFast != 2 {
		t.Errorf("keep failed_fast %d, want 2", st.FailedFast)
	}

	// The changed upstream starts up, so the next request dials.
	if st := status("change"); st.State != StateUp || st.FailedFast != 0 || st.DownFor != 0 {
		t.Errorf("change after reload: %+v", st)
	}
	if gauge("change", StateUp) != 1 || gauge("change", StateDown) != 0 {
		t.Errorf("change gauges up %d down %d", gauge("change", StateUp), gauge("change", StateDown))
	}
	changeDials := dials("10.8.0.7:3128")
	if res := connect("change.cuttle.test"); res.StatusCode != http.StatusBadGateway {
		t.Errorf("change after reload: %d", res.StatusCode)
	}
	if d := dials("10.8.0.7:3128"); d != changeDials+1 {
		t.Errorf("change dialed %d times after reload, want 1", d-changeDials)
	}

	// The unchanged upstream keeps its pooled connection.
	getPool()
	if rec.Connections() != 1 {
		t.Errorf("pool connections %d after reload, want the pooled one reused", rec.Connections())
	}

	// The removed upstream leaves the status and its gauges.
	for _, u := range e.p.Upstreams() {
		if u.Name == "gone" {
			t.Errorf("removed upstream still listed: %+v", u)
		}
	}
	for _, k := range e.p.Live().Keys() {
		if strings.Contains(k, `upstream="gone"`) && strings.HasPrefix(k, stats.UpstreamState) {
			t.Errorf("gauge %s remains", k)
		}
	}
}

// sourceConfig lets remote clients from 10.8.0.1 reach plain.cuttle.test
// directly.
const sourceConfig = baseConfig + `
[[rule]]
name = "open"
hosts = ["plain.cuttle.test"]
action = "direct"

[proxy.remote]
sources = ["10.8.0.1/32"]
allow_direct = true
`

// TestReloadRemovesSource checks N5 (design 0003 D10, second bullet): after
// a reload removes a remote source, the next request on its kept-alive
// connection gets no response and the connection closes, while its
// established tunnel keeps transferring and local clients are unaffected;
// new connections from the source are refused on accept.
func TestReloadRemovesSource(t *testing.T) {
	e := newEnvWith(t, sourceConfig, envOptions{remote: []string{"10.8.0.1"}})
	const plainGET = "GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n"

	kept := e.dialFrom("10.8.0.1")
	keptBR := bufio.NewReader(kept)
	if code, err := tunnelGET(kept, keptBR, plainGET); code != 200 {
		t.Fatalf("remote kept-alive request: %d %v", code, err)
	}
	tunnel := e.connectRawFrom("10.8.0.1", "plain.cuttle.test:80")
	tunnelBR := bufio.NewReader(tunnel)
	if code, err := tunnelGET(tunnel, tunnelBR, originGET); code != 200 {
		t.Fatalf("remote tunnel: %d %v", code, err)
	}
	local := e.dial()
	localBR := bufio.NewReader(local)
	if code, err := tunnelGET(local, localBR, plainGET); code != 200 {
		t.Fatalf("local kept-alive request: %d %v", code, err)
	}
	originBefore := len(e.http.Requests())

	e.reload(strings.Replace(sourceConfig, `sources = ["10.8.0.1/32"]`, `sources = ["10.8.0.2/32"]`, 1))

	// The kept-alive connection gets no response and closes.
	io.WriteString(kept, plainGET)
	kept.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(keptBR); len(b) != 0 || err != nil {
		t.Errorf("removed source's kept-alive request got %q, %v; want no response and a close", b, err)
	}
	// The recheck logged the rejected-source warning before it closed the
	// connection. No new connection from the source exists yet, so the
	// warning cannot come from the accept path.
	if !strings.Contains(e.logs.String(), `msg="rejected source" source=10.8.0.1`) {
		t.Errorf("no rejected-source warning from the recheck:\n%s", e.logs.String())
	}
	if got := e.p.Live().Get(stats.ConnectionsRejectedTotal, "reason", "source"); got != 0 {
		t.Errorf("connections_rejected_total{reason=source} %d before any new connection", got)
	}
	// The tunnel keeps transferring, and local clients are unaffected.
	for range 2 {
		if code, err := tunnelGET(tunnel, tunnelBR, originGET); code != 200 {
			t.Errorf("tunnel after reload: %d %v", code, err)
		}
	}
	if code, err := tunnelGET(local, localBR, plainGET); code != 200 {
		t.Errorf("local request after reload: %d %v", code, err)
	}
	if got := len(e.http.Requests()) - originBefore; got != 3 {
		t.Errorf("origin received %d requests after the reload, want 3 (two through the tunnel, one local)", got)
	}
	// A new connection from the removed source is refused on accept.
	fresh := e.dialFrom("10.8.0.1")
	if b, _ := io.ReadAll(fresh); len(b) != 0 {
		t.Errorf("new connection from the removed source got %q", b)
	}
	waitFor(t, "rejected connection counted", func() bool {
		return e.p.Live().Get(stats.ConnectionsRejectedTotal, "reason", "source") == 1
	})
	tunnel.Close()
	ds := e.waitDecisions(3)
	remoteHTTP := 0
	for _, d := range ds {
		if d["source"] == "remote" && d["kind"] == "http" {
			remoteHTTP++
		}
	}
	if remoteHTTP != 1 {
		t.Errorf("%d remote plain-HTTP decisions, want 1 (the dropped request logs none): %v", remoteHTTP, ds)
	}
}

// retireConfig routes pool.cuttle.test to the upstream pooled and
// late.cuttle.test to the upstream late, at the addresses POOLED and LATE.
const retireConfig = baseConfig + `
[[upstream]]
name = "pooled"
address = "POOLED"
disclosure = "via"

[[upstream]]
name = "late"
address = "LATE"
disclosure = "via"

[[rule]]
name = "pooled"
hosts = ["pool.cuttle.test"]
action = "upstream:pooled"

[[rule]]
name = "late"
hosts = ["late.cuttle.test"]
action = "upstream:late"
`

// TestReloadRetiresUpstreams checks what a reload does to the state it
// replaces (design 0003 D8, 0004 D6 step 3): a replaced upstream closes its
// pooled connection; a request that finishes on a replaced upstream after
// the reload changes neither its gauges nor the status, which show the new
// upstream; and the old direct transports close their idle connections.
func TestReloadRetiresUpstreams(t *testing.T) {
	cfg := func(pooled, late string) string {
		return strings.NewReplacer("POOLED", pooled, "LATE", late).Replace(retireConfig)
	}
	e := newEnv(t, cfg("10.8.0.6:3128", "slow-gw.cuttle.test:3128"))
	// The recording upstreams reach their own origin, so e.http sees only
	// the connections of cuttle's direct transport.
	other := testenv.NewOrigin(t, testenv.OriginOptions{})
	n := testenv.NewNet()
	n.Map("pool.cuttle.test", "203.0.113.11")
	n.Route("203.0.113.11:80", other.Addr)
	oldRec := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: n})
	newRec := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: n})
	e.net.Route("10.8.0.6:3128", oldRec.Addr)
	e.net.Route("10.8.0.7:3128", newRec.Addr)
	// The late upstream's own name resolves only after 500 ms, and then not
	// at all, which would hold the upstream as unreachable.
	e.net.Delay("slow-gw.cuttle.test", 500*time.Millisecond)
	client, _ := e.goClient()
	get := func(u string) {
		t.Helper()
		res, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", u, res.StatusCode)
		}
	}
	gauge := func(state string) int64 {
		return e.p.Live().Get(stats.UpstreamState, "upstream", "late", "state", state)
	}

	// Idle pooled connections on the old upstream transport and on the old
	// direct transport.
	get("http://pool.cuttle.test/echo")
	get("http://plain.cuttle.test/echo")
	waitFor(t, "pooled connections", func() bool { return oldRec.OpenConnections() == 1 && e.http.OpenConnections() == 1 })

	// A request on the old late upstream is still resolving its name.
	late := make(chan int, 1)
	go func() {
		late <- rawStatus(e.addr, "CONNECT late.cuttle.test:443 HTTP/1.1\r\nHost: late.cuttle.test:443\r\n\r\n")
	}()
	waitFor(t, "lookup in flight", func() bool { return slices.Contains(e.net.Lookups(), "slow-gw.cuttle.test") })

	e.reload(cfg("10.8.0.7:3128", "10.8.0.8:3128"))

	waitFor(t, "the replaced upstream's pooled connection to close", func() bool { return oldRec.OpenConnections() == 0 })
	waitFor(t, "the old direct transport's idle connection to close", func() bool { return e.http.OpenConnections() == 0 })

	if code := <-late; code != http.StatusBadGateway {
		t.Errorf("request on the replaced upstream: %d, want 502 dns_error", code)
	}
	// The failure on the replaced upstream changed nothing that the new
	// upstream shows.
	if gauge(StateUp) != 1 || gauge(StateDown) != 0 || gauge(StateProbing) != 0 {
		t.Errorf("late gauges up %d down %d probing %d, want the new upstream's up", gauge(StateUp), gauge(StateDown), gauge(StateProbing))
	}
	for _, u := range e.p.Upstreams() {
		if u.Name == "late" && (u.State != StateUp || u.Address != "10.8.0.8:3128" || u.LastError != "") {
			t.Errorf("late status %+v", u)
		}
	}
	if strings.Contains(e.logs.String(), `msg="upstream unreachable" upstream=late`) {
		t.Errorf("replaced upstream logged a state change:\n%s", e.logs.String())
	}

	// New requests use the new upstream and new direct connections.
	get("http://pool.cuttle.test/echo")
	get("http://plain.cuttle.test/echo")
	if len(oldRec.Requests()) != 1 || len(newRec.Requests()) != 1 {
		t.Errorf("requests: old %d new %d", len(oldRec.Requests()), len(newRec.Requests()))
	}
}
