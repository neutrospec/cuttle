package testenv

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"squid/internal/netx"
)

// TestBinaryHelperIsolation checks E6 without starting a process: the
// sandbox refuses to start cuttle unless its directories are test-owned
// temporary directories and, for serve, the listen addresses are safe.
func TestBinaryHelperIsolation(t *testing.T) {
	fresh := func(t *testing.T) *Sandbox {
		s := NewSandbox(t, "/nonexistent/cuttle")
		if err := os.MkdirAll(filepath.Dir(s.ConfigPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		return s
	}
	write := func(t *testing.T, s *Sandbox, text string) {
		if err := os.WriteFile(s.ConfigPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	good := "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:41000\"]\ndefault = \"block\"\n[admin]\nlisten = \"\"\n"

	t.Run("isolated sandbox", func(t *testing.T) {
		s := fresh(t)
		write(t, s, good)
		for _, args := range [][]string{{"serve"}, {"check"}, {"init"}, {"paths"}, {"explain", "example.com:443"}} {
			if err := s.Check(args); err != nil {
				t.Errorf("%v: %v", args, err)
			}
		}
		for _, kv := range s.Env() {
			if strings.HasPrefix(kv, "HOME=") && os.Getenv("HOME") != "" && kv == "HOME="+os.Getenv("HOME") {
				t.Errorf("sandbox uses the real HOME")
			}
		}
	})

	dirCases := map[string]func(s *Sandbox){
		"HOME is the real home":       func(s *Sandbox) { s.Home = os.Getenv("HOME") },
		"HOME relative":               func(s *Sandbox) { s.Home = "home" },
		"XDG_CONFIG_HOME outside":     func(s *Sandbox) { s.ConfigHome = filepath.Join(os.Getenv("HOME"), ".config") },
		"XDG_DATA_HOME outside":       func(s *Sandbox) { s.DataHome = "/usr/local/share" },
		"XDG_STATE_HOME outside":      func(s *Sandbox) { s.StateHome = filepath.Join(os.Getenv("HOME"), ".local", "state") },
		"TMPDIR outside":              func(s *Sandbox) { s.TmpDir = os.TempDir() },
		"root outside temporary dirs": func(s *Sandbox) { s.Root = "/usr" },
		"runtime dir not under /tmp":  func(s *Sandbox) { s.RuntimeDir = s.TmpDir },
		"runtime dir not cuttle-*": func(s *Sandbox) {
			d, _ := os.MkdirTemp("/tmp", "other-")
			os.Chmod(d, 0o700)
			s.RuntimeDir = d
			s.t.Cleanup(func() { os.RemoveAll(d) })
		},
		"runtime dir 0755": func(s *Sandbox) { os.Chmod(s.RuntimeDir, 0o755) },
	}
	for name, mutate := range dirCases {
		t.Run(name, func(t *testing.T) {
			s := fresh(t)
			write(t, s, good)
			mutate(s)
			if err := s.Check([]string{"check"}); err == nil {
				t.Errorf("accepted")
			}
		})
	}

	t.Run("environment overrides", func(t *testing.T) {
		s := fresh(t)
		inside := filepath.Join(s.Root, "alt-state")
		if err := s.check([]string{"status"}, map[string]string{"XDG_STATE_HOME": inside}); err != nil {
			t.Errorf("override inside the sandbox refused: %v", err)
		}
		for name, over := range map[string]map[string]string{
			"state outside":       {"XDG_STATE_HOME": filepath.Join(os.Getenv("HOME"), ".local", "state")},
			"config outside":      {"XDG_CONFIG_HOME": "/usr/local/etc"},
			"HOME not allowed":    {"HOME": inside},
			"runtime not allowed": {"XDG_RUNTIME_DIR": inside},
		} {
			if err := s.check([]string{"status"}, over); err == nil {
				t.Errorf("%s: accepted %v", name, over)
			}
		}
	})

	t.Run("config outside the sandbox", func(t *testing.T) {
		s := fresh(t)
		if err := s.Check([]string{"check", "--config", filepath.Join(os.Getenv("HOME"), ".config", "cuttle", "config.toml")}); err == nil {
			t.Error("accepted --config outside")
		}
		if err := s.Check([]string{"check", "--config=/etc/cuttle.toml"}); err == nil {
			t.Error("accepted --config= outside")
		}
	})

	serveCases := map[string]string{
		"no configuration": "",
		"listen unset":     "name = \"cuttle-test\"\n[proxy]\ndefault = \"block\"\n[admin]\nlisten = \"\"\n",
		"listen 3128":      "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:3128\"]\ndefault = \"block\"\n[admin]\nlisten = \"\"\n",
		"listen 3129":      "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:41000\", \"[::1]:3129\"]\ndefault = \"block\"\n[admin]\nlisten = \"\"\n",
		"admin unset":      "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:41000\"]\ndefault = \"block\"\n",
		"admin unchosen":   "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:41000\"]\ndefault = \"block\"\n[admin]\nlisten = \"127.0.0.1:41001\"\n",
		"admin 3129":       "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:41000\"]\ndefault = \"block\"\n[admin]\nlisten = \"127.0.0.1:3129\"\n",
	}
	for name, text := range serveCases {
		t.Run("serve "+name, func(t *testing.T) {
			s := fresh(t)
			if text != "" {
				write(t, s, text)
			}
			s.MarkChosen(3129) // even a chosen 3129 is refused
			if err := s.Check([]string{"serve"}); err == nil {
				t.Errorf("accepted")
			}
		})
	}

	t.Run("admin on a chosen port", func(t *testing.T) {
		s := fresh(t)
		write(t, s, "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:41000\"]\ndefault = \"block\"\n[admin]\nlisten = \"127.0.0.1:41001\"\n")
		s.MarkChosen(41001)
		if err := s.Check([]string{"serve"}); err != nil {
			t.Error(err)
		}
	})
}

// TestCALeaves checks the generated leaves (part of E1): a SAN, the
// serverAuth EKU, and a validity far below 825 days.
func TestCALeaves(t *testing.T) {
	ca := NewCA(t, "root")
	leaf := ca.Leaf(t, "*.cuttle.test", "127.0.0.1")
	l := leaf.Leaf
	if len(l.DNSNames) != 1 || len(l.IPAddresses) != 1 {
		t.Errorf("SANs %v %v", l.DNSNames, l.IPAddresses)
	}
	if len(l.ExtKeyUsage) != 1 || l.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("EKU %v", l.ExtKeyUsage)
	}
	if d := l.NotAfter.Sub(l.NotBefore); d > 825*24*time.Hour || d < LeafValidity {
		t.Errorf("validity %v", d)
	}
	if _, err := l.Verify(x509.VerifyOptions{DNSName: "origin.cuttle.test", Roots: ca.Pool()}); err != nil {
		t.Errorf("verify: %v", err)
	}
	if !strings.Contains(string(ca.PEM), "BEGIN CERTIFICATE") || strings.Contains(string(ca.PEM), "PRIVATE") {
		t.Errorf("PEM holds more than the certificate")
	}
}

// TestNetRecords checks part of E1: testenv.Net answers from its map,
// routes dials, refuses unrouted addresses, blackholes, and records every
// lookup and dial in order.
func TestNetRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("uses sockets")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	n := NewNet()
	n.Map("a.cuttle.test", "203.0.113.1", "2001:db8::1")
	n.Route("203.0.113.1:443", ln.Addr().(*net.TCPAddr).AddrPort())
	n.Blackhole("203.0.113.9")
	ctx := context.Background()

	addrs, err := n.LookupNetIP(ctx, "a.cuttle.test")
	if err != nil || len(addrs) != 2 {
		t.Fatalf("lookup %v %v", addrs, err)
	}
	var de *net.DNSError
	if _, err := n.LookupNetIP(ctx, "unknown.cuttle.test"); !errors.As(err, &de) || !de.IsNotFound {
		t.Errorf("unknown: %v", err)
	}
	c, err := n.DialContext(ctx, netip.MustParseAddrPort("203.0.113.1:443"), netx.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := n.DialContext(ctx, netip.MustParseAddrPort("203.0.113.2:443"), netx.Binding{}); err == nil {
		t.Error("unrouted dial succeeded")
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := n.DialContext(tctx, netip.MustParseAddrPort("203.0.113.9:443"), netx.Binding{}); err == nil || time.Since(start) < 40*time.Millisecond {
		t.Errorf("blackhole: %v after %v", err, time.Since(start))
	}
	var got []string
	for _, e := range n.Events() {
		got = append(got, e.String())
	}
	want := "lookup a.cuttle.test,lookup unknown.cuttle.test,dial 203.0.113.1:443,dial 203.0.113.2:443,dial 203.0.113.9:443"
	if strings.Join(got, ",") != want {
		t.Errorf("events %v", got)
	}
}

// TestOriginStatus checks /status: it answers the chosen status, and 407
// only when the test enables it (design 0007 D6).
func TestOriginStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("uses sockets")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	get := func(o *Origin, path string) (int, string, string) {
		res, err := client.Get("http://" + o.Addr.String() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Proxy-Authenticate"), string(b)
	}
	safe := NewOrigin(t, OriginOptions{})
	if code, pa, body := get(safe, "/status?code=407"); code != 400 || pa != "" || !strings.Contains(body, "407 refused") {
		t.Errorf("default origin: %d %q %q", code, pa, body)
	}
	if code, _, _ := get(safe, "/status?code=503"); code != 503 {
		t.Errorf("503: %d", code)
	}
	if code, _, _ := get(safe, "/status?code=101"); code != 400 {
		t.Errorf("101: %d", code)
	}
	allowed := NewOrigin(t, OriginOptions{Allow407: true})
	if code, pa, _ := get(allowed, "/status?code=407"); code != 407 || pa == "" {
		t.Errorf("Allow407 origin: %d %q", code, pa)
	}
	if reqs := safe.Requests(); len(reqs) != 3 || reqs[0].URI != "/status?code=407" {
		t.Errorf("recorded %+v", reqs)
	}
}

// TestRecordingUpstream checks part of E1 and OI-0006 V4: the recording
// upstream records each request head and body byte for byte, per
// connection and in order; passes absolute-form requests to the origin
// unchanged; answers 403 for names outside cuttle.test; and refuses 407
// unless the test allows it.
func TestRecordingUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("uses sockets")
	}
	origin := NewOrigin(t, OriginOptions{})
	n := NewNet()
	n.Map("a.cuttle.test", "203.0.113.10")
	n.Route("203.0.113.10:80", origin.Addr)
	u := NewRecordingUpstream(t, UpstreamOptions{Net: n})
	u.SetMode("s407.cuttle.test", Status(407, ""))

	exchange := func(c net.Conn, br *bufio.Reader, req string) *http.Response {
		t.Helper()
		if _, err := io.WriteString(c, req); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	dial := func() (net.Conn, *bufio.Reader) {
		c, err := net.DialTimeout("tcp", u.Addr.String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		c.SetDeadline(time.Now().Add(10 * time.Second))
		return c, bufio.NewReader(c)
	}

	get := "GET http://a.cuttle.test/x?q=%zz HTTP/1.1\r\nHost: a.cuttle.test\r\nx-lower:  One \r\nX-Two: 2\r\n\r\n"
	post := "POST http://a.cuttle.test/y HTTP/1.1\r\nHost: a.cuttle.test\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2;ext=1\r\nde\r\n0\r\nX-T: 1\r\n\r\n"
	c, br := dial()
	if res := exchange(c, br, get); res.StatusCode != 200 {
		t.Errorf("GET status %d", res.StatusCode)
	}
	if res := exchange(c, br, post); res.StatusCode != 200 {
		t.Errorf("POST status %d", res.StatusCode)
	}
	// Informational responses are relayed before the final one: 100
	// Continue for Expect, and 103 Early Hints (TASK-0006 U-R5).
	c4, br4 := dial()
	if res := exchange(c4, br4, "POST http://a.cuttle.test/expect HTTP/1.1\r\nHost: a.cuttle.test\r\nExpect: 100-continue\r\n"+
		"Content-Length: 5\r\n\r\nhello"); res.StatusCode != 100 {
		t.Errorf("Expect: first response %d, want 100", res.StatusCode)
	}
	if res, err := http.ReadResponse(br4, nil); err != nil || res.StatusCode != 200 {
		t.Errorf("Expect: final response %v %v", res, err)
	} else {
		io.Copy(io.Discard, res.Body)
	}
	if res := exchange(c4, br4, "GET http://a.cuttle.test/hints HTTP/1.1\r\nHost: a.cuttle.test\r\n\r\n"); res.StatusCode != 103 ||
		res.Header.Get("Link") == "" {
		t.Errorf("hints: first response %d %v, want 103", res.StatusCode, res.Header)
	}
	if res, err := http.ReadResponse(br4, nil); err != nil || res.StatusCode != 200 {
		t.Errorf("hints: final response %v %v", res, err)
	} else if b, _ := io.ReadAll(res.Body); string(b) != "hints" {
		t.Errorf("hints body %q", b)
	}

	c2, br2 := dial()
	if res := exchange(c2, br2, "CONNECT outside.example:443 HTTP/1.1\r\nHost: outside.example:443\r\n\r\n"); res.StatusCode != 403 {
		t.Errorf("name outside cuttle.test: %d", res.StatusCode)
	}
	c3, br3 := dial()
	if res := exchange(c3, br3, "GET http://s407.cuttle.test/ HTTP/1.1\r\nHost: s407.cuttle.test\r\n\r\n"); res.StatusCode != 400 ||
		res.Header.Get("Proxy-Authenticate") != "" {
		t.Errorf("407 without Allow407: %d %v", res.StatusCode, res.Header)
	}

	reqs := u.Requests()
	if len(reqs) != 6 {
		t.Fatalf("%d requests", len(reqs))
	}
	g, p := reqs[0], reqs[1]
	if g.Conn != 1 || p.Conn != 1 || reqs[2].Conn != 2 || reqs[3].Conn != 2 || reqs[4].Conn != 3 || reqs[5].Conn != 4 {
		t.Errorf("connections %d %d %d %d %d %d", g.Conn, p.Conn, reqs[2].Conn, reqs[3].Conn, reqs[4].Conn, reqs[5].Conn)
	}
	if e := reqs[2]; e.Get("Expect") != "100-continue" || string(e.Body) != "hello" {
		t.Errorf("Expect request recorded %v %q", e.Fields, e.Body)
	}
	if string(g.Head) != get || g.Target != "http://a.cuttle.test/x?q=%zz" || g.Fields[1] != (Field{"x-lower", "One"}) {
		t.Errorf("GET recorded %q %+v", g.Head, g.Fields)
	}
	if string(p.Head)+string(p.RawBody) != post || string(p.Body) != "abcde" || len(p.Trailer) != 1 || p.Trailer[0] != (Field{"X-T", "1"}) {
		t.Errorf("POST recorded %q %q %q %v", p.Head, p.RawBody, p.Body, p.Trailer)
	}
	if string(u.Raw(1)) != get+post {
		t.Errorf("raw bytes of connection 1: %q", u.Raw(1))
	}
	o := origin.Requests()
	if len(o) != 4 || o[0].URI != "http://a.cuttle.test/x?q=%zz" || o[0].Header.Get("X-Lower") != "One" || string(o[1].Body) != "abcde" {
		t.Errorf("origin received %+v", o)
	}
}

// TestInspectingLeaves checks part of E1: the inspecting egress mock's
// leaves have a SAN, the serverAuth EKU, and at most 825 days of validity;
// it decrypts the tunnelled request and refuses listed names.
func TestInspectingLeaves(t *testing.T) {
	if testing.Short() {
		t.Skip("uses sockets")
	}
	originCA := NewCA(t, "origin root")
	inspection := NewCA(t, "inspection root")
	origin := NewOrigin(t, OriginOptions{TLS: originCA})
	n := NewNet()
	n.Map("origin.cuttle.test", "203.0.113.10")
	n.Route("203.0.113.10:443", origin.Addr)
	p := NewInspectingProxy(t, InspectingOptions{Root: inspection, Origins: originCA.Pool(), Net: n, Refuse: []string{"refused.cuttle.test"}})

	c, err := net.DialTimeout("tcp", p.Addr.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n")
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", res, err)
	}
	tc := tls.Client(c, &tls.Config{ServerName: "origin.cuttle.test", RootCAs: inspection.Pool()})
	io.WriteString(tc, "GET /e HTTP/1.1\r\nHost: origin.cuttle.test\r\n\r\n")
	res, err = http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("through the mock: %v %v", res, err)
	}
	l := tc.ConnectionState().PeerCertificates[0]
	if len(l.DNSNames) != 1 || l.DNSNames[0] != "origin.cuttle.test" {
		t.Errorf("SAN %v", l.DNSNames)
	}
	if len(l.ExtKeyUsage) != 1 || l.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("EKU %v", l.ExtKeyUsage)
	}
	if d := l.NotAfter.Sub(l.NotBefore); d > 825*24*time.Hour {
		t.Errorf("validity %v", d)
	}
	if l.Issuer.CommonName != "inspection root" || p.Leaves()["origin.cuttle.test"] == nil {
		t.Errorf("issuer %q", l.Issuer.CommonName)
	}
	if d := p.Decrypted(); len(d) != 1 || d[0].URI != "/e" || d[0].Host != "origin.cuttle.test" {
		t.Errorf("decrypted %+v", d)
	}

	c2, err := net.DialTimeout("tcp", p.Addr.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c2, "CONNECT refused.cuttle.test:443 HTTP/1.1\r\nHost: refused.cuttle.test:443\r\n\r\n")
	if res, err := http.ReadResponse(bufio.NewReader(c2), nil); err != nil || res.StatusCode != 403 {
		t.Errorf("refused name: %v %v", res, err)
	}
	if h := p.Requests(); len(h) != 2 || h[1].Target != "refused.cuttle.test:443" {
		t.Errorf("heads %+v", h)
	}
}
