package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"squid/internal/iface"
	"squid/internal/policy"
	"squid/internal/stats"
	"squid/internal/testenv"
)

// ifaceDefs defines the test interfaces eth-a, eth-b, and wifi with
// documentation MACs (design 0008 acceptance criteria).
const ifaceDefs = `
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
`

// ifaceConfig is the test configuration with extra [proxy] keys, the
// starter's loopback rule, the definitions, and more tables.
func ifaceConfig(proxyKeys, rest string) string {
	return `
name = "cuttle-test"

[proxy]
default = "direct"
` + proxyKeys + `
[admin]
listen = ""

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true
` + ifaceDefs + rest
}

// ifaceRows are the rows of eth-a (en7), eth-b (en8), and wifi (en0), each
// online for IPv4 with an address in 198.18.0.0/15, outside SelfAddrs.
func ifaceRows() []iface.Link {
	wifi := testenv.Row(4, "en0", 0x01, "198.18.0.4")
	wifi.Kind = iface.KindWifi
	return []iface.Link{testenv.Row(7, "en7", 0x0a, "198.18.0.7"), testenv.Row(8, "en8", 0x0b, "198.18.0.8"), wifi}
}

// newIfaceEnv starts an instance with the fake interface source and an
// injected timer, creating them unless opt has them.
func newIfaceEnv(t *testing.T, cfgText string, opt envOptions) (*env, *testenv.Interfaces, *testenv.Timers) {
	t.Helper()
	if opt.interfaces == nil {
		opt.interfaces = testenv.NewInterfaces(ifaceRows()...)
	}
	if opt.timers == nil {
		opt.timers = &testenv.Timers{}
	}
	e := newEnvWith(t, cfgText, opt)
	return e, opt.interfaces, opt.timers
}

// reread sends a notice and runs the debounced re-read it causes.
func reread(t *testing.T, src *testenv.Interfaces, timers *testenv.Timers) {
	t.Helper()
	before := src.Reads()
	if !src.Notice() {
		t.Fatal("no active Watch call")
	}
	if !timers.WaitPending(2, 2*time.Second) {
		t.Fatalf("no debounce timer: %d pending", timers.Pending())
	}
	timers.Advance(iface.Debounce)
	if got := src.Reads(); got != before+1 {
		t.Fatalf("reads %d, want %d", got, before+1)
	}
}

// dialsVia lists the recorded dials as "interface address", "-" for an
// unbound dial.
func dialsVia(n *testenv.Net) []string {
	var out []string
	for _, e := range n.DialEvents() {
		name := e.Interface
		if name == "" {
			name = "-"
		}
		out = append(out, name+" "+e.Addr.String())
	}
	return out
}

func wantDials(t *testing.T, n *testenv.Net, want ...string) {
	t.Helper()
	if got := dialsVia(n); !slices.Equal(got, want) {
		t.Errorf("dials %q, want %q", got, want)
	}
}

// lastDecision waits for the nth decision line and returns the last one.
func (e *env) lastDecision(n int) map[string]string {
	e.t.Helper()
	ds := e.waitDecisions(n)
	return ds[len(ds)-1]
}

// checkSkipped compares the skipped field; "" means absent.
func checkSkipped(t *testing.T, d map[string]string, want string) {
	t.Helper()
	got, present := d["skipped"]
	if want == "" && present {
		t.Errorf("skipped=%q present, want absent", got)
	} else if want != "" && got != want {
		t.Errorf("skipped=%q, want %q", got, want)
	}
}

// connectClose opens a tunnel and closes it at once, so its decision line
// follows.
func (e *env) connectClose(target string) {
	e.t.Helper()
	c := e.connectRaw(target)
	c.Close()
}

func plainGet(host, path string) string {
	return "GET http://" + host + path + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
}

func connectReq(target string) string {
	return "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
}

func linkDown(l *iface.Link) { l.Media = iface.MediaInactive }

// rowsWith returns the test rows with changes applied by device.
func rowsWith(changes map[string]func(*iface.Link)) []iface.Link {
	rows := ifaceRows()
	for i := range rows {
		if f := changes[rows[i].Device]; f != nil {
			f(&rows[i])
		}
	}
	return rows
}

const failoverProxy = `interfaces = ["eth-a", "eth-b", "wifi"]
interface_attempt_timeout = "300ms"
connect_timeout = "2s"
`

// TestInterfaceFailover checks failover along proxy.interfaces for CONNECT
// and plain HTTP (design 0008 D7, D8, D15, I6).
func TestInterfaceFailover(t *testing.T) {
	cfg := ifaceConfig(failoverProxy, "")
	type variant struct {
		name   string
		kind   string
		target string
		run    func(e *env)
	}
	connect := variant{"connect", "connect", "origin.cuttle.test:443", func(e *env) { e.connectClose("origin.cuttle.test:443") }}
	plain := variant{"http", "http", "plain.cuttle.test:80", func(e *env) {
		if res, _ := e.raw(plainGet("plain.cuttle.test", "/echo")); res.StatusCode != 200 {
			e.t.Fatalf("status %d", res.StatusCode)
		}
	}}
	addr := func(v variant) string {
		if v.kind == "http" {
			return "203.0.113.10:80"
		}
		return "203.0.113.10:443"
	}

	t.Run("a all reachable", func(t *testing.T) {
		for _, wifiDown := range []bool{false, true} {
			for _, v := range []variant{connect, plain} {
				rows := ifaceRows()
				if wifiDown {
					rows = rowsWith(map[string]func(*iface.Link){"en0": linkDown})
				}
				e, _, _ := newIfaceEnv(t, cfg, envOptions{interfaces: testenv.NewInterfaces(rows...)})
				v.run(e)
				d := e.lastDecision(1)
				checkDecision(t, d, map[string]string{"kind": v.kind, "interface": "eth-a", "outcome": "ok"})
				checkSkipped(t, d, "")
				wantDials(t, e.net, "eth-a "+addr(v))
				if ev := e.net.DialEvents(); len(ev) == 1 && ev[0].Index != 7 {
					t.Errorf("bound to index %d, want 7", ev[0].Index)
				}
			}
		}
	})

	t.Run("b eth-a link down", func(t *testing.T) {
		for _, v := range []variant{connect, plain} {
			e, _, _ := newIfaceEnv(t, cfg, envOptions{interfaces: testenv.NewInterfaces(rowsWith(map[string]func(*iface.Link){"en7": linkDown})...)})
			v.run(e)
			d := e.lastDecision(1)
			checkDecision(t, d, map[string]string{"kind": v.kind, "interface": "eth-b", "outcome": "ok"})
			checkSkipped(t, d, "eth-a:link_down")
			wantDials(t, e.net, "eth-b "+addr(v))
		}
	})

	t.Run("c eth-a unreachable", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		e.net.OnInterface("eth-a", "", testenv.Errno(syscall.ENETUNREACH))
		e.connectClose("origin.cuttle.test:443")
		d := e.lastDecision(1)
		checkDecision(t, d, map[string]string{"interface": "eth-b", "outcome": "ok"})
		checkSkipped(t, d, "eth-a:unreachable")
		wantDials(t, e.net, "eth-a 203.0.113.10:443", "eth-b 203.0.113.10:443")
		if ev := e.net.DialEvents(); len(ev) == 2 {
			if gap := ev[1].At.Sub(ev[0].At); gap > 50*time.Millisecond {
				t.Errorf("eth-b started %v after eth-a, want within 50ms", gap)
			}
		}
	})

	t.Run("d eth-a blackholed", func(t *testing.T) {
		for _, v := range []variant{connect, plain} {
			e, _, _ := newIfaceEnv(t, cfg, envOptions{})
			e.net.OnInterface("eth-a", "", testenv.Blackhole)
			v.run(e)
			d := e.lastDecision(1)
			checkDecision(t, d, map[string]string{"kind": v.kind, "interface": "eth-b", "outcome": "ok"})
			checkSkipped(t, d, "eth-a:timeout")
			wantDials(t, e.net, "eth-a "+addr(v), "eth-b "+addr(v))
			if ev := e.net.DialEvents(); len(ev) == 2 {
				if gap := ev[1].At.Sub(ev[0].At); gap < 200*time.Millisecond || gap > 400*time.Millisecond {
					t.Errorf("%s: eth-b started %v after eth-a, want 300ms ± 100ms", v.name, gap)
				}
			}
		}
	})

	t.Run("e eth-a refuses", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		e.net.OnInterface("eth-a", "", testenv.Refuse)
		res, _ := e.raw(connectReq("origin.cuttle.test:443"))
		if res.StatusCode != 502 || !strings.Contains(res.Header.Get("Proxy-Status"), "error=connection_refused") {
			t.Errorf("status %d %q", res.StatusCode, res.Header.Get("Proxy-Status"))
		}
		d := e.lastDecision(1)
		checkDecision(t, d, map[string]string{"interface": "", "outcome": OutcomeRefused})
		checkSkipped(t, d, "eth-a:refused")
		wantDials(t, e.net, "eth-a 203.0.113.10:443")
	})

	t.Run("f eth-a EPERM", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		e.net.OnInterface("eth-a", "", testenv.Errno(syscall.EPERM))
		res, _ := e.raw(connectReq("origin.cuttle.test:443"))
		if res.StatusCode != 502 {
			t.Errorf("status %d", res.StatusCode)
		}
		d := e.lastDecision(1)
		checkDecision(t, d, map[string]string{"interface": ""})
		checkSkipped(t, d, "eth-a:error")
		wantDials(t, e.net, "eth-a 203.0.113.10:443")
	})

	t.Run("g all blackholed", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		for _, name := range []string{"eth-a", "eth-b", "wifi"} {
			e.net.OnInterface(name, "", testenv.Blackhole)
		}
		start := time.Now()
		res, _ := e.raw(connectReq("origin.cuttle.test:443"))
		el := time.Since(start)
		if res.StatusCode != 504 || !strings.Contains(res.Header.Get("Proxy-Status"), "error=connection_timeout") {
			t.Errorf("status %d %q", res.StatusCode, res.Header.Get("Proxy-Status"))
		}
		if el < 1800*time.Millisecond || el > 2200*time.Millisecond {
			t.Errorf("answered after %v, want 2s ± 200ms", el)
		}
		d := e.lastDecision(1)
		checkSkipped(t, d, "eth-a:timeout,eth-b:timeout,wifi:timeout")
		ev := e.net.DialEvents()
		if len(ev) != 3 {
			t.Fatalf("dials %q", dialsVia(e.net))
		}
		// wifi, the last group, has the rest of the budget.
		if rest := start.Add(el).Sub(ev[2].At); rest < 1200*time.Millisecond {
			t.Errorf("wifi group ran %v, want the rest of the budget (about 1.4s)", rest)
		}
	})
}

// TestInterfacePinning checks rule lists, the no-usable-interface answer
// for local and remote clients, and unbound dials without a list (design
// 0008 D6, D10, I7).
func TestInterfacePinning(t *testing.T) {
	cfg := ifaceConfig("", `
[proxy.remote]
sources = ["10.8.0.1/32"]
allow_direct = true

[[rule]]
name = "system-updates"
domains = ["updates.cuttle.test"]
action = "direct"
interfaces = ["eth-b"]

[[rule]]
name = "prefer-b"
domains = ["both.cuttle.test"]
action = "direct"
interfaces = ["eth-b", "eth-a"]
`)
	e, src, timers := newIfaceEnv(t, cfg, envOptions{remote: []string{"10.8.0.1"}})
	for _, name := range []string{"updates.cuttle.test", "both.cuttle.test"} {
		e.net.Map(name, "203.0.113.10")
	}
	e.connectClose("updates.cuttle.test:443")
	checkDecision(t, e.lastDecision(1), map[string]string{"rule": "system-updates", "interface": "eth-b", "outcome": "ok"})
	wantDials(t, e.net, "eth-b 203.0.113.10:443")

	// eth-b absent: 503 with details for a local client, without for a
	// remote one; no dial either way.
	src.Remove("en8")
	reread(t, src, timers)
	e.net.Reset()
	res, body := e.raw(connectReq("updates.cuttle.test:443"))
	generatedHeaders(t, res, 503, `cuttle-test; error=destination_unavailable; details="no usable interface (eth-b: absent)"`)
	if string(body) != "cuttle: no usable interface for updates.cuttle.test:443\n" {
		t.Errorf("body %q", body)
	}
	d := e.lastDecision(2)
	checkDecision(t, d, map[string]string{"outcome": OutcomeNoInterface, "status": "503", "interface": "", "source": "local"})
	checkSkipped(t, d, "eth-b:absent")
	res, _ = e.rawFrom("10.8.0.1", connectReq("updates.cuttle.test:443"))
	generatedHeaders(t, res, 503, `cuttle-test; error=destination_unavailable`)
	d = e.lastDecision(3)
	checkDecision(t, d, map[string]string{"outcome": OutcomeNoInterface, "source": "remote"})
	checkSkipped(t, d, "eth-b:absent")
	wantDials(t, e.net)
	if n := e.p.Live().Get(stats.OutcomesTotal, "outcome", OutcomeNoInterface); n != 2 {
		t.Errorf("outcomes_total no_interface %d", n)
	}

	// ["eth-b", "eth-a"] uses eth-a while eth-b is link_down.
	src.Set(rowsWith(map[string]func(*iface.Link){"en8": linkDown})...)
	reread(t, src, timers)
	e.net.Reset()
	e.connectClose("both.cuttle.test:443")
	d = e.lastDecision(4)
	checkDecision(t, d, map[string]string{"rule": "prefer-b", "interface": "eth-a"})
	checkSkipped(t, d, "eth-b:link_down")
	wantDials(t, e.net, "eth-a 203.0.113.10:443")

	// Without proxy.interfaces, a request whose rule has no list dials
	// unbound.
	e.net.Reset()
	e.connectClose("origin.cuttle.test:443")
	d = e.lastDecision(5)
	checkDecision(t, d, map[string]string{"rule": "default", "interface": "other"})
	checkSkipped(t, d, "")
	wantDials(t, e.net, "- 203.0.113.10:443")
}

// TestInterfaceDemotion checks demotion: its trigger, effect, logs, gauge,
// and its end by expiry and by a row change (design 0008 D9, I8).
func TestInterfaceDemotion(t *testing.T) {
	clock := newFakeClock()
	cfg := ifaceConfig(failoverProxy+"interface_demotion = \"1s\"\n", "")
	e, src, timers := newIfaceEnv(t, cfg, envOptions{clock: clock.Now})
	e.net.Map("other.cuttle.test", "203.0.113.10")
	live := e.p.Live()
	gauge := func(state string) int64 {
		return live.Get(stats.InterfaceState, "interface", "eth-a", "state", state)
	}
	lines := func(msg string) int { return strings.Count(e.logs.String(), `msg="`+msg+`"`) }

	// I6 (d): eth-a times out, eth-b connects, eth-a is demoted.
	e.net.OnInterface("eth-a", "", testenv.Blackhole)
	e.connectClose("origin.cuttle.test:443")
	checkSkipped(t, e.lastDecision(1), "eth-a:timeout")
	if lines("interface demoted") != 1 || !strings.Contains(e.logs.String(), `msg="interface demoted" interface=eth-a device=en7 reason=timeout for=1s`) {
		t.Errorf("demoted lines:\n%s", e.logs.String())
	}
	if gauge(iface.StateDemoted) != 1 || gauge(iface.StateOnline) != 0 {
		t.Errorf("gauge demoted %d online %d", gauge(iface.StateDemoted), gauge(iface.StateOnline))
	}

	// The next request to another destination skips eth-a.
	e.net.Reset()
	e.connectClose("other.cuttle.test:443")
	d := e.lastDecision(2)
	checkDecision(t, d, map[string]string{"interface": "eth-b"})
	checkSkipped(t, d, "eth-a:demoted")
	wantDials(t, e.net, "eth-b 203.0.113.10:443")

	// After the clock passes 1 s, the next request plans eth-a first, and
	// the restored line comes at that request.
	e.net.ClearInterface("eth-a")
	clock.Advance(time.Second)
	if lines("interface restored") != 0 {
		t.Errorf("restored before a plan looked at eth-a")
	}
	e.net.Reset()
	e.connectClose("other.cuttle.test:443")
	checkDecision(t, e.lastDecision(3), map[string]string{"interface": "eth-a"})
	wantDials(t, e.net, "eth-a 203.0.113.10:443")
	if lines("interface restored") != 1 || !strings.Contains(e.logs.String(), `msg="interface restored" interface=eth-a cause=expired`) {
		t.Errorf("restored lines:\n%s", e.logs.String())
	}
	if gauge(iface.StateOnline) != 1 || gauge(iface.StateDemoted) != 0 {
		t.Errorf("gauge after expiry: online %d demoted %d", gauge(iface.StateOnline), gauge(iface.StateDemoted))
	}

	// A notice that changes eth-a's row ends a demotion at once.
	e.net.OnInterface("eth-a", "", testenv.Blackhole)
	e.connectClose("origin.cuttle.test:443")
	e.waitDecisions(4)
	if lines("interface demoted") != 2 || gauge(iface.StateDemoted) != 1 {
		t.Fatalf("second demotion: %d lines, gauge %d", lines("interface demoted"), gauge(iface.StateDemoted))
	}
	src.Update("en7", func(l *iface.Link) { l.Addrs = append(l.Addrs, netip.MustParseAddr("198.18.0.17")); l.Index = 17 })
	reread(t, src, timers)
	if !strings.Contains(e.logs.String(), `msg="interface restored" interface=eth-a cause=state_changed`) || gauge(iface.StateOnline) != 1 {
		t.Errorf("row change did not end the demotion:\n%s", e.logs.String())
	}

	// A dial in which every group timed out demotes nothing.
	for _, name := range []string{"eth-b", "wifi"} {
		e.net.OnInterface(name, "", testenv.Blackhole)
	}
	res, _ := e.raw(connectReq("origin.cuttle.test:443"))
	if res.StatusCode != 504 {
		t.Errorf("all blackholed: %d", res.StatusCode)
	}
	if lines("interface demoted") != 2 {
		t.Errorf("a dial that failed everywhere demoted: %d lines", lines("interface demoted"))
	}
	for _, name := range []string{"eth-a", "eth-b", "wifi"} {
		if live.Get(stats.InterfaceState, "interface", name, "state", iface.StateDemoted) != 0 {
			t.Errorf("%s demoted", name)
		}
	}
}

// TestInterfaceAddressClasses checks that loopback, self, and private
// addresses are dialed unbound in group 0, public ones per interface and
// family, and that a name with a refused private address still connects
// on its public one (design 0008 D7, D8, I9).
func TestInterfaceAddressClasses(t *testing.T) {
	cfg := ifaceConfig(`interfaces = ["eth-a", "eth-b"]
connect_timeout = "2s"
interface_attempt_timeout = "300ms"
`, `
[[rule]]
name = "lan"
hosts = ["nas.cuttle.test", "mixed.cuttle.test"]
action = "direct"
allow_private = true
`)
	offline := testenv.NewInterfaces(rowsWith(map[string]func(*iface.Link){
		"en7": func(l *iface.Link) { linkDown(l); l.Addrs = []netip.Addr{netip.MustParseAddr("198.18.0.10")} },
		"en8": linkDown,
	})...)
	e, src, timers := newIfaceEnv(t, cfg, envOptions{interfaces: offline})
	e.net.Map("nas.cuttle.test", "10.20.0.5")
	for _, a := range []string{"127.0.0.1:9000", "10.20.0.5:80", "192.0.2.50:80", "198.18.0.10:80"} {
		e.net.Route(a, e.http.Addr)
	}
	n := 0
	for _, c := range []struct {
		target, iface string
		dial          string
	}{
		{"localhost:9000", "", "- 127.0.0.1:9000"},
		{"127.0.0.1:9000", "", "- 127.0.0.1:9000"},
		{"10.20.0.5:80", "other", "- 10.20.0.5:80"},
		{"192.0.2.50:80", "", "- 192.0.2.50:80"},
		{"nas.cuttle.test:80", "other", "- 10.20.0.5:80"},
	} {
		e.net.Reset()
		e.connectClose(c.target)
		n++
		d := e.lastDecision(n)
		checkDecision(t, d, map[string]string{"target": c.target, "interface": c.iface, "outcome": "ok"})
		checkSkipped(t, d, "")
		wantDials(t, e.net, c.dial)
	}
	// A row's own address is public and gets D10 with every interface
	// offline.
	e.net.Reset()
	res, _ := e.raw(connectReq("198.18.0.10:80"))
	generatedHeaders(t, res, 503, `cuttle-test; error=destination_unavailable; details="no usable interface (eth-a: link_down, eth-b: link_down)"`)
	n++
	wantDials(t, e.net)

	// eth-a online for IPv4 only, eth-b for both families.
	src.Set(rowsWith(map[string]func(*iface.Link){
		"en8": func(l *iface.Link) {
			l.Addrs, l.Default6 = append(l.Addrs, netip.MustParseAddr("2001:db8::8")), iface.RouteScoped
		},
	})...)
	reread(t, src, timers)
	e.net.Map("dual.cuttle.test", "2001:db8::10", "203.0.113.10")
	e.net.Map("v6.cuttle.test", "2001:db8::10")
	e.net.Route("[2001:db8::10]:443", e.https.Addr)
	e.net.Reset()
	e.connectClose("dual.cuttle.test:443")
	n++
	checkDecision(t, e.lastDecision(n), map[string]string{"interface": "eth-a"})
	wantDials(t, e.net, "eth-a 203.0.113.10:443")

	e.net.Reset()
	e.connectClose("v6.cuttle.test:443")
	n++
	d := e.lastDecision(n)
	checkDecision(t, d, map[string]string{"interface": "eth-b"})
	checkSkipped(t, d, "eth-a:no_family")
	wantDials(t, e.net, "eth-b [2001:db8::10]:443")

	// No interface online for IPv6.
	src.Update("en8", func(l *iface.Link) { l.Default6 = iface.RouteNone })
	reread(t, src, timers)
	e.net.Reset()
	res, _ = e.raw(connectReq("v6.cuttle.test:443"))
	generatedHeaders(t, res, 503, `cuttle-test; error=destination_unavailable; details="no usable interface (eth-a: no_family, eth-b: no_family)"`)
	n++
	wantDials(t, e.net)

	// A name behind allow_private with a refusing private address and a
	// public one connects through eth-a on the public address, as in M1.4,
	// also when the private attempt fails with EPERM.
	e.net.Map("mixed.cuttle.test", "10.20.0.5", "203.0.113.10")
	for _, private := range []testenv.Behavior{testenv.Refuse, testenv.Errno(syscall.EPERM)} {
		e.net.OnInterface("", "10.20.0.5:443", private)
		e.net.Reset()
		e.connectClose("mixed.cuttle.test:443")
		n++
		d := e.lastDecision(n)
		checkDecision(t, d, map[string]string{"interface": "eth-a", "outcome": "ok"})
		checkSkipped(t, d, "")
		wantDials(t, e.net, "- 10.20.0.5:443", "eth-a 203.0.113.10:443")
	}
}

// pooledGet sends a plain-HTTP request through a keep-alive client.
func pooledGet(t *testing.T, c *http.Client, url string) {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("%s: status %d", url, res.StatusCode)
	}
}

// TestInterfacePools checks that pooled plain-HTTP connections stay with
// their interface list, and that an interface coming online or a demotion
// ending with cause connected closes idle connections while expiry does
// not (design 0008 D12, I10).
func TestInterfacePools(t *testing.T) {
	cfg := ifaceConfig(`interfaces = ["eth-a", "eth-b"]
interface_attempt_timeout = "300ms"
interface_demotion = "1s"
`, `
[[rule]]
name = "pin"
hosts = ["pin.cuttle.test"]
action = "direct"
interfaces = ["eth-b"]
`)
	t.Run("pooled per list", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		c, _ := e.goClient()
		pooledGet(t, c, "http://plain.cuttle.test/echo")
		pooledGet(t, c, "http://plain.cuttle.test/echo")
		wantDials(t, e.net, "eth-a 203.0.113.10:80")
		// Within the same snapshot and its transports, the same host and
		// port decided under a rule pinned to eth-b.
		s := e.p.snap.Load()
		pinned := *s
		settings := s.cfg.PolicySettings()
		settings.Rules = append([]policy.Rule{{Name: "pin-plain", Hosts: []string{"plain.cuttle.test"},
			Route: policy.Route{Action: policy.ActionDirect}, Interfaces: []string{"eth-b"}}}, settings.Rules...)
		pinned.policy = policy.New(settings)
		e.p.snap.Store(&pinned)
		pooledGet(t, c, "http://plain.cuttle.test/echo")
		wantDials(t, e.net, "eth-a 203.0.113.10:80", "eth-b 203.0.113.10:80")
		if d := e.lastDecision(3); d["rule"] != "pin-plain" || d["interface"] != "eth-b" {
			t.Errorf("decision %v", d)
		}
	})

	t.Run("online closes idle", func(t *testing.T) {
		e, src, timers := newIfaceEnv(t, cfg, envOptions{interfaces: testenv.NewInterfaces(rowsWith(map[string]func(*iface.Link){"en7": linkDown})...)})
		c, _ := e.goClient()
		pooledGet(t, c, "http://plain.cuttle.test/echo")
		wantDials(t, e.net, "eth-b 203.0.113.10:80")
		src.Set(ifaceRows()...)
		reread(t, src, timers)
		pooledGet(t, c, "http://plain.cuttle.test/echo")
		wantDials(t, e.net, "eth-b 203.0.113.10:80", "eth-a 203.0.113.10:80")
		checkDecision(t, e.lastDecision(2), map[string]string{"interface": "eth-a"})
	})

	t.Run("demotion ends", func(t *testing.T) {
		for _, cause := range []string{iface.CauseExpired, iface.CauseConnected} {
			clock := newFakeClock()
			e, _, _ := newIfaceEnv(t, cfg, envOptions{clock: clock.Now})
			e.net.Map("third.cuttle.test", "203.0.113.11")
			e.net.Route("203.0.113.11:80", e.http.Addr)
			c, _ := e.goClient()
			e.net.OnInterface("eth-a", "", testenv.Blackhole)
			pooledGet(t, c, "http://plain.cuttle.test/echo")
			wantDials(t, e.net, "eth-a 203.0.113.10:80", "eth-b 203.0.113.10:80")
			e.net.ClearInterface("eth-a")
			if cause == iface.CauseExpired {
				clock.Advance(time.Second)
				e.p.InterfaceStatus() // observes the expiry
			} else {
				// eth-b fails over and the demoted eth-a connects.
				e.net.OnInterface("eth-b", "203.0.113.11", testenv.Errno(syscall.ENETUNREACH))
				pooledGet(t, c, "http://third.cuttle.test/echo")
			}
			if !strings.Contains(e.logs.String(), `msg="interface restored" interface=eth-a cause=`+cause) {
				t.Fatalf("%s: no restored line:\n%s", cause, e.logs.String())
			}
			e.net.Reset()
			pooledGet(t, c, "http://plain.cuttle.test/echo")
			if cause == iface.CauseExpired {
				wantDials(t, e.net) // the idle connection on eth-b stays open
			} else {
				wantDials(t, e.net, "eth-a 203.0.113.10:80")
			}
		}
	})
}

// upstreamNames resolve to the mock origins in the recording upstreams'
// networks.
var upstreamNames = []string{"a.cuttle.test", "b.cuttle.test", "c.cuttle.test", "d.cuttle.test", "e.cuttle.test",
	"k.cuttle.test", "n.cuttle.test"}

// TestUpstreamInterfaces checks upstream lists (design 0008 D11, I11).
func TestUpstreamInterfaces(t *testing.T) {
	cfg := ifaceConfig(`interfaces = ["eth-a"]
interface_attempt_timeout = "300ms"
`, `
[[upstream]]
name = "plainup"
address = "203.0.113.30:3128"
disclosure = "none"

[[upstream]]
name = "pubup"
address = "203.0.113.20:3128"
disclosure = "none"
interfaces = ["eth-a", "wifi"]
connect_timeout = "1s"

[[upstream]]
name = "privup"
address = "10.8.0.7:3128"
disclosure = "none"
interfaces = ["eth-a", "wifi"]

[[upstream]]
name = "up-a"
address = "203.0.113.40:3128"
disclosure = "none"
interfaces = ["eth-a"]
connect_timeout = "1s"

[[upstream]]
name = "up-wifi"
address = "203.0.113.40:3128"
disclosure = "none"
interfaces = ["wifi"]

[[rule]]
name = "r-plain"
hosts = ["a.cuttle.test"]
action = "upstream:plainup"

[[rule]]
name = "r-pub"
hosts = ["b.cuttle.test"]
action = "upstream:pubup"

[[rule]]
name = "r-priv"
hosts = ["c.cuttle.test"]
action = "upstream:privup"

[[rule]]
name = "r-a"
hosts = ["d.cuttle.test"]
action = "upstream:up-a"

[[rule]]
name = "r-wifi"
hosts = ["e.cuttle.test"]
action = "upstream:up-wifi"
`)
	upState := func(e *env, name, state string) int64 {
		return e.p.Live().Get(stats.UpstreamState, "upstream", name, "state", state)
	}
	t.Run("binding", func(t *testing.T) {
		e, src, timers := newIfaceEnv(t, cfg, envOptions{})
		for _, at := range []string{"203.0.113.30:3128", "203.0.113.20:3128", "10.8.0.7:3128"} {
			e.recorder(at, testenv.UpstreamOptions{}, upstreamNames...)
		}
		e.connectClose("a.cuttle.test:443")
		wantDials(t, e.net, "- 203.0.113.30:3128")
		checkDecision(t, e.lastDecision(1), map[string]string{"upstream": "plainup", "interface": "other"})

		e.net.Reset()
		e.net.OnInterface("eth-a", "203.0.113.20", testenv.Errno(syscall.ENETUNREACH))
		e.connectClose("b.cuttle.test:443")
		wantDials(t, e.net, "eth-a 203.0.113.20:3128", "wifi 203.0.113.20:3128")
		d := e.lastDecision(2)
		checkDecision(t, d, map[string]string{"upstream": "pubup", "interface": "wifi"})
		checkSkipped(t, d, "eth-a:unreachable")
		e.net.ClearInterface("eth-a")

		e.net.Reset()
		e.connectClose("c.cuttle.test:443")
		wantDials(t, e.net, "- 10.8.0.7:3128")

		// Neither interface online: 503 with next-hop, no dial, and the
		// upstream stays up.
		src.Set(rowsWith(map[string]func(*iface.Link){"en7": linkDown, "en0": linkDown})...)
		reread(t, src, timers)
		e.net.Reset()
		res, _ := e.raw(connectReq("b.cuttle.test:443"))
		generatedHeaders(t, res, 503, `cuttle-test; error=destination_unavailable; next-hop="pubup"; details="no usable interface (eth-a: link_down, wifi: link_down)"`)
		checkDecision(t, e.lastDecision(4), map[string]string{"outcome": OutcomeNoInterface, "upstream": "pubup"})
		wantDials(t, e.net)
		if upState(e, "pubup", StateUp) != 1 {
			t.Errorf("upstream_state of pubup changed")
		}
	})

	t.Run("hold after both groups time out", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		e.net.OnInterface("eth-a", "203.0.113.20", testenv.Blackhole)
		e.net.OnInterface("wifi", "203.0.113.20", testenv.Blackhole)
		res, _ := e.raw(connectReq("b.cuttle.test:443"))
		if res.StatusCode != 504 {
			t.Errorf("status %d", res.StatusCode)
		}
		if upState(e, "pubup", StateDown) != 1 {
			t.Errorf("hold did not start")
		}
		res, _ = e.raw(connectReq("b.cuttle.test:443"))
		if res.StatusCode != 503 || !strings.Contains(res.Header.Get("Proxy-Status"), "unreachable for") {
			t.Errorf("held: %d %q", res.StatusCode, res.Header.Get("Proxy-Status"))
		}
	})

	t.Run("two upstreams with one address keep separate holds", func(t *testing.T) {
		e, _, _ := newIfaceEnv(t, cfg, envOptions{})
		e.recorder("203.0.113.40:3128", testenv.UpstreamOptions{}, upstreamNames...)
		e.net.OnInterface("eth-a", "203.0.113.40", testenv.Blackhole)
		if res, _ := e.raw(connectReq("d.cuttle.test:443")); res.StatusCode != 504 {
			t.Errorf("up-a: %d", res.StatusCode)
		}
		e.connectClose("e.cuttle.test:443")
		if upState(e, "up-a", StateDown) != 1 || upState(e, "up-wifi", StateUp) != 1 {
			t.Errorf("holds: up-a down %d, up-wifi up %d", upState(e, "up-a", StateDown), upState(e, "up-wifi", StateUp))
		}
	})

	t.Run("online closes the upstream's idle connections", func(t *testing.T) {
		e, src, timers := newIfaceEnv(t, cfg, envOptions{interfaces: testenv.NewInterfaces(rowsWith(map[string]func(*iface.Link){"en7": linkDown})...)})
		e.recorder("203.0.113.20:3128", testenv.UpstreamOptions{}, upstreamNames...)
		c, _ := e.goClient()
		pooledGet(t, c, "http://b.cuttle.test/echo")
		pooledGet(t, c, "http://b.cuttle.test/echo")
		wantDials(t, e.net, "wifi 203.0.113.20:3128")
		src.Set(ifaceRows()...)
		reread(t, src, timers)
		pooledGet(t, c, "http://b.cuttle.test/echo")
		wantDials(t, e.net, "wifi 203.0.113.20:3128", "eth-a 203.0.113.20:3128")
	})
}

// TestInterfaceStateEvents checks the state lines, the identity warnings,
// the gauge, and the interface of tunnels in status (design 0008 D3, D15,
// I12).
func TestInterfaceStateEvents(t *testing.T) {
	cfg := ifaceConfig(`interfaces = ["eth-a", "eth-b"]
`, `
[[interface]]
name = "phone-a"
mac = "00:00:5e:00:53:1a"

[[interface]]
name = "tagged"
device = "en9"
mac = "00:00:5e:00:53:1c"

[[interface]]
name = "cell"
device = "en5"
kind = "cellular"
`)
	rows := append(ifaceRows(),
		testenv.Row(9, "en9", 0x1d, "198.18.0.9"), // tagged's device with another MAC
		testenv.Row(5, "en5", 0x1e, "198.18.0.5"), // a wired row where cell expects cellular
		testenv.Row(11, "en11", 0x1a, "198.18.0.11"),
		testenv.Row(12, "vlan0", 0x1a, "198.18.0.12")) // phone-a's MAC twice
	e, src, timers := newIfaceEnv(t, cfg, envOptions{interfaces: testenv.NewInterfaces(rows...)})
	live := e.p.Live()
	gauge := func(name, state string) int64 {
		return live.Get(stats.InterfaceState, "interface", name, "state", state)
	}
	count := func(s string) int { return strings.Count(e.logs.String(), s) }

	// eth-b goes down and comes back.
	src.Update("en8", linkDown)
	reread(t, src, timers)
	reread(t, src, timers)
	if count(`msg="interface offline" interface=eth-b device=en8 reason=link_down`) != 1 {
		t.Errorf("offline lines:\n%s", e.logs.String())
	}
	if gauge("eth-b", iface.StateOffline) != 1 || gauge("eth-b", iface.StateOnline) != 0 {
		t.Errorf("gauge while offline")
	}
	src.Update("en8", func(l *iface.Link) { l.Media = iface.MediaActive })
	reread(t, src, timers)
	if count(`msg="interface online" interface=eth-b device=en8 families=ipv4 offline_for=`) != 1 {
		t.Errorf("online lines:\n%s", e.logs.String())
	}
	if gauge("eth-b", iface.StateOnline) != 1 || gauge("eth-b", iface.StateOffline) != 0 {
		t.Errorf("gauge after recovery")
	}
	// Each identity warning once, across several re-reads.
	for _, want := range []string{
		`msg="interface definition matches several devices" interface=phone-a devices=en11,vlan0`,
		`msg="interface kind mismatch" interface=cell device=en5 kind=wired expected=cellular`,
		`msg="interface device has a different MAC" interface=tagged device=en9`,
	} {
		if n := count(want); n != 1 {
			t.Errorf("%d lines %q:\n%s", n, want, e.logs.String())
		}
	}

	// Status lists each definition in configuration order with the D15
	// fields, and each tunnel with its interface.
	st := e.p.InterfaceStatus()
	var got []string
	for _, s := range st {
		got = append(got, fmt.Sprintf("%s/%s/%s/%s/%s/%s/%v", s.Name, s.Device, s.State, s.Reason, s.Families, s.Primary, s.Since.IsZero()))
	}
	want := []string{
		"eth-a/en7/online//ipv4//false",
		"eth-b/en8/online//ipv4//false",
		"wifi/en0/online//ipv4//false",
		"phone-a//offline/ambiguous///false",
		"tagged//offline/absent///false",
		"cell/en5/offline/kind_mismatch///false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("status:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	e.net.Reset()
	e.net.OnInterface("eth-a", "", testenv.Errno(syscall.ENETUNREACH))
	c := e.connectRaw("origin.cuttle.test:443")
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.p.Tunnels()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ts := e.p.Tunnels(); len(ts) != 1 || ts[0].Interface != "eth-b" {
		t.Errorf("tunnels %+v", ts)
	}
	for _, line := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(strings.ToLower(line), "5e:00:53") {
			t.Errorf("a log line holds a MAC: %s", line)
		}
	}
}

// TestReloadInterfaces checks reload with interfaces (design 0008 D14,
// I13).
func TestReloadInterfaces(t *testing.T) {
	head := `interfaces = ["eth-a", "eth-b"]
interface_attempt_timeout = "300ms"
interface_demotion = "1m"
`
	rest := `
[[rule]]
name = "pin"
hosts = ["pin.cuttle.test"]
action = "direct"
interfaces = ["eth-b"]

[[upstream]]
name = "kept"
address = "203.0.113.20:3128"
disclosure = "none"
interfaces = ["eth-a"]

[[upstream]]
name = "changed"
address = "203.0.113.21:3128"
disclosure = "none"
interfaces = ["eth-a"]

[[upstream]]
name = "named"
address = "203.0.113.22:3128"
disclosure = "none"
interfaces = ["wifi"]

[[rule]]
name = "via-kept"
hosts = ["k.cuttle.test"]
action = "upstream:kept"

[[rule]]
name = "via-changed"
hosts = ["c.cuttle.test"]
action = "upstream:changed"

[[rule]]
name = "via-named"
hosts = ["n.cuttle.test"]
action = "upstream:named"
`
	cfg := ifaceConfig(head, rest)
	e, src, _ := newIfaceEnv(t, cfg, envOptions{})
	e.net.Map("pin.cuttle.test", "203.0.113.10")
	e.net.Map("other.cuttle.test", "203.0.113.10")
	for _, at := range []string{"203.0.113.20:3128", "203.0.113.21:3128", "203.0.113.22:3128"} {
		e.recorder(at, testenv.UpstreamOptions{}, upstreamNames...)
	}
	c, _ := e.goClient()
	for _, host := range []string{"k", "c", "n"} {
		pooledGet(t, c, "http://"+host+".cuttle.test/echo")
	}

	// A tunnel through eth-b.
	tun := e.connectRaw("pin.cuttle.test:80")
	br := bufio.NewReader(tun)
	roundTrip := func(what string) {
		t.Helper()
		io.WriteString(tun, "GET /echo HTTP/1.1\r\nHost: pin.cuttle.test\r\n\r\n")
		res, err := http.ReadResponse(br, nil)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", what, res, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	roundTrip("before the reload")

	// Demote eth-a, whose definition stays unchanged.
	e.net.OnInterface("eth-a", "203.0.113.10", testenv.Blackhole)
	e.connectClose("other.cuttle.test:443")
	e.net.ClearInterface("eth-a")
	upBefore := e.p.snap.Load().upstreams
	watches := src.WatchCalls()

	// eth-b leaves every list and gets another MAC; wifi gets another MAC,
	// which the upstream named lists; changed gets another list.
	next := strings.Replace(cfg, `mac = "00:00:5e:00:53:0b"`, `mac = "00:00:5e:00:53:2b"`, 1)
	next = strings.Replace(next, `interfaces = ["eth-a", "eth-b"]`, `interfaces = ["eth-a", "wifi"]`, 1)
	next = strings.Replace(next, "interfaces = [\"eth-b\"]\n", "interfaces = [\"wifi\"]\n", 1)
	next = strings.Replace(next, "name = \"changed\"\naddress = \"203.0.113.21:3128\"\ndisclosure = \"none\"\ninterfaces = [\"eth-a\"]",
		"name = \"changed\"\naddress = \"203.0.113.21:3128\"\ndisclosure = \"none\"\ninterfaces = [\"eth-a\", \"wifi\"]", 1)
	next = strings.Replace(next, "device = \"en0\"\nkind = \"wifi\"", "device = \"en0\"\nmac = \"00:00:5e:00:53:01\"\nkind = \"wifi\"", 1)
	if strings.Count(next, "53:2b") != 1 || strings.Count(next, `["eth-a", "wifi"]`) != 2 || !strings.Contains(next, `mac = "00:00:5e:00:53:01"`) {
		t.Fatalf("configuration edit failed:\n%s", next)
	}
	if err := e.p.Reload(mustParse(t, next)); err != nil {
		t.Fatal(err)
	}
	roundTrip("after the reload")
	tun.Close()
	if src.WatchCalls() != watches {
		t.Errorf("a reload with definitions called Watch again")
	}

	// The demotion of the unchanged eth-a survives.
	if st := e.p.InterfaceStatus(); st[0].Name != "eth-a" || st[0].State != iface.StateDemoted {
		t.Errorf("eth-a after the reload: %+v", st[0])
	}
	// A request after the reload follows the new lists.
	e.net.Reset()
	e.connectClose("pin.cuttle.test:443")
	wantDials(t, e.net, "wifi 203.0.113.10:443")

	// Upstreams: kept keeps its state and pooled connection; changed and
	// named get new state.
	upAfter := e.p.snap.Load().upstreams
	if upAfter["kept"] != upBefore["kept"] || upAfter["changed"] == upBefore["changed"] || upAfter["named"] == upBefore["named"] {
		t.Errorf("upstream state kept %v changed %v named %v", upAfter["kept"] == upBefore["kept"],
			upAfter["changed"] == upBefore["changed"], upAfter["named"] == upBefore["named"])
	}
	if s := e.p.Upstreams(); len(s) != 3 || s[1].Name != "changed" || s[1].State != StateUp {
		t.Errorf("upstreams %+v", s)
	}
	e.net.Reset()
	pooledGet(t, c, "http://k.cuttle.test/echo")
	wantDials(t, e.net) // reused
	pooledGet(t, c, "http://n.cuttle.test/echo")
	wantDials(t, e.net, "wifi 203.0.113.22:3128") // not reused after the mac change

	// Removing the last definition ends the Watch context; adding one
	// calls Watch once.
	noIfaces := `
name = "cuttle-test"
[proxy]
default = "direct"
[admin]
listen = ""
`
	if err := e.p.Reload(mustParse(t, noIfaces)); err != nil {
		t.Fatal(err)
	}
	if !src.WaitWatchEnded(2*time.Second) || src.ActiveWatches() != 0 {
		t.Errorf("Watch still active without definitions")
	}
	if len(e.p.InterfaceStatus()) != 0 {
		t.Errorf("status lists interfaces without definitions")
	}
	for _, k := range e.p.Live().Keys() {
		if strings.HasPrefix(k, stats.InterfaceState) {
			t.Errorf("gauge %s left after the definitions went", k)
		}
	}
	watches = src.WatchCalls()
	if err := e.p.Reload(mustParse(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if src.WatchCalls() != watches+1 || src.ActiveWatches() != 1 {
		t.Errorf("Watch calls %d, active %d", src.WatchCalls()-watches, src.ActiveWatches())
	}

	// A proxy built without a source refuses a reload that adds a
	// definition; serve shows the error as last_reload_error.
	bare := newEnv(t, baseConfig)
	err := bare.p.Reload(mustParse(t, cfg))
	if err == nil || !strings.Contains(err.Error(), "interface definitions need an interface source") {
		t.Errorf("reload without a source: %v", err)
	}
	if bare.p.Config().Interfaces != nil {
		t.Errorf("refused configuration applied")
	}
}

// byteServer reads exactly up bytes, then writes down bytes and closes.
func byteServer(t *testing.T, up, down int) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.CopyN(io.Discard, c, int64(up)); err != nil {
					return
				}
				c.Write(make([]byte, down))
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort()
}

// TestInterfaceCounters checks interface_bytes_total and the empty
// interface field (design 0008 D15, I14).
func TestInterfaceCounters(t *testing.T) {
	const up, down = 65536, 1048576
	cfg := ifaceConfig("", `
[[rule]]
name = "pin"
hosts = ["bulk.cuttle.test"]
action = "direct"
interfaces = ["eth-b"]

[[rule]]
name = "blocked"
domains = ["blocked.cuttle.test"]
action = "block"
`)
	e, _, _ := newIfaceEnv(t, cfg, envOptions{})
	e.net.Map("bulk.cuttle.test", "203.0.113.50")
	e.net.Route("203.0.113.50:9000", byteServer(t, up, down))
	c := e.connectRaw("bulk.cuttle.test:9000")
	if _, err := c.Write(make([]byte, up)); err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, c)
	if err != nil || n != down {
		t.Fatalf("read %d %v", n, err)
	}
	c.Close()
	d := e.lastDecision(1)
	checkDecision(t, d, map[string]string{"interface": "eth-b", "up": fmt.Sprint(up), "down": fmt.Sprint(down)})
	live := e.p.Live()
	if u, dn := live.Get(stats.InterfaceBytesTotal, "interface", "eth-b", "direction", "up"),
		live.Get(stats.InterfaceBytesTotal, "interface", "eth-b", "direction", "down"); u != up || dn != down {
		t.Errorf("interface_bytes_total eth-b up %d down %d", u, dn)
	}

	// Empty for a block and a loopback destination; skipped absent when
	// group 0 carried the connection.
	res, _ := e.raw(connectReq("blocked.cuttle.test:443"))
	if res.StatusCode != 403 {
		t.Errorf("block: %d", res.StatusCode)
	}
	checkDecision(t, e.lastDecision(2), map[string]string{"outcome": OutcomeBlocked, "interface": ""})
	e.net.Route("127.0.0.1:9001", e.http.Addr)
	e.connectClose("127.0.0.1:9001")
	d = e.lastDecision(3)
	checkDecision(t, d, map[string]string{"rule": "loopback", "interface": "", "outcome": "ok"})
	checkSkipped(t, d, "")

	// Without definitions no interface_* counter exists and the field is
	// empty.
	plain := newEnv(t, baseConfig)
	plain.connectClose("origin.cuttle.test:443")
	checkDecision(t, plain.lastDecision(1), map[string]string{"interface": "", "outcome": "ok"})
	for _, k := range plain.p.Live().Keys() {
		if strings.HasPrefix(k, "interface_") {
			t.Errorf("counter %s without definitions", k)
		}
	}
}

// TestInterfaceTableFailures checks re-reads after stale-table dial errors
// and the handling of an unreadable table (design 0008 D5, I18).
func TestInterfaceTableFailures(t *testing.T) {
	cfg := ifaceConfig(`interfaces = ["eth-a", "eth-b"]
interface_attempt_timeout = "300ms"
interface_demotion = "0s"
`, "")
	t.Run("stale table", func(t *testing.T) {
		for _, errno := range []syscall.Errno{syscall.ENXIO, syscall.EADDRNOTAVAIL, syscall.ENETDOWN} {
			e, src, timers := newIfaceEnv(t, cfg, envOptions{})
			before := src.Reads()
			e.net.OnInterface("eth-a", "", testenv.Errno(errno))
			e.connectClose("origin.cuttle.test:443")
			checkDecision(t, e.lastDecision(1), map[string]string{"interface": "eth-b"})
			if !timers.WaitPending(2, 2*time.Second) {
				t.Fatalf("%v: no re-read scheduled", errno)
			}
			timers.Advance(iface.Debounce - time.Millisecond)
			if src.Reads() != before {
				t.Errorf("%v: re-read before 200 ms", errno)
			}
			timers.Advance(time.Millisecond)
			if src.Reads() != before+1 {
				t.Errorf("%v: reads %d, want %d", errno, src.Reads(), before+1)
			}
		}
		// A refused dial shows no stale table.
		e, _, timers := newIfaceEnv(t, cfg, envOptions{})
		e.net.OnInterface("eth-a", "", testenv.Errno(syscall.ENETUNREACH))
		e.connectClose("origin.cuttle.test:443")
		e.waitDecisions(1)
		if timers.Pending() != 1 {
			t.Errorf("ENETUNREACH scheduled a re-read: %d timers", timers.Pending())
		}
	})

	t.Run("unreadable at start", func(t *testing.T) {
		src := testenv.NewInterfaces(ifaceRows()...)
		src.FailRead(errors.New("injected read failure"))
		e, _, _ := newIfaceEnv(t, cfg, envOptions{interfaces: src})
		for _, s := range e.p.InterfaceStatus() {
			if s.State != iface.StateOffline || s.Reason != iface.ReasonUnknown {
				t.Errorf("%s: %s %s", s.Name, s.State, s.Reason)
			}
		}
		if n := strings.Count(e.logs.String(), `msg="interface table unreadable" error="injected read failure"`); n != 1 {
			t.Errorf("%d warnings:\n%s", n, e.logs.String())
		}
		res, _ := e.raw(connectReq("origin.cuttle.test:443"))
		generatedHeaders(t, res, 503, `cuttle-test; error=destination_unavailable; details="no usable interface (eth-a: unknown, eth-b: unknown)"`)
		checkDecision(t, e.lastDecision(1), map[string]string{"outcome": OutcomeNoInterface})
		wantDials(t, e.net)
	})

	t.Run("unreadable later", func(t *testing.T) {
		e, src, timers := newIfaceEnv(t, cfg, envOptions{})
		src.FailRead(errors.New("injected read failure"))
		reread(t, src, timers)
		reread(t, src, timers)
		e.connectClose("origin.cuttle.test:443")
		checkDecision(t, e.lastDecision(1), map[string]string{"interface": "eth-a", "outcome": "ok"})
		if n := strings.Count(e.logs.String(), `msg="interface table re-read failed; keeping the previous table"`); n != 1 {
			t.Errorf("%d warnings for one run of failures:\n%s", n, e.logs.String())
		}
		src.FailRead(nil)
		reread(t, src, timers)
		src.FailRead(errors.New("injected read failure"))
		reread(t, src, timers)
		if n := strings.Count(e.logs.String(), `msg="interface table re-read failed; keeping the previous table"`); n != 2 {
			t.Errorf("%d warnings for two runs of failures", n)
		}
	})
}
