package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"squid/internal/testenv"
)

// TestServeRemoteListener checks that cuttle serve accepts a non-loopback
// listener, which M1.1 refused, and serves remote clients on it under the
// remote policy, and that it serves upstream routes, which it refused
// before M1.3. The bind function puts each configured address on a
// loopback port that the test chose, and the listener for 10.8.0.6 reports
// its clients as 10.8.0.1, so no real interface is bound (design 0007 D4).
// Requests are blocked by policy or go to a recording upstream at a
// loopback literal, so no name is resolved (design 0007 D4).
func TestServeRemoteListener(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	dir := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const head = `name = "cuttle-gateway"
[proxy]
listen = ["127.0.0.1:18080", "10.8.0.6:18080"]
default = "block"
[admin]
listen = ""
`
	write(head + `[proxy.remote]
sources = ["10.8.0.1/32"]
[[rule]]
name = "restricted"
domains = ["internal.example"]
action = "block"
`)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	var mu sync.Mutex
	bound := map[string]string{}
	listen := func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		mu.Lock()
		bound[address] = ln.Addr().String()
		mu.Unlock()
		if address == "10.8.0.6:18080" {
			return testenv.SourceListener(ln, netip.MustParseAddr("10.8.0.1")), nil
		}
		return ln, nil
	}
	var stderr bytes.Buffer
	// start runs serve until the test sends SIGTERM on the returned channel.
	start := func(s *serveState) (chan<- os.Signal, <-chan int) {
		t.Helper()
		mu.Lock()
		clear(bound)
		mu.Unlock()
		stderr.Reset()
		signals := make(chan os.Signal, 1)
		done := make(chan int, 1)
		go func() { done <- s.run(&stderr, listen, signals) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			n := len(bound)
			mu.Unlock()
			if n == 2 {
				return signals, done
			}
			select {
			case code := <-done:
				t.Fatalf("serve exited with %d: %s", code, stderr.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("listeners not bound")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	signals, done := start(s)

	connect := func(addr, target string) (int, string) {
		t.Helper()
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("CONNECT %s: %v", target, err)
		}
		res.Body.Close()
		return res.StatusCode, res.Header.Get("Proxy-Status")
	}
	mu.Lock()
	remote, local := bound["10.8.0.6:18080"], bound["127.0.0.1:18080"]
	mu.Unlock()
	for _, c := range []struct{ addr, target, rule string }{
		{remote, "wiki.internal.example:443", "restricted"},
		{remote, "example.com:443", "remote-default"},
		{remote, "wiki.internal.example:22", "port"},
		{local, "example.com:443", "default"},
	} {
		code, ps := connect(c.addr, c.target)
		if want := `cuttle-gateway; error=http_request_denied; details="blocked by rule ` + c.rule + `"`; code != 403 || ps != want {
			t.Errorf("%s: %d %q, want 403 %q", c.target, code, ps, want)
		}
	}
	signals <- syscall.SIGTERM
	if code := <-done; code != exitOK {
		t.Errorf("exit status %d after SIGTERM: %s", code, stderr.String())
	}

	log, _ := os.ReadFile(s.paths.Log.Path)
	for _, want := range []string{
		"msg=started",
		"listeners=" + local + "," + remote,
		"source=remote kind=connect target=wiki.internal.example:443 rule=restricted action=block",
		"source=remote kind=connect target=example.com:443 rule=remote-default action=block",
		"source=remote kind=connect target=wiki.internal.example:22 rule=port action=block",
		"source=local kind=connect target=example.com:443 rule=default action=block",
		"msg=stopped",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Count(string(log), "client=10.8.0.1:") != 3 {
		t.Errorf("remote client address not logged three times:\n%s", log)
	}

	// Upstream routes are served from M1.3. The remote default goes to a
	// recording upstream at a loopback literal with disclosure = "none";
	// the upstream refuses names outside cuttle.test with 403.
	rec := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{})
	write(head + `[proxy.remote]
sources = ["10.8.0.1/32"]
default = "upstream:gw"
[[upstream]]
name = "gw"
address = "` + rec.Addr.String() + `"
disclosure = "none"
`)
	s2, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	signals, done = start(s2)
	mu.Lock()
	remote = bound["10.8.0.6:18080"]
	mu.Unlock()
	code, ps := connect(remote, "wiki.internal.example:443")
	if want := `cuttle-gateway; received-status=403; next-hop="gw"`; code != 502 || ps != want {
		t.Errorf("upstream route: %d %q, want 502 %q", code, ps, want)
	}
	reqs := rec.Requests()
	if len(reqs) != 1 || reqs[0].Method != "CONNECT" || reqs[0].Target != "wiki.internal.example:443" || len(reqs[0].Values("Via")) != 0 {
		t.Errorf("upstream received %+v", reqs)
	}
	signals <- syscall.SIGTERM
	if code := <-done; code != exitOK {
		t.Errorf("exit status %d after SIGTERM: %s", code, stderr.String())
	}
	log, _ = os.ReadFile(s2.paths.Log.Path)
	if want := "source=remote kind=connect target=wiki.internal.example:443 rule=remote-default action=upstream upstream=gw interface=\"\" outcome=upstream_status status=502"; !strings.Contains(string(log), want) {
		t.Errorf("log lacks %q:\n%s", want, log)
	}
}
