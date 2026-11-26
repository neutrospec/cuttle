package main

import (
	"bufio"
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/admin"
	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/testenv"
)

const adminTestPassword = "correct horse battery"

// adminServe is cuttle serve running in-process with the admin UI, a
// test network, and statistics that the test flushes itself.
type adminServe struct {
	t       *testing.T
	s       *serveState
	lookup  env
	net     *testenv.Net
	signals chan os.Signal
	done    chan int
	stderr  *lockedBuffer

	mu    sync.Mutex
	bound map[string]string // configured address -> real address
}

// startAdminServe writes text as the configuration, sets the admin
// password, and starts serve with every listener on a loopback port of
// its own; text names the listeners it wants with addresses that are
// never bound.
func startAdminServe(t *testing.T, text string) *adminServe {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	dir := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle")
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &adminServe{t: t, lookup: lookup, net: testenv.NewNet(), signals: make(chan os.Signal, 1), done: make(chan int, 1),
		stderr: &lockedBuffer{}, bound: map[string]string{}}
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	a.s = s
	s.network, s.selfAddrs, s.interfaces, s.timers = a.net, testenv.SelfAddrs, testenv.NewInterfaces(), &testenv.Timers{}
	s.statsInterval = -1
	phc, err := admin.HashPassword([]byte(adminTestPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.WritePasswordFile(s.paths.Password.Path, phc); err != nil {
		t.Fatal(err)
	}
	listen := func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			a.mu.Lock()
			a.bound[address] = ln.Addr().String()
			a.mu.Unlock()
		}
		return ln, err
	}
	go func() { a.done <- s.run(a.stderr, listen, a.signals) }()
	t.Cleanup(func() { a.stop() })
	waitControl(t, lookup)
	return a
}

// addr returns the real address of a configured listener.
func (a *adminServe) addr(configured string) string {
	a.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.mu.Lock()
		got := a.bound[configured]
		a.mu.Unlock()
		if got != "" {
			return got
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("%s not bound: %s", configured, a.stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stop sends SIGTERM once and checks the exit status.
func (a *adminServe) stop() {
	select {
	case a.signals <- syscall.SIGTERM:
	default:
		return
	}
	select {
	case code := <-a.done:
		if code != exitOK {
			a.t.Errorf("serve exit %d: %s", code, a.stderr.String())
		}
	case <-time.After(15 * time.Second):
		a.t.Error("serve did not stop")
	}
}

func (a *adminServe) flush() {
	a.t.Helper()
	if err := a.s.stats.Flush(context.Background()); err != nil {
		a.t.Fatalf("flush: %v", err)
	}
}

// signIn signs a new Go client in at base.
func signIn(t *testing.T, base string) *http.Client {
	t.Helper()
	c := adminClient(t)
	res, body, tok := adminGet(t, c, base+"/login")
	if res.StatusCode != 200 || tok == "" {
		t.Fatalf("/login: %d\n%s", res.StatusCode, body)
	}
	if res, _ := adminPost(t, c, base+"/login", url.Values{"login_token": {tok}, "password": {adminTestPassword}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", res.StatusCode)
	}
	return c
}

var (
	hostCellRE = regexp.MustCompile(`<td class="host[^"]*">([^<]+)</td>`)
	rowRE      = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellRE     = regexp.MustCompile(`(?s)<t[dh][^>]*>(.*?)</t[dh]>`)
	tagRE      = regexp.MustCompile(`<[^>]*>`)
)

// hasRow reports whether a table row of the page starts with the cells,
// compared as text without tags, so that the checks do not depend on the
// markup of the admin UI's visual system.
func hasRow(body string, cells ...string) bool {
	for _, r := range rowRE.FindAllStringSubmatch(body, -1) {
		var got []string
		for _, c := range cellRE.FindAllStringSubmatch(r[1], -1) {
			got = append(got, strings.Join(strings.Fields(html.UnescapeString(tagRE.ReplaceAllString(c[1], " "))), " "))
		}
		if len(got) >= len(cells) && slices.Equal(got[:len(cells)], cells) {
			return true
		}
	}
	return false
}

func hostCells(body string) string {
	var out []string
	for _, m := range hostCellRE.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return strings.Join(out, ",")
}

// dashboardConfig routes gw.cuttle.test through upstream gateway, blocks
// blocked.cuttle.test, and sends everything else direct.
const dashboardConfig = `name = "cuttle-test"

[proxy]
listen = ["127.0.0.1:18080"]
default = "direct"

[admin]
listen = "127.0.0.1:18081"

[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"

[[rule]]
name = "restricted"
hosts = ["gw.cuttle.test"]
action = "upstream:gateway"

[[rule]]
name = "blocked"
domains = ["blocked.cuttle.test"]
action = "block"
`

// trafficMocks routes bulk.cuttle.test and files.cuttle.test to one
// origin, and the gateway upstream to a recording upstream that passes
// gw.cuttle.test to the same origin.
func trafficMocks(t *testing.T, n *testenv.Net) {
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	n.Map("bulk.cuttle.test", "203.0.113.50")
	n.Map("files.cuttle.test", "203.0.113.51")
	n.Route("203.0.113.50:80", origin.Addr)
	n.Route("203.0.113.51:80", origin.Addr)
	up := testenv.NewNet()
	up.Map("gw.cuttle.test", "203.0.113.60")
	up.Route("203.0.113.60:80", origin.Addr)
	gw := testenv.NewRecordingUpstream(t, testenv.UpstreamOptions{Net: up})
	n.Route("10.8.0.6:3128", gw.Addr)
}

// TestAdminPages checks the page half of A8 in a running instance: after
// direct tunnels, plain HTTP through upstream gateway, and blocked
// requests, /domains renders the rows of each route over 24h, the
// overview shows the routes and their top hosts with the upstream state,
// and /live lists an open tunnel. A failed statistics write shows in the
// overview and in cuttle status (OI-0006 Q-R5), and status shows the
// admin listener (OI-0006 P-R2).
func TestAdminPages(t *testing.T) {
	a := startAdminServe(t, dashboardConfig)
	trafficMocks(t, a.net)
	proxyAddr, adminAddr := a.addr("127.0.0.1:18080"), a.addr("127.0.0.1:18081")
	base := "http://" + adminAddr

	// A finished tunnel, an open one, plain HTTP through the upstream, and
	// two blocked requests.
	openTunnel(t, proxyAddr, "files.cuttle.test:80", 700).Close()
	open := openTunnel(t, proxyAddr, "bulk.cuttle.test:80", 5000)
	pc := proxyClient(t, proxyAddr)
	for range 3 {
		if code := getStatus(t, pc, "http://gw.cuttle.test/bytes?n=1234"); code != 200 {
			t.Fatalf("upstream request: %d", code)
		}
	}
	for _, target := range []string{"blocked.cuttle.test:443", "x.blocked.cuttle.test:443"} {
		c, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
		if err != nil || res.StatusCode != 403 {
			t.Fatalf("CONNECT %s: %v %v", target, res, err)
		}
		c.Close()
	}
	waitLog2(t, a.s.paths.Log.Path, 6, "msg=decision")
	// A flush that fails keeps its rows for the next one and shows as the
	// last write error.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.s.stats.Flush(cancelled); err == nil {
		t.Fatal("flush with a cancelled context succeeded")
	}
	a.flush()

	c := signIn(t, base)
	for _, q := range []struct{ query, want string }{
		{"route=upstream:gateway&window=24h", "gw.cuttle.test"},
		{"route=direct&window=24h", "bulk.cuttle.test,files.cuttle.test"},
		{"route=block&window=24h", "blocked.cuttle.test,x.blocked.cuttle.test"},
	} {
		res, body, _ := adminGet(t, c, base+"/domains?"+q.query)
		if got := hostCells(body); res.StatusCode != 200 || got != q.want {
			t.Errorf("/domains?%s: %d %q, want %q", q.query, res.StatusCode, got, q.want)
		}
	}
	_, body, _ := adminGet(t, c, base+"/domains?route=upstream:gateway")
	if !hasRow(body, "1", "gw.cuttle.test", "upstream gateway", "restricted", "3", "0") {
		t.Errorf("upstream row:\n%s", body)
	}

	res, body, _ := adminGet(t, c, base+"/")
	for _, want := range []string{
		"<h3>Sent direct</h3>", "<h3>Sent to upstream gateway</h3>", "<h3>Blocked</h3>",
		"Last statistics write error at ", "context canceled",
		"<dt>Admin listener</dt><dd>" + adminAddr + ` <span class="state listening">listening</span></dd>`,
	} {
		if res.StatusCode != 200 || !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	for _, want := range [][]string{{"upstream gateway", "1", "3"}, {"gateway", "10.8.0.6:3128", "up"}} {
		if !hasRow(body, want...) {
			t.Errorf("overview lacks the row %q", want)
		}
	}
	if got := hostCells(body); got != "bulk.cuttle.test,files.cuttle.test,gw.cuttle.test,blocked.cuttle.test,x.blocked.cuttle.test" {
		t.Errorf("overview hosts %s", got)
	}

	res, body, _ = adminGet(t, c, base+"/live")
	if res.StatusCode != 200 || !hasRow(body, "bulk.cuttle.test:80", "connect", "direct", "local") ||
		!strings.Contains(body, `<meta http-equiv="refresh" content="5">`) || !strings.Contains(body, "Active tunnels (1)") {
		t.Errorf("/live:\n%s", body)
	}
	open.Close()

	code, out, errOut := runCLI(a.lookup, "status", "--json")
	var st control.Status
	if code != exitOK || json.Unmarshal([]byte(out), &st) != nil {
		t.Fatalf("status: %d %s", code, errOut)
	}
	if st.Admin.State != control.AdminListening || st.Admin.Address != adminAddr || st.Admin.Since.IsZero() ||
		!strings.Contains(st.StatsWriteError, "context canceled") || st.StatsWriteErrorAt == nil {
		t.Errorf("status admin %+v, write error %q at %v", st.Admin, st.StatsWriteError, st.StatsWriteErrorAt)
	}
	_, out, _ = runCLI(a.lookup, "status")
	for _, re := range []string{
		`(?m)^admin +` + regexp.QuoteMeta(adminAddr) + ` listening since \S+$`,
		`(?m)^ +last write error at \S+: .*context canceled$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("status text lacks %s:\n%s", re, out)
		}
	}
}

// waitLog2 waits until the log at path holds n lines with want.
func waitLog2(t *testing.T, path string, n int, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		if strings.Count(string(data), want) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log has fewer than %d %q:\n%s", n, want, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var settingsBaseRE = regexp.MustCompile(`name="base" value="([0-9a-f]{64})"`)

// TestAdminSettings checks A9 in a running instance: invalid text gets 422
// with line numbers and leaves the file; a stale base gets 409 and leaves
// the file; a valid save replaces the file with mode 0600, writes .bak
// with the previous content, and reloads, while a tunnel opened before
// keeps transferring and a new request follows the new rules; and a
// proxy.listen change shows restart_required (design 0005 D10).
func TestAdminSettings(t *testing.T) {
	a := startAdminServe(t, dashboardConfig)
	trafficMocks(t, a.net)
	proxyAddr, adminAddr := a.addr("127.0.0.1:18080"), a.addr("127.0.0.1:18081")
	base := "http://" + adminAddr
	path := a.s.cfgPath
	c := signIn(t, base)

	// A tunnel opened before the save, through the origin's echo.
	tunnel := openTunnel(t, proxyAddr, "bulk.cuttle.test:80", 100)
	br := bufio.NewReader(tunnel)
	echo := func(what string) {
		t.Helper()
		io.WriteString(tunnel, "GET /bytes?n=321 HTTP/1.1\r\nHost: bulk.cuttle.test\r\n\r\n")
		res, err := http.ReadResponse(br, nil)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: request through the tunnel: %v %v", what, res, err)
		}
		if n, _ := io.Copy(io.Discard, res.Body); n != 321 {
			t.Fatalf("%s: read %d bytes", what, n)
		}
	}
	// openTunnel's reader may hold buffered bytes; it read exactly one
	// response, so a new reader starts at the next one.
	echo("before the save")

	page := func() (csrf, b string) {
		t.Helper()
		res, body, tok := adminGet(t, c, base+"/settings")
		m := settingsBaseRE.FindStringSubmatch(body)
		if res.StatusCode != 200 || tok == "" || m == nil || !strings.Contains(body, path) {
			t.Fatalf("/settings: %d\n%s", res.StatusCode, body)
		}
		return tok, m[1]
	}
	csrf, b := page()
	original, _ := os.ReadFile(path)
	unchanged := func(what string) {
		t.Helper()
		if now, _ := os.ReadFile(path); string(now) != string(original) {
			t.Fatalf("%s: the file changed", what)
		}
		if _, err := os.Stat(path + ".bak"); err == nil {
			t.Fatalf("%s: .bak written", what)
		}
	}
	save := func(text, b string) (*http.Response, string) {
		t.Helper()
		return adminPost(t, c, base+"/settings", url.Values{"csrf": {csrf}, "base": {b}, "text": {text}})
	}

	invalid := strings.Replace(dashboardConfig, `default = "direct"`, `default = "sideways"`, 1)
	res, body := save(invalid, b)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "line 5: proxy.default: ") {
		t.Errorf("invalid: %d\n%s", res.StatusCode, body)
	}
	unchanged("invalid")

	newRules := strings.Replace(dashboardConfig, `domains = ["blocked.cuttle.test"]`, `domains = ["blocked.cuttle.test", "files.cuttle.test"]`, 1)
	if res, _ := save(newRules, strings.Repeat("0", 64)); res.StatusCode != http.StatusConflict {
		t.Errorf("stale base: %d", res.StatusCode)
	}
	unchanged("stale base")
	if code := getStatus(t, proxyClient(t, proxyAddr), "http://files.cuttle.test/bytes?n=10"); code != 200 {
		t.Fatalf("files.cuttle.test before the save: %d", code)
	}

	res, body = save(newRules, b)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved and reloaded.") {
		t.Fatalf("save: %d\n%s", res.StatusCode, body)
	}
	data, _ := os.ReadFile(path)
	bak, _ := os.ReadFile(path + ".bak")
	fi, _ := os.Stat(path)
	bi, _ := os.Stat(path + ".bak")
	if string(data) != newRules || string(bak) != string(original) || fi.Mode().Perm() != 0o600 || bi.Mode().Perm() != 0o600 {
		t.Errorf("after the save: file %q (%v), .bak %q (%v)", data, fi.Mode(), bak, bi.Mode())
	}
	waitLog2(t, a.s.paths.Log.Path, 1, `msg=reloaded`)
	echo("after the reload")
	if code := getStatus(t, proxyClient(t, proxyAddr), "http://files.cuttle.test/bytes?n=10"); code != http.StatusForbidden {
		t.Errorf("files.cuttle.test after the save: %d, want 403", code)
	}
	_, out, _ := runCLI(a.lookup, "status", "--json")
	var st control.Status
	json.Unmarshal([]byte(out), &st)
	if st.Config.SHA256 != config.Digest([]byte(newRules)) || len(st.Tunnels) != 1 || st.Tunnels[0].Target != "bulk.cuttle.test:80" {
		t.Errorf("status after the save: sha256 %s, tunnels %+v", st.Config.SHA256, st.Tunnels)
	}

	// A proxy.listen change saves and lists the key under restart required.
	_, b = page()
	moved := strings.Replace(newRules, "127.0.0.1:18080", "127.0.0.1:18082", 1)
	res, body = save(moved, b)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Restart required for: proxy.listen.") {
		t.Errorf("proxy.listen change: %d\n%s", res.StatusCode, body)
	}
	_, out, _ = runCLI(a.lookup, "status")
	if !strings.Contains(out, "restart required: proxy.listen") {
		t.Errorf("status after the proxy.listen change:\n%s", out)
	}
	echo("after the second reload")
	tunnel.Close()
}
