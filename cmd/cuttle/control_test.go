package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"squid/internal/config"
	"squid/internal/control"
	"squid/internal/paths"
	"squid/internal/proxy"
	"squid/internal/testenv"
)

// serveConfig is a configuration for binary tests: listeners on chosen
// ports, no admin listener, a block default, and the starter's loopback
// rule, so requests to loopback mocks go direct without any lookup (design
// 0007 D4).
func serveConfig(listen []string, extra string) string {
	return fmt.Sprintf(`name = "cuttle-test"

[proxy]
listen = [%s]
default = "block"

[admin]
listen = ""

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true
%s`, `"`+strings.Join(listen, `", "`)+`"`, extra)
}

// writeConfig writes the sandbox's configuration file.
func writeConfig(t *testing.T, sb *testenv.Sandbox, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sb.ConfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sb.ConfigPath(), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// statusJSON runs cuttle status --json and decodes it.
func statusJSON(t *testing.T, sb *testenv.Sandbox) *control.Status {
	t.Helper()
	r := sb.Run(10*time.Second, "status", "--json")
	if r.Code != 0 {
		t.Fatalf("status --json: exit %d: %s", r.Code, r.Stderr)
	}
	var st control.Status
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, r.Stdout)
	}
	return &st
}

// proxyClient returns a Go client that uses the proxy at addr.
func proxyClient(t *testing.T, addr string) *http.Client {
	t.Helper()
	tr := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: addr}), DisableCompression: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

// getStatus sends a GET through the proxy and returns the status code.
func getStatus(t *testing.T, c *http.Client, u string) int {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

// waitLog waits until the log contains want.
func waitLog(t *testing.T, sb *testenv.Sandbox, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(sb.LogPath())
		if strings.Contains(string(data), want) {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", want, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestControlSocket checks P4: cuttle status and cuttle reload work over
// the socket, which has mode 0600 in a 0700 directory; a second cuttle
// serve exits with status 1; a stale socket is replaced; and a 0755 socket
// directory leaves the service running without a control socket and logs
// a warning.
func TestControlSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr := testenv.Port(sb.FreePort())
	writeConfig(t, sb, serveConfig([]string{addr}, ""))
	proc := sb.Start(addr, "serve")
	sock := filepath.Join(sb.RuntimeDir, "cuttle", "control.sock")

	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %s: %v %v", sock, fi.Mode(), err)
	}
	if di, _ := os.Stat(filepath.Dir(sock)); di.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode %v", di.Mode())
	}

	r := sb.Run(10*time.Second, "status")
	for _, want := range []string{"cuttle-test", addr, "bound", "tunnels (0)", "rotation failures 0", "\nadmin       disabled\n"} {
		if r.Code != 0 || !strings.Contains(r.Stdout, want) {
			t.Errorf("status: exit %d, lacks %q:\n%s%s", r.Code, want, r.Stdout, r.Stderr)
		}
	}
	data, _ := os.ReadFile(sb.ConfigPath())
	st := statusJSON(t, sb)
	if st.Name != "cuttle-test" || st.Version != "0.0.0-test" || st.PID <= 0 || st.Config.Path != sb.ConfigPath() ||
		st.Config.SHA256 != config.Digest(data) || st.Stats != "enabled" || st.RlimitNofile == 0 ||
		len(st.Listeners) != 1 || st.Listeners[0].Address != addr || st.Listeners[0].State != "bound" ||
		st.Admin.State != control.AdminDisabled || st.Admin.Address != "" {
		t.Errorf("status %+v", st)
	}
	if r := sb.Run(10*time.Second, "reload"); r.Code != 0 || !strings.Contains(r.Stdout, "reloaded") {
		t.Errorf("reload: exit %d %q %q", r.Code, r.Stdout, r.Stderr)
	}
	waitLog(t, sb, "msg=reloaded")

	// A second instance finds the socket in use and exits before binding.
	addr2 := testenv.Port(sb.FreePort())
	second := filepath.Join(filepath.Dir(sb.ConfigPath()), "second.toml")
	os.WriteFile(second, []byte(serveConfig([]string{addr2}, "")), 0o600)
	r = sb.Run(10*time.Second, "serve", "--config", second)
	if r.Code != 1 || !strings.Contains(r.Stderr, "another cuttle instance is running") {
		t.Errorf("second serve: exit %d %q", r.Code, r.Stderr)
	}
	if c, err := net.DialTimeout("tcp", addr2, time.Second); err == nil {
		c.Close()
		t.Errorf("second instance bound %s", addr2)
	}
	if r := sb.Run(10*time.Second, "status"); r.Code != 0 {
		t.Errorf("first instance's socket gone after the second serve: %q", r.Stderr)
	}
	if code := proc.Stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Lstat(sock); err == nil {
		t.Errorf("socket left after a clean stop")
	}

	// A stale socket, left without a listener, is replaced.
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ul.(*net.UnixListener).SetUnlinkOnClose(false)
	ul.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("stale socket not left: %v", err)
	}
	proc = sb.Start(addr, "serve")
	if r := sb.Run(10*time.Second, "status"); r.Code != 0 || !strings.Contains(r.Stdout, "cuttle-test") {
		t.Errorf("status after replacing a stale socket: exit %d %q", r.Code, r.Stderr)
	}
	if code := proc.Stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}

	// A socket directory with mode 0755 leaves the service without a socket.
	sb2 := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr3 := testenv.Port(sb2.FreePort())
	writeConfig(t, sb2, serveConfig([]string{addr3}, ""))
	if err := os.Mkdir(filepath.Join(sb2.RuntimeDir, "cuttle"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(sb2.RuntimeDir, "cuttle"), 0o755)
	proc = sb2.Start(addr3, "serve")
	log := waitLog(t, sb2, `msg="control socket unavailable; running without it"`)
	if !strings.Contains(log, "not 0700") {
		t.Errorf("warning does not name the mode:\n%s", log)
	}
	// The client applies the same directory check before it dials.
	for _, cmd := range []string{"status", "reload"} {
		if r := sb2.Run(10*time.Second, cmd); r.Code != 1 || !strings.Contains(r.Stderr, "refusing the control socket") ||
			!strings.Contains(r.Stderr, "not 0700") {
			t.Errorf("%s with a 0755 socket directory: exit %d %q", cmd, r.Code, r.Stderr)
		}
	}
	c := proxyClient(t, addr3)
	if code := getStatus(t, c, "http://blocked.cuttle.test/"); code != http.StatusForbidden {
		t.Errorf("service without a socket answered %d", code)
	}
	if code := proc.Stop(); code != 0 {
		t.Errorf("exit %d", code)
	}
}

// TestPathsMatchService checks P2: cuttle paths --json equals the paths of
// /v1/status for the same environment, and with a different
// XDG_STATE_HOME cuttle status warns about the mismatch.
func TestPathsMatchService(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr := testenv.Port(sb.FreePort())
	writeConfig(t, sb, serveConfig([]string{addr}, ""))
	sb.Start(addr, "serve")

	r := sb.Run(10*time.Second, "paths", "--json")
	var own paths.Paths
	if err := json.Unmarshal([]byte(r.Stdout), &own); r.Code != 0 || err != nil {
		t.Fatalf("paths --json: %d %v", r.Code, err)
	}
	st := statusJSON(t, sb)
	if !reflect.DeepEqual(own, st.Paths) {
		t.Errorf("paths differ:\ncli     %+v\nservice %+v", own, st.Paths)
	}
	if r := sb.Run(10*time.Second, "status"); r.Code != 0 || r.Stderr != "" {
		t.Errorf("status with the same environment: exit %d, stderr %q", r.Code, r.Stderr)
	}

	alt := filepath.Join(sb.Root, "alt-state")
	r = sb.RunEnv(10*time.Second, map[string]string{"XDG_STATE_HOME": alt}, "status")
	if r.Code != 0 {
		t.Fatalf("status with another XDG_STATE_HOME: exit %d %q", r.Code, r.Stderr)
	}
	for _, name := range []string{"log", "stats"} {
		want := fmt.Sprintf("cuttle: warning: the service's %s path is %s", name, filepath.Join(sb.StateHome, "cuttle"))
		if !strings.Contains(r.Stderr, want) || !strings.Contains(r.Stderr, filepath.Join(alt, "cuttle")) {
			t.Errorf("no %s mismatch warning:\n%s", name, r.Stderr)
		}
	}
	if strings.Contains(r.Stderr, "config path") || strings.Contains(r.Stderr, "control path") {
		t.Errorf("warned about paths that match:\n%s", r.Stderr)
	}
}

// TestReloadInvalid checks K4 over the control socket: reloading an invalid
// file keeps the previous policy, logs the errors, and returns them
// through cuttle reload with exit status 1; a valid file then applies.
func TestReloadInvalid(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	target := "http://" + origin.Addr.String() + "/echo"
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr := testenv.Port(sb.FreePort())
	good := serveConfig([]string{addr}, "")
	writeConfig(t, sb, good)
	sb.Start(addr, "serve")
	c := proxyClient(t, addr)
	if code := getStatus(t, c, target); code != 200 {
		t.Fatalf("before reload: %d", code)
	}
	sum := statusJSON(t, sb).Config.SHA256

	writeConfig(t, sb, strings.Replace(good, `default = "block"`, `default = "deny"`, 1))
	r := sb.Run(10*time.Second, "reload")
	wantErr := "error: " + sb.ConfigPath() + ":5: proxy.default: invalid action"
	if r.Code != 1 || !strings.Contains(r.Stderr, wantErr) {
		t.Errorf("invalid reload: exit %d, stderr %q, want %q", r.Code, r.Stderr, wantErr)
	}
	if code := getStatus(t, c, target); code != 200 {
		t.Errorf("previous policy not kept: %d", code)
	}
	if code := getStatus(t, c, "http://blocked.cuttle.test/"); code != 403 {
		t.Errorf("previous default not kept: %d", code)
	}
	log := waitLog(t, sb, `msg="reload failed; keeping the running configuration"`)
	if !strings.Contains(log, `msg="configuration error" error="`+sb.ConfigPath()+`:5: proxy.default: invalid action`) {
		t.Errorf("errors not logged:\n%s", log)
	}
	st := statusJSON(t, sb)
	if st.Config.SHA256 != sum || !strings.Contains(st.Config.LastReloadError, "proxy.default: invalid action") {
		t.Errorf("config state after a refused reload: %+v", st.Config)
	}

	// A valid file applies, and the reload error clears.
	writeConfig(t, sb, good[:strings.Index(good, "[[rule]]")])
	if r := sb.Run(10*time.Second, "reload"); r.Code != 0 {
		t.Fatalf("valid reload: exit %d %q", r.Code, r.Stderr)
	}
	if code := getStatus(t, c, target); code != 403 {
		t.Errorf("new policy not applied: %d", code)
	}
	if st := statusJSON(t, sb); st.Config.LastReloadError != "" || st.Config.SHA256 == sum {
		t.Errorf("config state after a reload: %+v", st.Config)
	}
}

// TestReloadSIGHUP checks the SIGHUP half of K4: an invalid file keeps the
// previous policy and logs the errors, and a valid file applies.
func TestReloadSIGHUP(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	target := "http://" + origin.Addr.String() + "/echo"
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr := testenv.Port(sb.FreePort())
	good := serveConfig([]string{addr}, "")
	writeConfig(t, sb, good)
	proc := sb.Start(addr, "serve")
	c := proxyClient(t, addr)
	if code := getStatus(t, c, target); code != 200 {
		t.Fatalf("before reload: %d", code)
	}

	writeConfig(t, sb, strings.Replace(good, `default = "block"`, `default = "deny"`, 1))
	proc.Signal(syscall.SIGHUP)
	log := waitLog(t, sb, `msg="reload failed; keeping the running configuration"`)
	if !strings.Contains(log, "proxy.default: invalid action") {
		t.Errorf("errors not logged:\n%s", log)
	}
	if code := getStatus(t, c, target); code != 200 {
		t.Errorf("previous policy not kept: %d", code)
	}

	writeConfig(t, sb, good[:strings.Index(good, "[[rule]]")])
	proc.Signal(syscall.SIGHUP)
	waitLog(t, sb, "msg=reloaded")
	if code := getStatus(t, c, target); code != 403 {
		t.Errorf("new policy not applied: %d", code)
	}
	if code := proc.Stop(); code != 0 {
		t.Errorf("exit %d", code)
	}
}

// TestReloadRestartRequired checks K5: changing proxy.listen and reloading
// leaves the listeners unchanged and lists proxy.listen under
// restart_required.
func TestReloadRestartRequired(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	a, b := testenv.Port(sb.FreePort()), testenv.Port(sb.FreePort())
	writeConfig(t, sb, serveConfig([]string{a}, ""))
	sb.Start(a, "serve")

	writeConfig(t, sb, serveConfig([]string{b}, ""))
	r := sb.Run(10*time.Second, "reload")
	if r.Code != 0 || !strings.Contains(r.Stdout, "restart required: proxy.listen\n") {
		t.Errorf("reload: exit %d %q %q", r.Code, r.Stdout, r.Stderr)
	}
	if code := getStatus(t, proxyClient(t, a), "http://blocked.cuttle.test/"); code != 403 {
		t.Errorf("old listener answered %d", code)
	}
	if c, err := net.DialTimeout("tcp", b, time.Second); err == nil {
		c.Close()
		t.Errorf("reload bound %s", b)
	}
	st := statusJSON(t, sb)
	if !slices.Equal(st.Config.RestartRequired, []string{"proxy.listen"}) ||
		len(st.Listeners) != 1 || st.Listeners[0].Address != a || st.Listeners[0].State != "bound" {
		t.Errorf("status: restart_required %v listeners %+v", st.Config.RestartRequired, st.Listeners)
	}
	if r := sb.Run(10*time.Second, "status"); !strings.Contains(r.Stdout, "restart required: proxy.listen") {
		t.Errorf("text status lacks restart required:\n%s", r.Stdout)
	}
}

// TestListenerBindRetry checks N7: a non-loopback listener on an address
// absent from the host (192.0.2.1) shows retrying in cuttle status while
// the loopback listener serves. A loopback listener that cannot bind still
// stops startup with exit status 1. In-process, a retried listener that
// binds later serves remote clients (design 0003 D9).
func TestListenerBindRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	local, local2 := testenv.Port(sb.FreePort()), testenv.Port(sb.FreePort())
	absent := fmt.Sprintf("192.0.2.1:%d", sb.FreePort())
	// The retried listener sits between two bound ones in proxy.listen, and
	// status keeps that order.
	writeConfig(t, sb, serveConfig([]string{local, absent, local2}, "\n[proxy.remote]\nsources = [\"10.8.0.1/32\"]\n"))
	proc := sb.Start(local, "serve")
	if code := getStatus(t, proxyClient(t, local), "http://blocked.cuttle.test/"); code != 403 {
		t.Errorf("loopback listener answered %d", code)
	}
	st := statusJSON(t, sb)
	want := map[string]string{local: "bound", absent: "retrying", local2: "bound"}
	for _, l := range st.Listeners {
		if want[l.Address] != l.State {
			t.Errorf("listener %+v", l)
		}
		delete(want, l.Address)
		if l.State == "retrying" && !strings.Contains(l.Error, "assign requested address") {
			t.Errorf("retrying listener error %q", l.Error)
		}
	}
	if len(want) != 0 {
		t.Errorf("listeners missing: %v (have %+v)", want, st.Listeners)
	}
	if len(st.Listeners) != 3 || st.Listeners[0].Address != local || st.Listeners[1].Address != absent ||
		st.Listeners[2].Address != local2 {
		t.Errorf("listeners not in configuration order: %+v", st.Listeners)
	}
	if r := sb.Run(10*time.Second, "status"); !strings.Contains(r.Stdout, absent) || !strings.Contains(r.Stdout, "retrying") {
		t.Errorf("text status:\n%s", r.Stdout)
	}
	waitLog(t, sb, `msg="listener retrying" address=`+absent)
	if code := proc.Stop(); code != 0 {
		t.Errorf("exit %d", code)
	}

	// A loopback listener that cannot bind stops startup.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	busy := held.Addr().String()
	sb.MarkChosen(uint16(held.Addr().(*net.TCPAddr).Port))
	writeConfig(t, sb, serveConfig([]string{busy}, ""))
	if r := sb.Run(10*time.Second, "serve"); r.Code != 1 || !strings.Contains(r.Stderr, "bind "+busy) {
		t.Errorf("busy loopback listener: exit %d %q", r.Code, r.Stderr)
	}

	// In-process: the retried listener binds on the third attempt and then
	// serves remote clients. Each configured address goes to a loopback port,
	// and the one for 10.8.0.6 reports its clients as 10.8.0.1.
	lookup, vars := tempEnv(t)
	os.MkdirAll(filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle"), 0o700)
	os.WriteFile(filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml"),
		[]byte(serveConfig([]string{"127.0.0.1:18080", "10.8.0.6:18080"}, "\n[proxy.remote]\nsources = [\"10.8.0.1/32\"]\n")), 0o600)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.retryInterval = 50 * time.Millisecond
	var mu sync.Mutex
	attempts, bound := 0, map[string]string{}
	listen := func(network, address string) (net.Listener, error) {
		mu.Lock()
		defer mu.Unlock()
		if address == "10.8.0.6:18080" {
			if attempts++; attempts < 3 {
				return nil, &net.OpError{Op: "listen", Net: network, Err: syscall.EADDRNOTAVAIL}
			}
		}
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		bound[address] = ln.Addr().String()
		if address == "10.8.0.6:18080" {
			return testenv.SourceListener(ln, netip.MustParseAddr("10.8.0.1")), nil
		}
		return ln, nil
	}
	signals := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- s.run(io.Discard, listen, signals) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(vars["XDG_STATE_HOME"], "cuttle", "cuttle.log"))
		if strings.Contains(string(data), `msg="listener bound" address=10.8.0.6:18080`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retried listener not bound:\n%s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	remote, tries := bound["10.8.0.6:18080"], attempts
	mu.Unlock()
	if tries != 3 {
		t.Errorf("bound after %d attempts, want 3", tries)
	}
	conn, err := net.DialTimeout("tcp", remote, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	conn.Close()
	if err != nil || res.StatusCode != 403 || !strings.Contains(res.Header.Get("Proxy-Status"), "remote-default") {
		t.Fatalf("remote client on the retried listener: %v %v", res, err)
	}
	signals <- syscall.SIGTERM
	if code := <-done; code != exitOK {
		t.Errorf("exit %d", code)
	}
	for _, l := range s.p.Listeners() {
		if l.State != proxy.ListenerBound {
			t.Errorf("listener %+v", l)
		}
	}
	log, _ := os.ReadFile(s.paths.Log.Path)
	for _, want := range []string{`msg="listener retrying" address=10.8.0.6:18080`, `msg="listener bound" address=10.8.0.6:18080`,
		"source=remote kind=connect target=example.com:443 rule=remote-default"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// TestServeSIGTERM checks P5: the built binary, sent SIGTERM with an open
// tunnel and an in-flight 2 s plain-HTTP response, completes the response,
// closes the tunnel, and exits 0 within 8 s; a second signal exits at once
// with status 1.
func TestServeSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr := testenv.Port(sb.FreePort())
	writeConfig(t, sb, serveConfig([]string{addr}, ""))
	proc := sb.Start(addr, "serve")
	c := proxyClient(t, addr)

	// Open tunnels to the origin through the loopback rule, and one
	// connection upgraded after a 101.
	const tunnels = 8
	target := origin.Addr.String()
	var tbrs []*bufio.Reader
	for range tunnels {
		tun, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer tun.Close()
		tun.SetDeadline(time.Now().Add(20 * time.Second))
		io.WriteString(tun, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		tbr := bufio.NewReader(tun)
		if res, err := http.ReadResponse(tbr, &http.Request{Method: http.MethodConnect}); err != nil || res.StatusCode != 200 {
			t.Fatalf("CONNECT: %v %v", res, err)
		}
		io.WriteString(tun, "GET /echo HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		if res, err := http.ReadResponse(tbr, nil); err != nil || res.StatusCode != 200 {
			t.Fatalf("request through the tunnel: %v %v", res, err)
		} else {
			io.Copy(io.Discard, res.Body)
		}
		tbrs = append(tbrs, tbr)
	}
	ws, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetDeadline(time.Now().Add(20 * time.Second))
	io.WriteString(ws, "GET http://"+target+"/ws HTTP/1.1\r\nHost: "+target+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nOrigin: http://"+target+"\r\n\r\n")
	wsbr := bufio.NewReader(ws)
	if res, err := http.ReadResponse(wsbr, &http.Request{Method: http.MethodGet}); err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}

	// An in-flight 2 s response.
	type result struct {
		code int
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		res, err := c.Get("http://" + target + "/slow?ms=2000")
		if err != nil {
			slow <- result{err: err}
			return
		}
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		slow <- result{res.StatusCode, string(b), err}
	}()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("the slow request to reach the origin", func() bool {
		return slices.ContainsFunc(origin.Requests(), func(r testenv.Request) bool { return strings.HasPrefix(r.URI, "/slow") })
	})

	start := time.Now()
	proc.Signal(syscall.SIGTERM)
	r := <-slow
	if r.err != nil || r.code != 200 || r.body != "slow" {
		t.Errorf("in-flight response: %d %q %v", r.code, r.body, r.err)
	}
	for _, tbr := range tbrs {
		if n, err := tbr.ReadByte(); err == nil {
			t.Errorf("tunnel still open: read %q", n)
		}
	}
	if n, err := wsbr.ReadByte(); err == nil {
		t.Errorf("upgraded connection still open: read %q", n)
	}
	code, exited := proc.Wait(8*time.Second - time.Since(start))
	if !exited || code != 0 {
		t.Fatalf("exit %d after %v (exited %v)", code, time.Since(start), exited)
	}
	if el := time.Since(start); el < 1500*time.Millisecond || el > 8*time.Second {
		t.Errorf("stopped after %v", el)
	}
	log, _ := os.ReadFile(sb.LogPath())
	for _, want := range []string{
		"kind=connect target=" + target + " rule=loopback action=direct upstream=\"\" interface=\"\" outcome=shutdown status=200",
		"kind=http target=" + target + " rule=loopback action=direct upstream=\"\" interface=\"\" outcome=shutdown status=101",
		"kind=http target=" + target + " rule=loopback action=direct upstream=\"\" interface=\"\" outcome=ok status=200",
		"msg=stopping signal=terminated", "msg=stopped",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	// Every decision line comes before stopped, the last line (design 0006
	// D6 step 6).
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "msg=stopped") {
		t.Errorf("last log line %q, want stopped", last)
	}
	if n := strings.Count(string(log), "outcome=shutdown"); n != tunnels+1 {
		t.Errorf("%d shutdown decision lines, want %d:\n%s", n, tunnels+1, log)
	}

	// A second signal during the drain exits at once with status 1.
	proc = sb.Start(addr, "serve")
	c2 := proxyClient(t, addr)
	go func() {
		if res, err := c2.Get("http://" + target + "/slow?ms=6000"); err == nil {
			res.Body.Close()
		}
	}()
	waitFor("the second slow request", func() bool {
		n := 0
		for _, r := range origin.Requests() {
			if r.URI == "/slow?ms=6000" {
				n++
			}
		}
		return n == 1
	})
	proc.Signal(syscall.SIGTERM)
	if _, exited := proc.Wait(500 * time.Millisecond); exited {
		t.Fatal("exited before the drain ended")
	}
	start = time.Now()
	proc.Signal(syscall.SIGTERM)
	code, exited = proc.Wait(2 * time.Second)
	if !exited || code != 1 {
		t.Errorf("second signal: exit %d after %v (exited %v)", code, time.Since(start), exited)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("second signal took %v", el)
	}
}

// TestControlSocketPathTooLong checks design 0006 D1 for socket paths over
// the Unix limit: with XDG_RUNTIME_DIR and TMPDIR both too long, cuttle
// paths names the cause, cuttle status and cuttle reload refuse with it
// before dialing, and cuttle serve logs one error and serves without the
// socket. It runs serve in-process on a loopback port the test chose.
func TestControlSocketPathTooLong(t *testing.T) {
	if testing.Short() {
		t.Skip("serves")
	}
	lookup, vars := tempEnv(t)
	long := filepath.Join(t.TempDir(), strings.Repeat("r", 90))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	vars["XDG_RUNTIME_DIR"] = long
	vars["TMPDIR"] = filepath.Join(t.TempDir(), strings.Repeat("t", 90))

	code, out, _ := runCLI(lookup, "paths")
	if code != 0 || !strings.Contains(out, "XDG_RUNTIME_DIR path too long") || !strings.Contains(out, "no control socket") {
		t.Errorf("paths: %d\n%s", code, out)
	}
	for _, cmd := range []string{"status", "reload"} {
		if code, _, errOut := runCLI(lookup, cmd); code != 1 || !strings.Contains(errOut, "longer than the 103-byte limit") {
			t.Errorf("%s: %d %q", cmd, code, errOut)
		}
	}

	dir := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(serveConfig([]string{"127.0.0.1:18080"}, "")), 0o600)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	bound := make(chan string, 1)
	listen := func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			bound <- ln.Addr().String()
		}
		return ln, err
	}
	signals := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- s.run(io.Discard, listen, signals) }()
	var addr string
	select {
	case addr = <-bound:
	case code := <-done:
		t.Fatalf("serve exited with %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("listener not bound")
	}
	if code := getStatus(t, proxyClient(t, addr), "http://blocked.cuttle.test/"); code != http.StatusForbidden {
		t.Errorf("proxy without a control socket answered %d", code)
	}
	signals <- syscall.SIGTERM
	if code := <-done; code != exitOK {
		t.Errorf("exit %d", code)
	}
	log, _ := os.ReadFile(s.paths.Log.Path)
	if n := strings.Count(string(log), `level=ERROR msg="control socket unavailable; running without it"`); n != 1 ||
		!strings.Contains(string(log), "longer than the 103-byte limit") {
		t.Errorf("%d control socket errors:\n%s", n, log)
	}
	if _, err := os.Stat(vars["TMPDIR"]); err == nil {
		t.Errorf("serve created %s", vars["TMPDIR"])
	}
}
