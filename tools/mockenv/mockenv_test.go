package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"squid/internal/config"
	"squid/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.CleanupBuild()
	os.Exit(code)
}

// rawProxy sends one raw request to a proxy and returns the response head
// and the connection, for a CONNECT that continues inside the tunnel.
func rawProxy(t *testing.T, addr, req string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	method := strings.Fields(req)[0]
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, br
}

// TestMockenvRefuses407 checks E4: no mode of a mockenv session can answer
// 407, in any spelling; serve refuses such a mode before it starts
// anything; nothing in tools/mockenv enables a mock's 407; and a running
// session's origins and recording upstreams answer a 407 request with 400
// instead.
func TestMockenvRefuses407(t *testing.T) {
	for _, s := range []string{"407", " 407 ", "0407", "status:407", "STATUS:407"} {
		if _, err := parseMode(s); err == nil || !strings.Contains(err.Error(), "407") {
			t.Errorf("parseMode(%q) = %v, want a 407 refusal", s, err)
		}
	}
	for _, s := range []string{"tunnel", "403", "502", "504", "201", "299", "silent", "reset", "buffered"} {
		if _, err := parseMode(s); err != nil {
			t.Errorf("parseMode(%q): %v", s, err)
		}
	}

	t.Run("serve refuses the mode before starting", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "session")
		var stdout, stderr bytes.Buffer
		waited := false
		code := run([]string{"serve", "-dir", dir, "-mode", "x.cuttle.test=407"}, &stdout, &stderr, func() { waited = true })
		if code != 2 || waited || !strings.Contains(stderr.String(), "407 refused") {
			t.Errorf("exit %d, waited %v, stderr %q; want exit 2 with a 407 refusal", code, waited, stderr.String())
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("session directory created: %v", err)
		}
		// Control: another status starts and stops a session.
		code = run([]string{"serve", "-dir", dir, "-mode", "x.cuttle.test=403"}, &stdout, &stderr, func() { waited = true })
		if code != 0 || !waited {
			t.Fatalf("403 mode: exit %d, waited %v: %s", code, waited, stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "session.json")); err != nil {
			t.Error(err)
		}
	})

	t.Run("no source enables 407", func(t *testing.T) {
		files, err := filepath.Glob("*.go")
		if err != nil || len(files) == 0 {
			t.Fatalf("glob: %v %v", files, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("Allow407")) {
				t.Errorf("%s mentions Allow407", f)
			}
		}
	})

	t.Run("a running session answers 400", func(t *testing.T) {
		if testing.Short() {
			t.Skip("starts mocks")
		}
		s, err := startSession(sessionOptions{Dir: filepath.Join(t.TempDir(), "s"),
			Modes: map[string]testenv.Mode{"status.cuttle.test": testenv.Status(407, "")}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		check := func(what string, resp *http.Response) {
			t.Helper()
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Proxy-Authenticate") != "" {
				t.Errorf("%s: %d %v, want 400 without a challenge", what, resp.StatusCode, resp.Header)
			}
		}
		// The origins, directly and through a recording upstream.
		resp, _, _ := rawProxy(t, s.HTTPOrigin, "GET /status?code=407 HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
		check("http origin", resp)
		resp, _, _ = rawProxy(t, s.Mocks[mockEgressDirect], "GET http://plain.cuttle.test/status?code=407 HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
		check("egress-direct to the http origin", resp)
		tc := tls.Client(dial(t, s.HTTPSOrigin), &tls.Config{ServerName: hostOrigin, RootCAs: pool(t, s.OriginCA)})
		io.WriteString(tc, "GET /status?code=407 HTTP/1.1\r\nHost: origin.cuttle.test\r\n\r\n")
		resp, err = http.ReadResponse(bufio.NewReader(tc), nil)
		if err != nil {
			t.Fatal(err)
		}
		check("https origin", resp)
		// A 407 mode set behind mockenv's back still gets 400 from each
		// recording upstream, which mockenv never lets answer 407.
		for _, m := range []string{mockUpstream, mockEgressDirect, mockEgressChain} {
			resp, _, _ := rawProxy(t, s.Mocks[m], "CONNECT status.cuttle.test:443 HTTP/1.1\r\nHost: status.cuttle.test:443\r\n\r\n")
			check(m, resp)
		}
	})
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

func pool(t *testing.T, pemPath string) *x509.CertPool {
	t.Helper()
	data, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatal(err)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(data) {
		t.Fatal("no certificate in " + pemPath)
	}
	return p
}

// TestServeSession checks a session as serve starts it: the configurations
// validate, use default = "block" with rules only for cuttle.test,
// loopback, and the LNA test literals (OI-0006 V4), and choose safe ports;
// the recording upstreams answer 403 outside cuttle.test and record every
// request, fields in received order and the body, as one JSON line; the
// leaves match the printed SPKI hashes; and Close removes the runtime
// directories.
func TestServeSession(t *testing.T) {
	if testing.Short() {
		t.Skip("starts mocks")
	}
	s, err := startSession(sessionOptions{Dir: filepath.Join(t.TempDir(), "s")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	wantRules := map[string][]string{
		"single":  {"loopback", "blocked", "cuttle-test", "test-literals"},
		"edge":    {"cuttle-test"},
		"gateway": {"inspected", "cuttle-test"},
	}
	wantUpstreams := map[string]map[string]string{
		"single":  {"recording": s.Mocks[mockUpstream] + " via"},
		"edge":    {"gateway": s.Instances["gateway"].Listen + " via"},
		"gateway": {"egress": s.Mocks[mockEgressChain] + " none", "inspect": s.Mocks[mockInspect] + " none"},
	}
	for role, in := range s.Instances {
		data, err := os.ReadFile(in.Config)
		if err != nil {
			t.Fatal(err)
		}
		cfg, rep := config.Parse(data, in.Config)
		if err := rep.Err(); err != nil || len(rep.Warnings) > 0 {
			t.Fatalf("%s: %v %v", role, err, rep.Warnings)
		}
		if cfg.Name != in.Name || cfg.Proxy.Default.String() != "block" || cfg.Admin.Listen.IsValid() {
			t.Errorf("%s: name %s, default %s, admin %v", role, cfg.Name, cfg.Proxy.Default, cfg.Admin.Listen)
		}
		if len(cfg.Proxy.Listen) != 1 || cfg.Proxy.Listen[0].String() != in.Listen || !cfg.Proxy.Listen[0].Addr().IsLoopback() ||
			cfg.Proxy.Listen[0].Port() == 3128 || cfg.Proxy.Listen[0].Port() == 3129 {
			t.Errorf("%s: listen %v", role, cfg.Proxy.Listen)
		}
		var rules []string
		for _, r := range cfg.Rules {
			rules = append(rules, r.Name)
			for _, d := range r.Domains {
				if d != "cuttle.test" {
					t.Errorf("%s: rule %s names domain %s", role, r.Name, d)
				}
			}
			for _, h := range r.Hosts {
				if h != "localhost" && !strings.HasSuffix(h, ".cuttle.test") {
					t.Errorf("%s: rule %s names host %s", role, r.Name, h)
				}
			}
			for _, p := range r.CIDRs {
				if !p.Addr().IsLoopback() && p.String() != originAddr+"/32" && p.String() != privateAddr+"/32" {
					t.Errorf("%s: rule %s names %s", role, r.Name, p)
				}
			}
		}
		if !slices.Equal(rules, wantRules[role]) {
			t.Errorf("%s: rules %v, want %v", role, rules, wantRules[role])
		}
		ups := map[string]string{}
		for _, u := range cfg.Upstreams {
			ups[u.Name] = fmt.Sprintf("%s %s", u.Address, u.Disclosure)
		}
		if fmt.Sprint(ups) != fmt.Sprint(wantUpstreams[role]) {
			t.Errorf("%s: upstreams %v, want %v", role, ups, wantUpstreams[role])
		}
		if err := checkIsolation(in, s.Dir); err != nil {
			t.Errorf("%s: %v", role, err)
		}
	}

	// V4: a name outside cuttle.test gets 403 and is recorded; a
	// cuttle.test name is tunnelled.
	resp, _, _ := rawProxy(t, s.Mocks[mockEgressDirect], "CONNECT accounts.example:443 HTTP/1.1\r\nHost: accounts.example:443\r\n\r\n")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT accounts.example: %d, want 403", resp.StatusCode)
	}
	resp, c, br := rawProxy(t, s.Mocks[mockEgressDirect], "CONNECT origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT origin.cuttle.test: %d", resp.StatusCode)
	}
	tc := tls.Client(&bufConn{Conn: c, r: br}, &tls.Config{ServerName: hostOrigin, RootCAs: pool(t, s.OriginCA)})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	leafHash, err := spkiHash(tc.ConnectionState().PeerCertificates[0].PublicKey)
	if err != nil || !slices.Contains(s.SPKI, leafHash) {
		t.Errorf("leaf SPKI %s not among the printed hashes %v (%v)", leafHash, s.SPKI, err)
	}
	// The recording upstream answers with Connection: close after the
	// POST's origin response, so the POST goes on its own connection.
	body := "a=1&b=\x00\xff"
	resp, _, _ = rawProxy(t, s.Mocks[mockEgressDirect], "POST http://plain.cuttle.test/sink HTTP/1.1\r\nHost: plain.cuttle.test\r\nZ-Last: 1\r\nA-First: 2\r\nContent-Length: "+fmt.Sprint(len(body))+"\r\nConnection: close\r\n\r\n"+body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST: %d", resp.StatusCode)
	}
	var recs []record
	deadline := time.Now().Add(5 * time.Second)
	for len(recs) < 3 && time.Now().Before(deadline) {
		recs, err = readRecords(filepath.Join(s.Dir, mockEgressDirect+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(recs) != 3 {
		t.Fatalf("records %+v", recs)
	}
	if recs[0].Method != "CONNECT" || recs[0].Target != "accounts.example:443" || recs[1].Target != "origin.cuttle.test:443" {
		t.Errorf("CONNECT records %+v", recs[:2])
	}
	p := recs[2]
	var names []string
	for _, f := range p.Fields {
		names = append(names, f.Name)
	}
	if p.Method != "POST" || p.Target != "http://plain.cuttle.test/sink" || string(p.Body) != body ||
		!slices.Equal(names, []string{"Host", "Z-Last", "A-First", "Content-Length", "Connection"}) {
		t.Errorf("POST record %+v", p)
	}
	for i, r := range recs {
		if r.Seq != i+1 {
			t.Errorf("record %d has seq %d", i, r.Seq)
		}
	}

	var out bytes.Buffer
	s.print(&out)
	for _, want := range []string{s.OriginCA, s.InspectCA, strings.Join(s.SPKI, ","), s.Instances["edge"].Config,
		"XDG_RUNTIME_DIR=" + s.Instances["gateway"].RuntimeDir, "check-disclosure -dir " + s.Dir} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("printed session lacks %q:\n%s", want, out.String())
		}
	}

	s.Close()
	for role, in := range s.Instances {
		if _, err := os.Stat(in.RuntimeDir); !os.IsNotExist(err) {
			t.Errorf("%s: runtime directory %s remains: %v", role, in.RuntimeDir, err)
		}
	}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// TestM1CheckCurl runs the M1 client check with /usr/bin/curl against cuttle
// binaries, so the procedure itself is exercised in T1, and shows that it
// fails when the gateway discloses toward egress-chain.
func TestM1CheckCurl(t *testing.T) {
	if testing.Short() {
		t.Skip("starts cuttle binaries")
	}
	curl := testenv.CurlPath(t)
	bin := testenv.BuildCuttle(t)
	runCheck := func(t *testing.T, disclosure string) (map[string]result, string) {
		var out bytes.Buffer
		results, err := m1Check(context.Background(), m1Options{Clients: []string{"curl"}, Out: t.TempDir(),
			Cuttle: bin, Curl: curl, gatewayDisclosure: disclosure}, &out)
		if err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		byCase := map[string]result{}
		for _, r := range results {
			byCase[r.Case] = r
		}
		return byCase, out.String()
	}

	t.Run("passes", func(t *testing.T) {
		byCase, out := runCheck(t, "")
		want := map[string]string{
			caseDirectHTTPS: statusPass, caseDirectHTTP: statusPass, caseBlock: statusPass, caseUpstream: statusPass,
			caseWebSocket: statusNotRun, caseChain: statusPass, caseNonDisclosure: statusPass, caseLNA: statusNotRun,
		}
		if len(byCase) != len(want) {
			t.Errorf("results %v", byCase)
		}
		for c, st := range want {
			if byCase[c].Status != st {
				t.Errorf("%s: %s, want %s", c, byCase[c].Status, st)
			}
		}
		if t.Failed() || !strings.Contains(out, "M1 client check: pass") {
			t.Log(out)
		}
	})

	t.Run("fails on a disclosing gateway", func(t *testing.T) {
		byCase, out := runCheck(t, "via")
		r := byCase[caseNonDisclosure]
		notes := strings.Join(r.Notes, "\n")
		if r.Status != statusFail || !strings.Contains(notes, "disclosure field Via") || !strings.Contains(notes, `contains "cuttle-gateway"`) {
			t.Errorf("non-disclosure %s:\n%s", r.Status, notes)
		}
		if byCase[caseChain].Status != statusPass || !strings.Contains(out, "M1 client check: FAIL") {
			t.Errorf("chain %s; output:\n%s", byCase[caseChain].Status, out)
		}
	})
}
