package proxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"squid/internal/testenv"
)

// Chain configurations of design 0003 D13 and 0007 D4: the edge sends
// cuttle.test names and h to the gateway at 10.8.0.6:3128 with
// disclosure = "via"; the gateway serves the edge as remote client
// 10.8.0.1 and sends them to the egress proxy with disclosure = "none".
// The %s verbs are the response_timeout of each upstream.
const (
	edgeChainConfig = `
name = "cuttle-edge"

[proxy]
default = "direct"

[admin]
listen = ""

[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"
response_timeout = "%s"

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "restricted"
hosts = ["h"]
domains = ["cuttle.test"]
action = "upstream:gateway"
`
	gatewayChainConfig = `
name = "cuttle-gateway"

[proxy]
default = "upstream:egress"

[proxy.remote]
sources = ["10.8.0.1/32"]
default = "block"

[admin]
listen = ""

[[upstream]]
name = "egress"
address = "proxy.internal.example:8080"
disclosure = "none"
response_timeout = "%s"

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "restricted"
hosts = ["h"]
domains = ["cuttle.test"]
action = "upstream:egress"
`
)

// newChain starts an edge and a gateway instance. The egress proxy's name
// resolves to 10.20.0.80 in the gateway's network; the test routes
// 10.20.0.80:8080 to its mock.
func newChain(t *testing.T, edgeTimeout, gatewayTimeout string, edgeOpt envOptions) (edge, gw *env) {
	t.Helper()
	edge = newEnvWith(t, fmt.Sprintf(edgeChainConfig, edgeTimeout), edgeOpt)
	gw = edge.peer(fmt.Sprintf(gatewayChainConfig, gatewayTimeout), envOptions{remote: []string{"10.8.0.1"}})
	edge.net.Route("10.8.0.6:3128", gw.listenerAddr("10.8.0.1"))
	gw.net.Map("proxy.internal.example", "10.20.0.80")
	return edge, gw
}

// checkNoDisclosureHead checks a request head and trailer received at the
// end of a chain: no disclosure field of design 0003 D5, the client's own
// included, and no value containing an instance name or an address of the
// chain.
func checkNoDisclosureHead(t *testing.T, r testenv.ProxyRequest, secrets ...string) {
	t.Helper()
	disclosure := func(name string) bool {
		ck := http.CanonicalHeaderKey(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
		_, bad := clientDisclosure[ck]
		return bad || strings.HasPrefix(ck, "X-Forwarded")
	}
	for _, f := range append(append([]testenv.Field(nil), r.Fields...), r.Trailer...) {
		if disclosure(f.Name) {
			t.Errorf("%s %s: disclosure field %s: %q", r.Method, r.Target, f.Name, f.Value)
		}
		if strings.EqualFold(f.Name, "Trailer") {
			for _, n := range strings.Split(f.Value, ",") {
				if disclosure(n) {
					t.Errorf("%s %s: Trailer declares disclosure field %s", r.Method, r.Target, n)
				}
			}
		}
		for _, s := range append([]string{"cuttle-edge", "cuttle-gateway"}, secrets...) {
			if strings.Contains(strings.ToLower(f.Value), s) {
				t.Errorf("%s %s: field %s discloses %q: %q", r.Method, r.Target, f.Name, s, f.Value)
			}
		}
	}
}

// TestChainInspecting checks U6: edge (via) -> gateway (none) ->
// inspecting egress mock. The mock's CONNECT head has no disclosure field;
// a client trusting the inspection root gets 200 and a leaf for the name
// issued by that root, and the mock decrypts the expected Host and path; a
// client trusting only the origin root fails verification while both
// instances log ok tunnels.
func TestChainInspecting(t *testing.T) {
	edge, gw := newChain(t, "80s", "60s", envOptions{})
	inspection := testenv.NewCA(t, "cuttle test inspection root")
	mock := testenv.NewInspectingProxy(t, testenv.InspectingOptions{Root: inspection, Origins: edge.ca.Pool(), Net: edge.originNet()})
	gw.net.Route("10.20.0.80:8080", mock.Addr)
	proxyURL := &url.URL{Scheme: "http", Host: edge.addr}
	client := func(roots *x509.CertPool) (*http.Client, *http.Transport) {
		tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots},
			DisableCompression: true, ForceAttemptHTTP2: false}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr, Timeout: 15 * time.Second}, tr
	}

	trusting, tr := client(inspection.Pool())
	res, err := trusting.Get("https://origin.cuttle.test/chain")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	tr.CloseIdleConnections()
	if res.StatusCode != 200 || !bytes.Contains(body, []byte(`"uri":"/chain"`)) {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	leaf := res.TLS.PeerCertificates[0]
	if leaf.Issuer.CommonName != "cuttle test inspection root" || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "origin.cuttle.test" {
		t.Errorf("leaf issuer %q names %v", leaf.Issuer.CommonName, leaf.DNSNames)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "origin.cuttle.test", Roots: inspection.Pool()}); err != nil {
		t.Errorf("leaf does not verify with the inspection root: %v", err)
	}
	dec := mock.Decrypted()
	if len(dec) != 1 || dec[0].Method != "GET" || dec[0].URI != "/chain" || dec[0].Host != "origin.cuttle.test" {
		t.Errorf("decrypted %+v", dec)
	}

	r := testenv.Curl(t, "-sS", "--proxy", "http://"+edge.addr, "--cacert", inspection.WritePEM(t),
		"-w", "%{http_connect} %{http_code}", "-o", "/dev/null", "https://origin.cuttle.test/chain")
	if r.Code != 0 || r.Stdout != "200 200" {
		t.Errorf("curl trusting the inspection root: exit %d out %q err %q", r.Code, r.Stdout, r.Stderr)
	}

	untrusting, tr2 := client(edge.ca.Pool())
	if _, err := untrusting.Get("https://origin.cuttle.test/chain"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("client trusting only the origin root: %v", err)
	}
	tr2.CloseIdleConnections()
	r = testenv.Curl(t, "-sS", "--proxy", "http://"+edge.addr, "--cacert", edge.ca.WritePEM(t), "-o", "/dev/null",
		"https://origin.cuttle.test/chain")
	if r.Code != 60 {
		t.Errorf("curl trusting only the origin root: exit %d err %q", r.Code, r.Stderr)
	}

	heads := mock.Requests()
	if len(heads) != 4 {
		t.Fatalf("mock received %d heads", len(heads))
	}
	for _, h := range heads {
		if h.Method != "CONNECT" || h.Target != "origin.cuttle.test:443" {
			t.Errorf("head %s %s", h.Method, h.Target)
		}
		checkNoDisclosureHead(t, h, "10.8.0.1")
	}
	for _, d := range edge.waitDecisions(4) {
		checkDecision(t, d, map[string]string{"source": "local", "action": "upstream", "upstream": "gateway", "outcome": "ok", "status": "200"})
	}
	for _, d := range gw.waitDecisions(4) {
		checkDecision(t, d, map[string]string{"source": "remote", "rule": "restricted", "action": "upstream", "upstream": "egress",
			"outcome": "ok", "status": "200"})
		if !strings.HasPrefix(d["client"], "10.8.0.1:") {
			t.Errorf("gateway client %q", d["client"])
		}
	}
	onlyDials(t, edge.net, "10.8.0.6:3128")
	onlyDials(t, gw.net, "10.20.0.80:8080")
}

// TestChainRefusal checks U7: when the inspecting egress mock refuses a
// name, the original client gets 502 with the gateway's member before the
// edge's.
func TestChainRefusal(t *testing.T) {
	edge, gw := newChain(t, "80s", "60s", envOptions{})
	mock := testenv.NewInspectingProxy(t, testenv.InspectingOptions{Root: testenv.NewCA(t, "inspection"), Origins: edge.ca.Pool(),
		Net: edge.originNet(), Refuse: []string{"refused.cuttle.test"}})
	gw.net.Route("10.20.0.80:8080", mock.Addr)
	want := `cuttle-gateway; received-status=403; next-hop="egress", cuttle-edge; received-status=502; next-hop="gateway"`
	res, _ := edge.raw("CONNECT refused.cuttle.test:443 HTTP/1.1\r\nHost: refused.cuttle.test:443\r\n\r\n")
	generatedHeaders(t, res, 502, want)
	r := testenv.Curl(t, "-sS", "--proxy", "http://"+edge.addr, "-w", "%{http_connect}", "-o", "/dev/null", "https://refused.cuttle.test/")
	if r.Stdout != "502" || r.Code != 56 {
		t.Errorf("curl: exit %d out %q err %q", r.Code, r.Stdout, r.Stderr)
	}
	checkDecision(t, edge.waitDecisions(1)[0], map[string]string{"outcome": "upstream_status", "status": "502"})
	checkDecision(t, gw.waitDecisions(1)[0], map[string]string{"outcome": "upstream_status", "status": "502", "source": "remote"})
}

// TestChainLoop checks U8: the edge routes loop.cuttle.test to the gateway
// and the gateway routes it back to the edge, both with disclosure = "via".
// The client gets 502 with error=proxy_loop_detected after exactly 2
// upstream requests in total, for CONNECT and for plain HTTP.
func TestChainLoop(t *testing.T) {
	edgeCfg := `
name = "cuttle-edge"

[proxy]
default = "direct"
max_connections = 16 # bounds a loop that detection misses

[proxy.remote]
sources = ["10.8.0.6/32"]

[admin]
listen = ""

[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"

[[rule]]
name = "loop"
domains = ["loop.cuttle.test"]
action = "upstream:gateway"
`
	gwCfg := `
name = "cuttle-gateway"

[proxy]
default = "block"
max_connections = 16

[proxy.remote]
sources = ["10.8.0.1/32"]

[admin]
listen = ""

[[upstream]]
name = "edge"
address = "10.8.0.1:3128"
disclosure = "via"

[[rule]]
name = "loop"
domains = ["loop.cuttle.test"]
action = "upstream:edge"
`
	// The edge's second listener reports the gateway's address.
	edge := newEnvWith(t, edgeCfg, envOptions{remote: []string{"10.8.0.6"}})
	gw := edge.peer(gwCfg, envOptions{remote: []string{"10.8.0.1"}})
	edge.net.Route("10.8.0.6:3128", gw.listenerAddr("10.8.0.1"))
	gw.net.Route("10.8.0.1:3128", edge.listenerAddr("10.8.0.6"))
	upstreamDials := func() int { return dialsTo(edge.net, "10.8.0.6:3128") + dialsTo(gw.net, "10.8.0.1:3128") }
	// forwarded counts the requests one instance received from the other:
	// every gateway decision and the edge's decisions for remote clients.
	forwarded := func() int {
		n := len(gw.logs.decisions())
		for _, d := range edge.logs.decisions() {
			if d["source"] == "remote" {
				n++
			}
		}
		return n
	}
	loopMember := `cuttle-edge; error=proxy_loop_detected; details="Via names this instance"`

	// The second round has the client's own Via first, so the instance's
	// entry is not the first Via line it receives (OI-0006 U-R2).
	for round, clientVia := range []string{"", "Via: 1.1 lab-gw\r\n"} {
		dials, fwd, decisions, gwDecisions := upstreamDials(), forwarded(), len(edge.logs.decisions()), len(gw.logs.decisions())
		res, _ := edge.raw("CONNECT loop.cuttle.test:443 HTTP/1.1\r\nHost: loop.cuttle.test:443\r\n" + clientVia + "\r\n")
		generatedHeaders(t, res, 502, loopMember+`, cuttle-gateway; received-status=502; next-hop="edge", cuttle-edge; received-status=502; next-hop="gateway"`)
		ed := edge.waitDecisions(decisions + 2)[decisions:]
		gw.waitDecisions(gwDecisions + 1)
		if n := upstreamDials() - dials; n != 2 {
			t.Errorf("round %d CONNECT: %d upstream dials", round, n)
		}
		if n := forwarded() - fwd; n != 2 {
			t.Errorf("round %d CONNECT: %d upstream requests", round, n)
		}
		var outcomes []string
		for _, d := range ed {
			outcomes = append(outcomes, d["kind"]+":"+d["source"]+":"+d["outcome"])
		}
		if strings.Join(outcomes, ",") != "connect:remote:loop,connect:local:upstream_status" {
			t.Errorf("round %d CONNECT: edge outcomes %v", round, outcomes)
		}

		fwd, decisions, gwDecisions = forwarded(), decisions+2, gwDecisions+1
		res, _ = edge.raw("GET http://loop.cuttle.test/x HTTP/1.1\r\nHost: loop.cuttle.test\r\n" + clientVia + "\r\n")
		if res.StatusCode != 502 || res.Header.Get("Proxy-Status") != loopMember {
			t.Errorf("round %d plain HTTP: %d %q", round, res.StatusCode, res.Header.Get("Proxy-Status"))
		}
		ed = edge.waitDecisions(decisions + 2)[decisions:]
		gw.waitDecisions(gwDecisions + 1)
		if n := forwarded() - fwd; n != 2 {
			t.Errorf("round %d plain HTTP: %d upstream requests", round, n)
		}
		outcomes = nil
		for _, d := range ed {
			outcomes = append(outcomes, d["kind"]+":"+d["source"]+":"+d["outcome"]+":"+d["status"])
		}
		if strings.Join(outcomes, ",") != "http:remote:loop:502,http:local:ok:502" {
			t.Errorf("round %d plain HTTP: edge outcomes %v", round, outcomes)
		}
	}
	for _, d := range gw.waitDecisions(4) {
		checkDecision(t, d, map[string]string{"source": "remote", "action": "upstream", "upstream": "edge"})
	}
}

// TestChainTimeoutPropagates checks U10: with the gateway's egress
// response_timeout at 300 ms and the edge's gateway response_timeout at
// 2 s, a silent egress mock yields, for CONNECT, 504 with
// received-status=504 and the gateway member before the edge member, and
// for plain HTTP the gateway's 504 unchanged with the gateway's member only.
func TestChainTimeoutPropagates(t *testing.T) {
	edge, gw := newChain(t, "2s", "300ms", envOptions{})
	rec := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: edge.originNet()})
	rec.SetMode("slow.cuttle.test", testenv.Silent())
	gw.net.Route("10.20.0.80:8080", rec.Addr)
	gwMember := `cuttle-gateway; error=http_response_timeout; next-hop="egress"; details="no response head within 300ms"`

	start := time.Now()
	res, _ := edge.raw("CONNECT slow.cuttle.test:443 HTTP/1.1\r\nHost: slow.cuttle.test:443\r\n\r\n")
	generatedHeaders(t, res, 504, gwMember+`, cuttle-edge; received-status=504; next-hop="gateway"`)
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Errorf("CONNECT took %v", el)
	}

	start = time.Now()
	res, body := edge.raw("GET http://slow.cuttle.test/x HTTP/1.1\r\nHost: slow.cuttle.test\r\n\r\n")
	if res.StatusCode != 504 || res.Header.Values("Proxy-Status")[0] != gwMember || len(res.Header.Values("Proxy-Status")) != 1 ||
		!strings.Contains(string(body), "cuttle: no response from upstream egress") {
		t.Errorf("plain HTTP: %d %q %q", res.StatusCode, res.Header.Values("Proxy-Status"), body)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Errorf("plain HTTP took %v", el)
	}
	ed := edge.waitDecisions(2)
	checkDecision(t, ed[0], map[string]string{"outcome": "upstream_status", "status": "504"})
	checkDecision(t, ed[1], map[string]string{"outcome": "ok", "status": "504"})
	gd := gw.waitDecisions(2)
	checkDecision(t, gd[0], map[string]string{"outcome": "timeout", "status": "504", "error": "http_response_timeout"})
	checkDecision(t, gd[1], map[string]string{"outcome": "timeout", "status": "504", "error": "http_response_timeout"})
}

// sentRequest is one request of the non-disclosure chain as the client
// sent it.
type sentRequest struct {
	method, target string
	fields         []testenv.Field
	body           []byte          // decoded
	raw            string          // body bytes on the wire, trailer included
	trailer        []testenv.Field // the trailer fields in raw
}

func (s sentRequest) wire() string {
	var b strings.Builder
	b.WriteString(s.method + " " + s.target + " HTTP/1.1\r\n")
	for _, f := range s.fields {
		b.WriteString(f.Name + ": " + f.Value + "\r\n")
	}
	b.WriteString("\r\n" + s.raw)
	return b.String()
}

// TestNoDisclosureChain checks U12: an original client with a distinctive
// source address sends a CONNECT and plain-HTTP requests through edge
// (via) -> gateway (none) -> recording mock, including its own Via and
// X-Real-IP and one request without User-Agent. The mock receives no field
// or value that cuttle added, no disclosure field, no instance name, and
// no address of the chain, and the client's method, target, and body
// bytes. OPTIONS and TRACE with Max-Forwards: 0 arrive unchanged and are
// answered by the mock.
func TestNoDisclosureChain(t *testing.T) {
	const clientAddr = "127.31.41.59" // loopback, so the edge sees a local client
	edge, gw := newChain(t, "80s", "60s", envOptions{local: clientAddr})
	rec := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: edge.originNet()})
	rec.SetMode("h", testenv.Pass())
	gw.net.Route("10.20.0.80:8080", rec.Addr)

	common := []testenv.Field{{Name: "Via", Value: "1.1 lab-gw"}, {Name: "X-Real-IP", Value: "198.51.100.7"},
		// The wider list of the TASK-0006 decision (0003 D5).
		{Name: "X-Forwarded", Value: "for=198.51.100.7"}, {Name: "Forwarded-For", Value: "198.51.100.7"},
		{Name: "X-Original-Forwarded-For", Value: "198.51.100.7"}, {Name: "X-Client-IP", Value: "198.51.100.7"},
		{Name: "X-Cluster-Client-IP", Value: "198.51.100.7"}}
	f := func(fs ...testenv.Field) []testenv.Field {
		return append(append([]testenv.Field(nil), fs...), common...)
	}
	ua := testenv.Field{Name: "User-Agent", Value: "nd-client/1.0"}
	chunkBody := []byte("first chunk;second chunk")
	reqs := []sentRequest{
		{method: "CONNECT", target: "origin.cuttle.test:443",
			fields: f(testenv.Field{Name: "Host", Value: "origin.cuttle.test:443"}, ua, testenv.Field{Name: "X-Custom", Value: "one"})},
		{method: "POST", target: "http://plain.cuttle.test/upload?k=v",
			fields: f(testenv.Field{Name: "Host", Value: "plain.cuttle.test"}, ua,
				testenv.Field{Name: "Content-Type", Value: "application/octet-stream"}, testenv.Field{Name: "Transfer-Encoding", Value: "chunked"}),
			body: chunkBody, raw: "c\r\nfirst chunk;\r\nc\r\nsecond chunk\r\n0\r\n\r\n"},
		{method: "GET", target: "http://h/a{b}|c?x=1;y=2&z=%zz", fields: f(testenv.Field{Name: "Host", Value: "h"}, ua)},
		{method: "GET", target: "http://h/q?b=2&a=1;c=3", fields: f(testenv.Field{Name: "Host", Value: "h"})}, // no User-Agent
		{method: "OPTIONS", target: "http://plain.cuttle.test/x",
			fields: f(testenv.Field{Name: "Host", Value: "plain.cuttle.test"}, ua, testenv.Field{Name: "Max-Forwards", Value: "0"})},
		{method: "TRACE", target: "http://plain.cuttle.test/x",
			fields: f(testenv.Field{Name: "Host", Value: "plain.cuttle.test"}, ua, testenv.Field{Name: "Max-Forwards", Value: "0"})},
		// A trailer that carries disclosure fields (OI-0006 U-R6).
		{method: "POST", target: "http://plain.cuttle.test/trailer",
			fields: f(testenv.Field{Name: "Host", Value: "plain.cuttle.test"}, ua, testenv.Field{Name: "Transfer-Encoding", Value: "chunked"},
				testenv.Field{Name: "Trailer", Value: "X-Checksum, X-Forwarded-For, Via, X-Real-IP"}),
			body: []byte("traced"),
			raw:  "6\r\ntraced\r\n0\r\nX-Checksum: abc\r\nX-Forwarded-For: 198.51.100.7\r\nVia: 1.1 lab-gw\r\nX-Real-IP: 198.51.100.7\r\n\r\n",
			trailer: []testenv.Field{{Name: "X-Checksum", Value: "abc"}, {Name: "X-Forwarded-For", Value: "198.51.100.7"},
				{Name: "Via", Value: "1.1 lab-gw"}, {Name: "X-Real-IP", Value: "198.51.100.7"}}},
		// Expect: 100-continue through the chain (OI-0006 U-R5).
		{method: "POST", target: "http://plain.cuttle.test/expect",
			fields: f(testenv.Field{Name: "Host", Value: "plain.cuttle.test"}, ua, testenv.Field{Name: "Expect", Value: "100-continue"},
				testenv.Field{Name: "Content-Length", Value: "5"}),
			body: []byte("hello"), raw: "hello"},
	}
	for _, s := range reqs {
		if s.method == "CONNECT" {
			c := edge.dial()
			io.WriteString(c, s.wire())
			got := make([]byte, len(connectEstablished))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != connectEstablished {
				t.Fatalf("CONNECT: %q %v", got, err)
			}
			if res, _ := tlsGet(t, c, edge, "origin.cuttle.test", "/x"); res.StatusCode != 200 {
				t.Errorf("through the tunnel: %d", res.StatusCode)
			}
			c.Close()
			continue
		}
		res, body := edge.raw(s.wire()) // skips 100 Continue
		if res.StatusCode != 200 || res.Header.Get("Proxy-Status") != "" {
			t.Errorf("%s %s: %d %v", s.method, s.target, res.StatusCode, res.Header)
		}
		if s.method == "POST" && !bytes.Contains(body, []byte(`"method":"POST"`)) {
			t.Errorf("%s %s not answered by the origin: %s", s.method, s.target, body)
		}
		if (s.method == "OPTIONS" || s.method == "TRACE") && !bytes.Contains(body, []byte(`"method":"`+s.method+`"`)) {
			t.Errorf("%s not answered by the mock: %s", s.method, body)
		}
	}

	got := rec.Requests()
	if len(got) != len(reqs) {
		t.Fatalf("mock received %d requests, want %d", len(got), len(reqs))
	}
	for i, r := range got {
		s := reqs[i]
		if r.Method != s.method || r.Target != s.target || !bytes.Equal(r.Body, s.body) {
			t.Errorf("request %d: %s %s body %q, want %s %s %q", i, r.Method, r.Target, r.Body, s.method, s.target, s.body)
		}
		checkNoDisclosureHead(t, r, clientAddr, "10.8.0.1")
		for _, rf := range r.Fields {
			switch strings.ToLower(rf.Name) {
			case "content-length", "transfer-encoding", "trailer":
				continue
			}
			sent := false
			for _, sf := range s.fields {
				if strings.EqualFold(sf.Name, rf.Name) && sf.Value == rf.Value {
					sent = true
				}
			}
			if !sent {
				t.Errorf("%s %s: field %s: %q was not sent by the client", r.Method, r.Target, rf.Name, rf.Value)
			}
		}
		for _, rf := range r.Trailer {
			sent := false
			for _, sf := range s.trailer {
				if strings.EqualFold(sf.Name, rf.Name) && sf.Value == rf.Value {
					sent = true
				}
			}
			if !sent {
				t.Errorf("%s %s: trailer field %s: %q was not sent by the client", r.Method, r.Target, rf.Name, rf.Value)
			}
		}
		if s.method == "OPTIONS" || s.method == "TRACE" {
			if mf := r.Values("Max-Forwards"); len(mf) != 1 || mf[0] != "0" {
				t.Errorf("%s: Max-Forwards %q", s.method, mf)
			}
		}
		if !strings.Contains(strings.Join(r.Values("User-Agent"), ""), "nd-client") && i != 3 {
			t.Errorf("request %d lost its User-Agent", i)
		}
	}
	// Go's ReverseProxy forwards the trailer declaration but no request
	// trailer values (Go 1.27.4); the filter leaves only X-Checksum in it.
	if tr := got[6]; tr.Get("Trailer") != "X-Checksum" || len(tr.Trailer) != 0 {
		t.Errorf("trailer request: Trailer %q, trailer fields %v", tr.Get("Trailer"), tr.Trailer)
	}
	if e := got[7]; e.Get("Expect") != "100-continue" {
		t.Errorf("Expect lost: %v", e.Fields)
	}
	if ua := got[3].Values("User-Agent"); len(ua) != 0 {
		t.Errorf("User-Agent added: %q", ua)
	}
	for _, d := range edge.waitDecisions(len(reqs)) {
		checkDecision(t, d, map[string]string{"source": "local", "action": "upstream", "outcome": "ok"})
		if !strings.HasPrefix(d["client"], clientAddr+":") {
			t.Errorf("edge client %q", d["client"])
		}
	}
	for _, d := range gw.waitDecisions(len(reqs)) {
		checkDecision(t, d, map[string]string{"source": "remote", "action": "upstream", "upstream": "egress", "outcome": "ok"})
	}
}
