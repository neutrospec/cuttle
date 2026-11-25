package testenv

import (
	"context"
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
	c, err := n.DialContext(ctx, netip.MustParseAddrPort("203.0.113.1:443"))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := n.DialContext(ctx, netip.MustParseAddrPort("203.0.113.2:443")); err == nil {
		t.Error("unrouted dial succeeded")
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := n.DialContext(tctx, netip.MustParseAddrPort("203.0.113.9:443")); err == nil || time.Since(start) < 40*time.Millisecond {
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
