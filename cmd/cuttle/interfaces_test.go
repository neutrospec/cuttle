package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/iface"
	"github.com/neutrospec/cuttle/internal/paths"
	"github.com/neutrospec/cuttle/internal/testenv"
)

func ipList(list ...string) []netip.Addr {
	out := make([]netip.Addr, len(list))
	for i, a := range list {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// formatTable is an injected interface table: Wi-Fi, a primary Ethernet,
// an Ethernet without a link, loopback, and a tunnel interface.
func formatTable() []iface.Link {
	wifi := testenv.Row(4, "en0", 0x01, "198.18.0.4", "fe80::4")
	wifi.Kind = iface.KindWifi
	eth := testenv.Row(7, "en7", 0x0a, "198.18.0.7", "2001:db8::7")
	eth.Default4 = iface.RoutePrimary
	down := testenv.Row(3, "en3", 0x03)
	down.Media, down.Default4 = iface.MediaInactive, iface.RouteNone
	lo := iface.Link{Index: 1, Device: "lo0", Up: true, Loopback: true, Addrs: ipList("127.0.0.1", "::1")}
	utun := iface.Link{Index: 20, Device: "utun0", Up: true, Addrs: ipList("fe80::20"), Default6: iface.RouteScoped}
	return []iface.Link{lo, wifi, eth, down, utun}
}

func formatDefs() []iface.Def {
	return []iface.Def{
		{Name: "eth-a", MAC: testenv.TestMAC(0x0a), Kind: iface.KindWired},
		{Name: "wifi", Device: "en0", Kind: iface.KindWifi},
		{Name: "eth-b", MAC: testenv.TestMAC(0x03)},
		{Name: "phone-a", MAC: testenv.TestMAC(0x1a)},
	}
}

// TestInterfacesFormat checks the text and JSON forms of cuttle interfaces
// for an injected table, including match with a definition and its
// reason, and the change lines of --watch (design 0008 D15, I15).
func TestInterfacesFormat(t *testing.T) {
	rows := interfaceRows(formatTable(), formatDefs(), false)
	var b bytes.Buffer
	printInterfaces(&b, rows)
	want := [][]string{
		{"index", "device", "kind", "mac", "media", "ipv4", "ipv6", "default4", "default6", "match"},
		{"4", "en0", "wifi", "00:00:5e:00:53:01", "active", "198.18.0.4", "-", "scoped", "none", "wifi"},
		{"7", "en7", "wired", "00:00:5e:00:53:0a", "active", "198.18.0.7", "2001:db8::7", "primary", "none", "eth-a"},
		{"3", "en3", "wired", "00:00:5e:00:53:03", "inactive", "-", "-", "none", "none", "eth-b", "(link_down)"},
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("text:\n%s", b.String())
	}
	for i, l := range lines {
		if got := strings.Fields(l); !slices.Equal(got, want[i]) {
			t.Errorf("line %d: %q, want %q", i, got, want[i])
		}
	}

	b.Reset()
	json.NewEncoder(&b).Encode(interfaceRows(formatTable(), formatDefs(), true))
	var objs []map[string]any
	if err := json.Unmarshal(b.Bytes(), &objs); err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 || objs[0]["device"] != "lo0" || objs[0]["mac"] != "" || objs[4]["device"] != "utun0" {
		t.Fatalf("--all rows: %v", objs)
	}
	eth, _ := json.Marshal(objs[2])
	if want := `{"addresses":{"ipv4":["198.18.0.7"],"ipv6":["2001:db8::7"]},"default":{"ipv4":"primary","ipv6":"none"},` +
		`"device":"en7","index":7,"kind":"wired","mac":"00:00:5e:00:53:0a","match":"eth-a","media":"active","reason":"","state":"online"}`; string(eth) != want {
		t.Errorf("en7:\n%s\nwant\n%s", eth, want)
	}
	if objs[3]["match"] != "eth-b" || objs[3]["state"] != "offline" || objs[3]["reason"] != "link_down" {
		t.Errorf("en3: %v", objs[3])
	}
	if objs[4]["match"] != "" || objs[4]["state"] != "" || objs[4]["media"] != "none" || objs[4]["kind"] != "other" {
		t.Errorf("utun0: %v", objs[4])
	}
	// Without a configuration, match, state, and reason are empty.
	for _, r := range interfaceRows(formatTable(), nil, true) {
		if r.Match != "" || r.State != "" || r.Reason != "" {
			t.Errorf("without definitions: %+v", r)
		}
	}

	// --watch: a notice, then one line per changed device after 200 ms.
	src := testenv.NewInterfaces(formatTable()...)
	timers := &testenv.Timers{}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut lockedBuffer
	done := make(chan struct{})
	now := time.Date(2026, 11, 25, 1, 2, 3, 456e6, time.UTC)
	notices := src.Watch(ctx)
	first, _ := src.Read()
	go func() {
		watchInterfaces(ctx, notices, src, timers, func() time.Time { return now }, first, false, &out, &errOut)
		close(done)
	}()
	for src.ActiveWatches() == 0 {
		time.Sleep(time.Millisecond)
	}
	src.Update("en7", func(l *iface.Link) { l.Media, l.Addrs, l.Default4 = iface.MediaInactive, nil, iface.RouteNone })
	src.Remove("en3")
	added := testenv.Row(9, "en9", 0x1a, "198.18.0.9")
	rowsNow, _ := src.Read()
	src.Set(append(rowsNow, added)...)
	src.Notice()
	timers.WaitPending(2, time.Second)
	timers.Advance(iface.Debounce)
	cancel()
	<-done
	wantOut := "2026-11-25T01:02:03.456Z en7 media active->inactive ipv4 198.18.0.7->- ipv6 2001:db8::7->- default4 primary->none\n" +
		"2026-11-25T01:02:03.456Z en9 attached\n" +
		"2026-11-25T01:02:03.456Z en3 detached\n"
	if out.String() != wantOut {
		t.Errorf("watch lines:\n%s\nwant\n%s%s", out.String(), wantOut, errOut.String())
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// failingSource is an interface table that cannot be read.
type failingSource struct{}

func (failingSource) Read() ([]iface.Link, error)               { return nil, errors.New("injected read failure") }
func (failingSource) Watch(ctx context.Context) <-chan struct{} { return make(chan struct{}) }

// TestInterfacesCommand runs cuttle interfaces against this host's table
// (design 0008 D15, I15). It asserts field names and types only and never
// prints the command's output, so the host's MACs and addresses never
// reach gate output.
func TestInterfacesCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("reads the host's interface table; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	check := func(args []string, all bool) {
		t.Helper()
		code, out, errOut := runCLI(lookup, args...)
		if code != exitOK || errOut != "" {
			t.Fatalf("%v: exit %d, %d bytes on stderr", args, code, len(errOut))
		}
		var objs []map[string]any
		if err := json.Unmarshal([]byte(out), &objs); err != nil {
			t.Fatalf("%v: output is not a JSON array", args)
		}
		if len(objs) == 0 {
			t.Fatalf("%v: no rows", args)
		}
		keys := []string{"addresses", "default", "device", "index", "kind", "mac", "match", "media", "reason", "state"}
		lo := false
		for i, o := range objs {
			var got []string
			for k := range o {
				got = append(got, k)
			}
			slices.Sort(got)
			if !slices.Equal(got, keys) {
				t.Errorf("row %d: field names %v", i, got)
			}
			_, isNum := o["index"].(float64)
			for _, k := range []string{"device", "kind", "mac", "media", "match", "state", "reason"} {
				if _, ok := o[k].(string); !ok {
					t.Errorf("row %d: %s is not a string", i, k)
				}
			}
			if !isNum {
				t.Errorf("row %d: index is not a number", i)
			}
			for _, k := range []string{"addresses", "default"} {
				m, ok := o[k].(map[string]any)
				if !ok || len(m) != 2 {
					t.Errorf("row %d: %s is not an object with two fields", i, k)
					continue
				}
				for _, fam := range []string{"ipv4", "ipv6"} {
					if k == "addresses" {
						list, ok := m[fam].([]any)
						if !ok {
							t.Errorf("row %d: addresses.%s is not a list", i, fam)
						}
						for _, a := range list {
							if _, ok := a.(string); !ok {
								t.Errorf("row %d: an address is not a string", i)
							}
						}
					} else if s, _ := m[fam].(string); !slices.Contains([]string{"primary", "scoped", "none"}, s) {
						t.Errorf("row %d: default.%s is not primary, scoped, or none", i, fam)
					}
				}
			}
			if s, _ := o["kind"].(string); !slices.Contains([]string{"wired", "wifi", "cellular", "other"}, s) {
				t.Errorf("row %d: kind outside wired, wifi, cellular, other", i)
			}
			if s, _ := o["media"].(string); !slices.Contains([]string{"active", "inactive", "none"}, s) {
				t.Errorf("row %d: media outside active, inactive, none", i)
			}
			if s, _ := o["mac"].(string); !all && len(s) != 17 {
				t.Errorf("row %d: the default lists a row without a six-byte MAC", i)
			}
			if o["match"] != "" || o["state"] != "" || o["reason"] != "" {
				t.Errorf("row %d: match, state, or reason set without a matching definition", i)
			}
			if o["device"] == "lo0" {
				lo = true
			}
		}
		if all && !lo {
			t.Errorf("--all lists no lo0 row")
		}
	}
	check([]string{"interfaces", "--json"}, false)
	check([]string{"interfaces", "--json", "--all"}, true)

	// A configuration whose definitions match no row leaves them empty.
	cfgPath := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	os.WriteFile(cfgPath, []byte("name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n[[interface]]\nname = \"phone-z\"\nmac = \"00:00:5e:00:53:fe\"\n"), 0o600)
	check([]string{"interfaces", "--json"}, false)

	if code, out, _ := runCLI(lookup, "interfaces"); code != exitOK || !strings.HasPrefix(out, "index ") {
		t.Errorf("text form: exit %d, header missing", code)
	}
	for _, args := range [][]string{{"interfaces", "--json", "--watch"}, {"interfaces", "extra"}, {"interfaces", "--bogus"}} {
		if code, _, _ := runCLI(lookup, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	saved := interfaceSource
	interfaceSource = failingSource{}
	code, _, errOut := runCLI(lookup, "interfaces")
	interfaceSource = saved
	if code != exitFailure || !strings.Contains(errOut, "cannot read the interface table") {
		t.Errorf("unreadable table: exit %d %q", code, errOut)
	}
}

// TestExplainInterfaces checks the interfaces line of cuttle explain: the
// list in force and its source for a direct rule, the default list, an
// upstream with a list, and system routing (design 0008 D15, I15).
func TestExplainInterfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	path := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`name = "cuttle-test"

[proxy]
default = "direct"
interfaces = ["eth-a", "wifi"]

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"

[[interface]]
name = "eth-b"
mac = "00:00:5e:00:53:0b"

[[interface]]
name = "wifi"
device = "en0"
kind = "wifi"

[[upstream]]
name = "proxy-wired"
address = "203.0.113.20:3128"
disclosure = "none"
interfaces = ["eth-a", "eth-b"]

[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"

[[rule]]
name = "system-updates"
domains = ["updates.example"]
action = "direct"
interfaces = ["eth-b"]

[[rule]]
name = "bulk-mirror"
domains = ["mirror.example"]
action = "upstream:proxy-wired"

[[rule]]
name = "restricted"
domains = ["internal.example"]
action = "upstream:gateway"

[[rule]]
name = "ads"
domains = ["ads.example"]
action = "block"
`), 0o600)
	for target, want := range map[string]string{
		"https://updates.example/":  "interfaces  eth-b (rule system-updates); public addresses only",
		"example.com:443":           "interfaces  eth-a, wifi (proxy.interfaces); public addresses only",
		"http://mirror.example/":    "interfaces  eth-a, eth-b (upstream proxy-wired); public addresses only",
		"wiki.internal.example:443": "interfaces  system routing",
		"ads.example:443":           "interfaces  system routing",
	} {
		code, out, errOut := runCLI(lookup, "explain", target)
		if code != exitOK || !strings.Contains(out, "\n"+want+"\n") {
			t.Errorf("explain %s: exit %d\n%s%s\nwant line %q", target, code, out, errOut, want)
		}
	}
}

// macPieces returns pieces of the test MACs in the texts that no status
// field, log line, or time stamp holds by chance: eight-character windows
// of the colon, dash, dotted, and bare forms that contain a hex letter,
// and the parser's quoted prefix of an unquoted MAC.
func macPieces(texts ...string) []string {
	re := regexp.MustCompile(`(?i)\b[0-9a-f]{2}(:[0-9a-f]{2}){5}\b`)
	letter := regexp.MustCompile(`[a-f]`)
	seen := map[string]bool{}
	for _, text := range texts {
		for _, m := range re.FindAllString(text, -1) {
			hex := strings.ToLower(strings.ReplaceAll(m, ":", ""))
			pairs := make([]string, 6)
			for i := range pairs {
				pairs[i] = hex[2*i : 2*i+2]
			}
			for _, form := range []string{hex, strings.Join(pairs, ":"), strings.Join(pairs, "-"),
				hex[0:4] + "." + hex[4:8] + "." + hex[8:12]} {
				for i := 0; i+8 <= len(form); i++ {
					if w := form[i : i+8]; letter.MatchString(w) {
						seen[w] = true
					}
				}
			}
			seen[strings.Join(pairs[:2], ":")+":"+pairs[2][:1]+`"`] = true
			seen[strings.Join(pairs[:2], ":")+":"+pairs[2][:1]+`\"`] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

func checkNoMAC(t *testing.T, what, text string, pieces []string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, p := range pieces {
		if strings.Contains(lower, p) {
			t.Errorf("%s holds MAC piece %q", what, p)
		}
	}
}

// TestStatusInterfaces checks cuttle status and /v1/status with interface
// definitions (design 0008 D15, I12): each definition with the D15 fields,
// each tunnel with its interface, and no MAC in the status output, the log,
// the error lines of reloads refused for a duplicate mac or an unquoted
// MAC, or last_reload_error. The instance runs in process with the test
// network, the fake interface source, and an injected timer.
func TestStatusInterfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	dir := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "config.toml")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	good := `name = "cuttle-test"
[proxy]
listen = ["127.0.0.1:18080"]
default = "direct"
interfaces = ["eth-a", "eth-b"]
[admin]
listen = ""

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"

[[interface]]
name = "eth-b"
mac = "00:00:5e:00:53:0b"
`
	dupMAC := strings.Replace(good, `mac = "00:00:5e:00:53:0b"`, `mac = "00:00:5E:00:53:0A"`, 1)
	unquoted := strings.Replace(good, `mac = "00:00:5e:00:53:0b"`, `mac = 00:00:5e:00:53:0c`, 1)
	pieces := macPieces(good, dupMAC, unquoted)
	write(good)

	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	n := testenv.NewNet()
	n.Route("203.0.113.10:80", origin.Addr)
	src := testenv.NewInterfaces(testenv.Row(7, "en7", 0x0a, "198.18.0.7"))
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.network, s.selfAddrs, s.interfaces, s.timers = n, testenv.SelfAddrs, src, &testenv.Timers{}
	var bound atomicAddr
	listen := func(network, address string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			bound.set(ln.Addr().String())
		}
		return ln, err
	}
	signals := make(chan os.Signal, 1)
	done := make(chan int, 1)
	var stderr lockedBuffer
	go func() { done <- s.run(&stderr, listen, signals) }()
	deadline := time.Now().Add(10 * time.Second)
	for bound.get() == "" {
		if time.Now().After(deadline) {
			t.Fatalf("not serving: %s", stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitControl(t, lookup)

	// A tunnel through eth-a.
	c, err := net.DialTimeout("tcp", bound.get(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT 203.0.113.10:80 HTTP/1.1\r\nHost: 203.0.113.10:80\r\n\r\n")
	br := bufio.NewReader(c)
	if res, err := http.ReadResponse(br, nil); err != nil || res.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", res, err)
	}

	code, out, errOut := runCLI(lookup, "status", "--json")
	if code != exitOK {
		t.Fatalf("status: exit %d %s", code, errOut)
	}
	checkNoMAC(t, "status --json", out, pieces)
	var raw map[string]json.RawMessage
	json.Unmarshal([]byte(out), &raw)
	var objs []map[string]any
	json.Unmarshal(raw["interfaces"], &objs)
	if len(objs) != 2 {
		t.Fatalf("interfaces: %s", raw["interfaces"])
	}
	for _, o := range objs {
		var keys []string
		for k := range o {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"demoted_until", "device", "families", "name", "primary", "reason", "since", "state"}; !slices.Equal(keys, want) {
			t.Errorf("interface fields %v, want %v", keys, want)
		}
	}
	var st control.Status
	json.Unmarshal([]byte(out), &st)
	a, b := st.Interfaces[0], st.Interfaces[1]
	if a.Name != "eth-a" || a.Device != "en7" || a.State != "online" || a.Reason != "" || !slices.Equal(a.Families, []string{"ipv4"}) ||
		len(a.Primary) != 0 || a.Since.IsZero() || a.DemotedUntil != "" {
		t.Errorf("eth-a %+v", a)
	}
	if b.Name != "eth-b" || b.Device != "" || b.State != "offline" || b.Reason != "absent" || len(b.Families) != 0 || len(b.Primary) != 0 {
		t.Errorf("eth-b %+v", b)
	}
	if len(st.Tunnels) != 1 || st.Tunnels[0].Interface != "eth-a" || !strings.Contains(string(raw["tunnels"]), `"interface": "eth-a"`) {
		t.Errorf("tunnels %s", raw["tunnels"])
	}
	if st.Counters[`interface_state{interface="eth-a",state="online"}`] != 1 || st.Counters[`interface_state{interface="eth-b",state="offline"}`] != 1 {
		t.Errorf("gauges %v", st.Counters)
	}
	code, out, _ = runCLI(lookup, "status")
	if code != exitOK || !regexp.MustCompile(`(?m)^  eth-a +en7 +online +since \S+ +families ipv4$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^  eth-b +- +offline \(absent\) +since \S+$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^  203\.0\.113\.10:80 +connect +direct +local +interface eth-a +age`).MatchString(out) {
		t.Errorf("status text:\n%s", out)
	}
	checkNoMAC(t, "status", out, pieces)
	c.Close()

	// Reloads refused for a duplicate mac and for an unquoted MAC.
	for _, bad := range []struct{ text, msg string }{
		{dupMAC, "interface[2].mac: duplicate of interface[1].mac"},
		{unquoted, "interface.mac: TOML syntax: invalid value; put it in double quotes"},
	} {
		write(bad.text)
		code, out, errOut := runCLI(lookup, "reload")
		if code != exitFailure || !strings.Contains(errOut, bad.msg) {
			t.Errorf("reload: exit %d %q, want %q", code, errOut, bad.msg)
		}
		checkNoMAC(t, "reload output", out+errOut, pieces)
		_, out, _ = runCLI(lookup, "status", "--json")
		json.Unmarshal([]byte(out), &st)
		if !strings.Contains(st.Config.LastReloadError, bad.msg) {
			t.Errorf("last_reload_error %q", st.Config.LastReloadError)
		}
		checkNoMAC(t, "last_reload_error", st.Config.LastReloadError, pieces)
		checkNoMAC(t, "status after a refused reload", out, pieces)
	}

	signals <- syscall.SIGTERM
	if code := <-done; code != exitOK {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
	log, _ := os.ReadFile(s.paths.Log.Path)
	checkNoMAC(t, "log", string(log), pieces)
	for _, want := range []string{
		`msg="interface state" interface=eth-a device=en7 state=online families=ipv4`,
		`msg="interface state" interface=eth-b device="" state=offline reason=absent`,
		`interface=eth-a outcome=ok`,
		`msg="configuration error" error="` + path + `:15: interface[2].mac: duplicate of interface[1].mac"`,
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

type atomicAddr struct {
	mu sync.Mutex
	s  string
}

func (a *atomicAddr) set(s string) { a.mu.Lock(); a.s = s; a.mu.Unlock() }
func (a *atomicAddr) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.s }

// waitControl waits until the control socket answers.
func waitControl(t *testing.T, lookup env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _, _ := runCLI(lookup, "status", "--json"); code == exitOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket not answering")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestControlUnreachable checks OI-0006 M-R1: status and reload exit 1
// with "cannot reach the running service at <path>" and the socket path,
// both when the socket directory is missing and when it is private but no
// socket exists.
func TestControlUnreachable(t *testing.T) {
	lookup, _ := tempEnv(t)
	p, err := paths.Resolve(lookup)
	if err != nil {
		t.Fatal(err)
	}
	sock := p.Control.Path
	for _, stage := range []string{"missing directory", "private directory without a socket"} {
		if stage != "missing directory" {
			if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
				t.Fatal(err)
			}
		} else if _, err := os.Stat(filepath.Dir(sock)); err == nil {
			t.Fatalf("%s exists", filepath.Dir(sock))
		}
		for _, cmd := range []string{"status", "reload"} {
			code, out, errOut := runCLI(lookup, cmd)
			want := "cannot reach the running service at " + sock
			if code != exitFailure || !strings.Contains(errOut, want) || out != "" {
				t.Errorf("%s, %s: exit %d stdout %q stderr %q, want exit 1 and %q", stage, cmd, code, out, errOut, want)
			}
		}
	}
}

// TestStopWaitsForReload checks OI-0006 M-R4: serve logs stopped only
// after a reload in progress has finished, so its lines come first.
func TestStopWaitsForReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	path := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:18080\"]\ndefault = \"block\"\n[admin]\nlisten = \"\"\n"), 0o600)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	listen := func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") }
	signals := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- s.run(io.Discard, listen, signals) }()
	waitControl(t, lookup)

	s.reloadMu.Lock() // a reload in progress
	signals <- syscall.SIGTERM
	time.Sleep(300 * time.Millisecond)
	log, _ := os.ReadFile(s.paths.Log.Path)
	if !strings.Contains(string(log), "msg=stopping") || strings.Contains(string(log), "msg=stopped") {
		t.Errorf("stopped logged while a reload was in progress:\n%s", log)
	}
	code, returned := 0, false
	select {
	case code = <-done:
		returned = true
		t.Errorf("serve returned while a reload was in progress")
	default:
	}
	s.reloadMu.Unlock()
	if !returned {
		code = <-done
	}
	if code != exitOK {
		t.Errorf("exit %d", code)
	}
	log, _ = os.ReadFile(s.paths.Log.Path)
	if !strings.Contains(string(log), "msg=stopped") {
		t.Errorf("no stopped line:\n%s", log)
	}
}

// blockingSource holds one Read open until the test releases it. The held
// read returns the rows as they were when it started.
type blockingSource struct {
	*testenv.Interfaces
	mu      sync.Mutex
	armed   bool
	enter   chan struct{}
	release chan struct{}
	calls   []string
}

func (b *blockingSource) Read() ([]iface.Link, error) {
	links, err := b.Interfaces.Read()
	b.mu.Lock()
	b.calls = append(b.calls, "read")
	armed := b.armed
	b.armed = false
	b.mu.Unlock()
	if armed {
		close(b.enter)
		<-b.release
	}
	return links, err
}

func (b *blockingSource) Watch(ctx context.Context) <-chan struct{} {
	b.mu.Lock()
	b.calls = append(b.calls, "watch")
	b.mu.Unlock()
	return b.Interfaces.Watch(ctx)
}

// TestInterfacesWatchNoticeDuringRead checks TASK-0009 N-R2: a notice that
// arrives while a re-read runs schedules another re-read 200 ms later, so
// the change it reports does not wait for the 30 s safety re-read. I20
// step 5 measures notice latency with this command.
func TestInterfacesWatchNoticeDuringRead(t *testing.T) {
	src := &blockingSource{Interfaces: testenv.NewInterfaces(formatTable()...), enter: make(chan struct{}), release: make(chan struct{})}
	timers := &testenv.Timers{}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut lockedBuffer
	now := time.Date(2026, 11, 25, 1, 2, 3, 456e6, time.UTC)
	notices := src.Watch(ctx)
	first, _ := src.Read()
	done := make(chan struct{})
	go func() {
		watchInterfaces(ctx, notices, src, timers, func() time.Time { return now }, first, false, &out, &errOut)
		close(done)
	}()

	// A notice starts a re-read, which the source holds open.
	src.mu.Lock()
	src.armed = true
	src.mu.Unlock()
	src.Notice()
	if !timers.WaitPending(2, 2*time.Second) {
		t.Fatalf("no debounce after the first notice: %d timers", timers.Pending())
	}
	advanced := make(chan struct{})
	go func() {
		timers.Advance(iface.Debounce)
		close(advanced)
	}()
	select {
	case <-src.enter:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-read did not start")
	}
	// The link drops during the read, and its notice arrives.
	src.Update("en7", func(l *iface.Link) { l.Media = iface.MediaInactive })
	src.Notice()
	timers.WaitPending(2, 500*time.Millisecond)
	close(src.release)
	<-advanced

	// 200 ms later the change is printed, long before the safety re-read.
	timers.Advance(iface.Debounce)
	cancel()
	<-done
	want := "2026-11-25T01:02:03.456Z en7 media active->inactive\n"
	if out.String() != want {
		t.Errorf("a notice during a read was dropped: output %q, want %q (stderr %q)", out.String(), want, errOut.String())
	}
}

// TestInterfacesWatchOrder checks that --watch opens the notice sockets
// before the first read, and that a failed read ends the Watch call
// (TASK-0009 N-R2).
func TestInterfacesWatchOrder(t *testing.T) {
	src := &blockingSource{Interfaces: testenv.NewInterfaces(formatTable()...)}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut lockedBuffer
	done := make(chan int)
	go func() {
		done <- listInterfaces(ctx, src, &testenv.Timers{}, time.Now, nil, false, false, true, &out, &errOut)
	}()
	for src.ActiveWatches() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("exit %d", code)
	}
	src.mu.Lock()
	order := strings.Join(src.calls, ",")
	src.mu.Unlock()
	if order != "watch,read" {
		t.Errorf("calls %q, want watch,read", order)
	}
	if !strings.HasPrefix(out.String(), "index ") {
		t.Errorf("no table before the change lines")
	}

	src.FailRead(errors.New("injected read failure"))
	if code := listInterfaces(context.Background(), src, &testenv.Timers{}, time.Now, nil, false, false, true, &out, &errOut); code != exitFailure {
		t.Errorf("unreadable table: exit %d", code)
	}
	if !src.WaitWatchEnded(time.Second) {
		t.Errorf("the Watch call outlived a failed read")
	}
}

// TestStatusInterfaceConsistency checks TASK-0009 N-R6: when a demotion
// has expired unobserved, one status reply shows the interface online in
// both interfaces and the interface_state gauge.
func TestStatusInterfaceConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped with -short")
	}
	lookup, vars := tempEnv(t)
	path := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`name = "cuttle-test"
[proxy]
listen = ["127.0.0.1:18080"]
default = "direct"
interfaces = ["eth-a", "eth-b"]
interface_attempt_timeout = "100ms"
interface_demotion = "1s"
[admin]
listen = ""

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"

[[interface]]
name = "eth-b"
mac = "00:00:5e:00:53:0b"
`), 0o600)
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	n := testenv.NewNet()
	n.Route("203.0.113.10:80", origin.Addr)
	n.OnInterface("eth-a", "", testenv.Blackhole)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.network, s.selfAddrs, s.timers = n, testenv.SelfAddrs, &testenv.Timers{}
	s.interfaces = testenv.NewInterfaces(testenv.Row(7, "en7", 0x0a, "198.18.0.7"), testenv.Row(8, "en8", 0x0b, "198.18.0.8"))
	var bound atomicAddr
	listen := func(network, _ string) (net.Listener, error) {
		ln, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			bound.set(ln.Addr().String())
		}
		return ln, err
	}
	signals := make(chan os.Signal, 1)
	done := make(chan int, 1)
	go func() { done <- s.run(io.Discard, listen, signals) }()
	defer func() {
		signals <- syscall.SIGTERM
		<-done
	}()
	waitControl(t, lookup)

	// eth-a times out, eth-b carries the tunnel, and eth-a is demoted.
	c, err := net.DialTimeout("tcp", bound.get(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT 203.0.113.10:80 HTTP/1.1\r\nHost: 203.0.113.10:80\r\n\r\n")
	if res, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil || res.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", res, err)
	}
	c.Close()
	status := func() (string, map[string]int64) {
		t.Helper()
		code, out, errOut := runCLI(lookup, "status", "--json")
		if code != exitOK {
			t.Fatalf("status: exit %d %s", code, errOut)
		}
		var st control.Status
		json.Unmarshal([]byte(out), &st)
		return st.Interfaces[0].State, st.Counters
	}
	gauge := func(c map[string]int64, state string) int64 {
		return c[`interface_state{interface="eth-a",state="`+state+`"}`]
	}
	if state, counters := status(); state != "demoted" || gauge(counters, "demoted") != 1 {
		t.Fatalf("after the timeout: state %q, demoted gauge %d", state, gauge(counters, "demoted"))
	}
	time.Sleep(1100 * time.Millisecond)
	state, counters := status()
	if state != "online" || gauge(counters, "online") != 1 || gauge(counters, "demoted") != 0 {
		t.Errorf("one reply disagrees after the demotion expired: state %q, gauge online %d demoted %d",
			state, gauge(counters, "online"), gauge(counters, "demoted"))
	}
}
