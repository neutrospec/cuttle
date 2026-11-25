package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"strings"
	"testing"

	"squid/internal/authority"
	"squid/internal/stats"
	"squid/internal/testenv"
)

func TestRemoveDisclosure(t *testing.T) {
	h := http.Header{}
	for k, v := range clientDisclosure {
		h.Set(k, v)
	}
	h["x-forwarded-lowercase"] = []string{"1"} // set directly, not canonical
	h["X-Real-IP"] = []string{"1"}
	h["X_forwarded_for"] = []string{"1"}
	h["x_real_ip"] = []string{"1"}
	h["X-FORWARDED_HOST"] = []string{"1"}
	h["Proxy_Status"] = []string{"1"}
	h["CF_CONNECTING_IP"] = []string{"1"}
	h.Set("X-Forwarded", "kept: not a prefix match")
	h.Set("X-Kept", "yes")
	h.Set("User-Agent", "ua")
	removeDisclosure(h)
	if len(h) != 3 || h.Get("X-Kept") != "yes" || h.Get("User-Agent") != "ua" || h.Get("X-Forwarded") == "" {
		t.Fatalf("after filter: %v", h)
	}
}

func TestSFString(t *testing.T) {
	if got := sfString(`blocked by rule "a\b"` + "\x00é"); got != `"blocked by rule \"a\\b\""` {
		t.Errorf("got %s", got)
	}
}

// TestRewriteDirect checks the raw-target table of design 0002 D5 without
// sockets: the URL fields that make the transport write the client's raw
// target.
func TestRewriteDirect(t *testing.T) {
	for _, c := range []struct {
		target string
		want   string // what URL.RequestURI() yields, as the transport writes it
	}{
		{"http://h/a{b}|c?x=1;y=2&z=%zz", "/a{b}|c?x=1;y=2&z=%zz"},
		{"http://h/q?b=2&a=1;c=3", "/q?b=2&a=1;c=3"},
		{"http://h/p?", "/p?"},
		{"http://h//x", "http://h//x"},
		{"http://h//x?y", "http://h//x?y"},
		{"http://h", "/"},
		{"http://h?q", "/?q"},
		{"http://Up.Example:8080/%2F%41", "/%2F%41"},
	} {
		ht, err := authority.ParseHTTP(c.target)
		if err != nil {
			t.Fatal(err)
		}
		in, err := http.NewRequest("GET", "http://placeholder/", nil)
		if err != nil {
			t.Fatal(err)
		}
		in.Host = ht.RawAuthority
		ex := &exchange{http: ht}
		in = in.WithContext(context.WithValue(in.Context(), exchangeKey{}, ex))
		out := in.Clone(in.Context())
		out.Header.Set("Via", "1.1 lab-gw")
		rewriteDirect(&httputil.ProxyRequest{In: in, Out: out})
		if got := out.URL.RequestURI(); got != c.want {
			t.Errorf("%s: RequestURI %q, want %q", c.target, got, c.want)
		}
		if out.URL.Host != ht.Authority.String() || out.Host != ht.RawAuthority || out.Header.Get("Via") != "" {
			t.Errorf("%s: host %q/%q via %q", c.target, out.URL.Host, out.Host, out.Header.Get("Via"))
		}
	}
}

// idleListener satisfies Deps.Listeners for tests that call ServeHTTP
// directly and never serve.
type idleListener struct{}

func (idleListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (idleListener) Close() error              { return nil }
func (idleListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func newUnitProxy(t *testing.T, n *testenv.Net, logs io.Writer) *Proxy {
	t.Helper()
	p, err := New(mustParse(t, baseConfig), Deps{
		Network:   n,
		SelfAddrs: testenv.SelfAddrs,
		Listeners: []net.Listener{idleListener{}},
		Logger:    newTestLogger(logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRequestWithoutSourceClass checks that a request on a connection that
// did not come through the classifying listener fails closed: it gets 500,
// is never routed as local, causes no lookup or dial, and is logged and
// counted as outcome internal with source unknown.
func TestRequestWithoutSourceClass(t *testing.T) {
	n := testenv.NewNet()
	n.Map("plain.cuttle.test", "203.0.113.10")
	logs := &logBuffer{}
	p := newUnitProxy(t, n, logs)
	for _, req := range []*http.Request{
		httptest.NewRequest("CONNECT", "origin.cuttle.test:443", nil),
		httptest.NewRequest("GET", "http://plain.cuttle.test/", nil),
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Header().Get("Proxy-Status"), "error=proxy_internal_error") {
			t.Errorf("%s: %d %q", req.Method, rec.Code, rec.Header().Get("Proxy-Status"))
		}
	}
	if ev := n.Events(); len(ev) != 0 {
		t.Errorf("network events %v", ev)
	}
	if !strings.Contains(logs.String(), "without a source class") {
		t.Errorf("log %q", logs.String())
	}
	ds := logs.decisions()
	if len(ds) != 2 {
		t.Fatalf("decision lines %v", ds)
	}
	for i, kind := range []string{"connect", "http"} {
		checkDecision(t, ds[i], map[string]string{"kind": kind, "source": "unknown", "target": "", "rule": "", "action": "",
			"outcome": "internal", "status": "500", "error": "proxy_internal_error", "detail": "unknown source", "up": "0", "down": "0"})
	}
	if got := p.Live().Get(stats.OutcomesTotal, "outcome", "internal"); got != 2 {
		t.Errorf("outcomes_total{outcome=internal} = %d", got)
	}
	if got := p.Live().Get(stats.DecisionsTotal, "kind", "connect", "action", "none") + p.Live().Get(stats.DecisionsTotal, "kind", "http", "action", "none"); got != 2 {
		t.Errorf("decisions_total{action=none} = %d", got)
	}
}

// TestWarnedSourcesBounded checks that the rejected-source warnings are rate
// limited per address and that their memory stays bounded.
func TestWarnedSourcesBounded(t *testing.T) {
	logs := &logBuffer{}
	p := newUnitProxy(t, testenv.NewNet(), logs)
	a := netip.MustParseAddr("10.8.0.1")
	p.warnSource(a)
	p.warnSource(a)
	if got := strings.Count(logs.String(), "rejected source"); got != 1 {
		t.Errorf("warnings for one source %d", got)
	}
	for i := range 5000 {
		// 10.20.0.0/16 is the private destination range of the test address
		// plan (design 0007 D3).
		p.warnSource(netip.AddrFrom4([4]byte{10, 20, byte(i >> 8), byte(i)}))
		p.mu.Lock()
		size := len(p.warned)
		p.mu.Unlock()
		if size > maxWarnedSources {
			t.Fatalf("warned map holds %d addresses", size)
		}
	}
}
