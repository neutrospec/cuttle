package admin

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"squid/internal/control"
	"squid/internal/stats"
)

// testFlow is a completed exchange with fixed bytes.
type testFlow struct {
	key      stats.RowKey
	up, down int64
}

func (f *testFlow) StatsKey() stats.RowKey     { return f.key }
func (f *testFlow) StatsBytes() (int64, int64) { return f.up, f.down }

// withStats gives the fixture's backend a statistics service on a
// temporary database with the fixture's clock and no flush timer.
func (f *fixture) withStats() *stats.Stats {
	f.t.Helper()
	st, err := stats.Open(context.Background(), stats.Options{
		Path: filepath.Join(f.t.TempDir(), "stats.db"), RetentionDays: 30, MaxHostsPerHour: 1000,
		Clock: f.clock.Now, Interval: -1,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { st.Close(context.Background()) })
	f.be.mu.Lock()
	f.be.st = st
	f.be.mu.Unlock()
	return st
}

// record completes n decisions for a key with the bytes given per
// decision.
func record(st *stats.Stats, k stats.RowKey, n int, connect, failed bool, up, down int64) {
	for range n {
		st.Recorder().Complete(&testFlow{key: k, up: up, down: down}, connect, failed)
	}
}

func flush(t *testing.T, st *stats.Stats) {
	t.Helper()
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

var hostCell = regexp.MustCompile(`<td class="host">([^<]+)</td>`)

// hosts returns the host cells of a page in order.
func hosts(body string) []string {
	var out []string
	for _, m := range hostCell.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestAdminDashboard checks the overview, domain, and live pages against
// a statistics store with known rows (design 0005 D9, A8): the route and
// interface totals, the top hosts per route, the filters, the windows
// and their exact ranges, paging, invalid parameters, and the statistics
// states, the last write error included (OI-0006 Q-R5).
func TestAdminDashboard(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	b.login()

	// Without statistics the pages say so and still render.
	for _, p := range []string{"/", "/domains"} {
		res := b.get(p)
		if body := bodyOf(t, res); res.StatusCode != http.StatusOK || !strings.Contains(body, "Statistics are unavailable: test backend.") {
			t.Errorf("%s without statistics: %d:\n%s", p, res.StatusCode, body)
		}
	}

	st := f.withStats()
	key := func(host, action, upstream, rule, source, iface string) stats.RowKey {
		return stats.RowKey{Host: host, Action: action, Upstream: upstream, Rule: rule, Source: source, Interface: iface}
	}
	// Two hours ago: a host that only the longer windows show.
	f.clock.Add(-2 * time.Hour)
	record(st, key("old.example", "direct", "", "default", "local", "eth-a"), 1, true, false, 10, 20)
	flush(t, st)
	f.clock.Add(2 * time.Hour)
	record(st, key("big.example", "direct", "", "default", "local", "eth-a"), 2, true, false, 1000, 5000)
	record(st, key("big.example", "direct", "", "pinned", "local", "eth-a"), 1, true, false, 0, 0)
	record(st, key("small.example", "direct", "", "default", "local", "other"), 1, false, false, 100, 200)
	record(st, key("wiki.internal.example", "upstream", "gateway", "restricted", "local", "other"), 3, false, false, 50, 70)
	record(st, key("ads.example", "block", "", "ads", "local", ""), 4, true, false, 0, 0)
	record(st, key("remote.example", "direct", "", "remote-direct", "remote", "eth-a"), 1, true, true, 0, 0)
	flush(t, st)

	res := b.get("/")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "Statistics are unavailable") {
		t.Fatalf("overview: %d:\n%s", res.StatusCode, body)
	}
	// The default window is 24h, the current hour and the 23 before it.
	for _, want := range []string{
		`<a href="/?window=24h" aria-current="true">24h</a>`,
		`2026-11-24 13:00 to 2026-11-25 13:00 UTC`,
		`<td class="route">direct</td><td class="n">4</td><td class="n">6</td><td class="n">1</td><td class="n">2.1 KiB</td><td class="n">10.0 KiB</td>`,
		`<td class="route">upstream:gateway</td><td class="n">1</td><td class="n">3</td><td class="n">0</td><td class="n">150 B</td><td class="n">210 B</td>`,
		`<td class="route">block</td><td class="n">1</td><td class="n">4</td>`,
		`<h2>Sent direct</h2>`, `<h2>Sent to upstream gateway</h2>`, `<h2>Blocked</h2>`,
		`<a href="/domains?route=upstream%3Agateway&amp;window=24h">All 1 hosts on upstream:gateway</a>`,
		`<h2>Interfaces</h2>`,
		`<td>eth-a</td><td class="n">3</td>`,
		`<td><span class="muted">none</span></td><td class="n">1</td><td class="n">4</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	// The routes come in the order direct, upstreams, block, and each
	// route's hosts by bytes, then requests.
	if got := strings.Join(hosts(body), ","); got != "big.example,small.example,old.example,remote.example,wiki.internal.example,ads.example" {
		t.Errorf("overview hosts %s", got)
	}
	res = b.get("/?window=1h")
	body = bodyOf(t, res)
	if got := strings.Join(hosts(body), ","); res.StatusCode != http.StatusOK || got != "big.example,small.example,remote.example,wiki.internal.example,ads.example" ||
		!strings.Contains(body, "2026-11-25 12:00 to 2026-11-25 13:00 UTC") {
		t.Errorf("1h: %d %s", res.StatusCode, got)
	}

	domains := func(query string) (int, []string, string) {
		t.Helper()
		res := b.get("/domains?" + query)
		body := bodyOf(t, res)
		return res.StatusCode, hosts(body), body
	}
	for _, c := range []struct{ query, want string }{
		{"", "big.example,wiki.internal.example,small.example,old.example,ads.example,remote.example"},
		{"route=upstream:gateway", "wiki.internal.example"},
		{"route=upstream", "wiki.internal.example"},
		{"route=direct", "big.example,small.example,old.example,remote.example"},
		{"route=block", "ads.example"},
		{"route=direct&window=1h", "big.example,small.example,remote.example"},
		{"source=remote", "remote.example"},
		{"source=local&route=direct", "big.example,small.example,old.example"},
		{"interface=eth-a", "big.example,old.example,remote.example"},
		{"interface=other", "wiki.internal.example,small.example"},
		{"interface=none", "ads.example"},
		{"interface=wifi", ""},
		{"route=upstream:removed", ""},
		{"from=2026-11-25&to=2026-11-25&route=direct", "big.example,small.example,old.example,remote.example"},
		{"from=2026-11-24&to=2026-11-24", ""},
	} {
		code, got, body := domains(c.query)
		if code != http.StatusOK || strings.Join(got, ",") != c.want {
			t.Errorf("/domains?%s: %d %v, want %s", c.query, code, got, c.want)
			if code != http.StatusOK {
				t.Log(body)
			}
		}
	}
	// One row per host and route, with its rules, requests, failures,
	// bytes, and times; the date range shows its exact hours.
	_, _, body = domains("route=direct&from=2026-11-25&to=2026-11-25")
	for _, want := range []string{
		`<td class="host">big.example</td><td class="route">direct</td><td>default, pinned</td><td class="n">3</td><td class="n">0</td><td class="n">2.0 KiB</td><td class="n">9.8 KiB</td><td>2026-11-25 12:00</td><td>2026-11-25 12:00</td>`,
		`<td class="host">remote.example</td><td class="route">direct</td><td>remote-direct</td><td class="n">1</td><td class="n">1</td>`,
		`2026-11-25 00:00 to 2026-11-25 13:00 UTC`,
		`<option value="upstream:gateway">upstream:gateway</option>`,
		`<option value="direct" selected>direct</option>`,
		`name="from" value="2026-11-25"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/domains lacks %q:\n%s", want, body)
		}
	}

	// Invalid parameters get 400 with the reason.
	for _, q := range []string{"route=bogus", "route=upstream:", "source=elsewhere", "interface=Wi-Fi", "window=2h",
		"from=2026-11-25", "from=yesterday&to=2026-11-25", "from=2026-11-25&to=2026-11-24", "from=2025-01-01&to=2025-01-02",
		"offset=7", "offset=-500"} {
		if code, _, _ := domains(q); code != http.StatusBadRequest {
			t.Errorf("/domains?%s: %d, want 400", q, code)
		}
	}
	if res := b.get("/?window=2d"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("/?window=2d: %d", res.StatusCode)
	}

	// Paging: 501 hosts in the current hour make two pages.
	for i := range 501 {
		record(st, key(fmt.Sprintf("h%03d.page.example", i), "direct", "", "paged", "local", ""), 1, true, false, int64(1000-i), 0)
	}
	flush(t, st)
	code, got, body := domains("source=local&interface=none&route=direct")
	if code != http.StatusOK || len(got) != 500 || got[0] != "h000.page.example" || got[499] != "h499.page.example" ||
		!strings.Contains(body, `<a href="/domains?interface=none&amp;offset=500&amp;route=direct&amp;source=local&amp;window=24h">Next 500</a>`) ||
		strings.Contains(body, "Previous 500") {
		t.Errorf("page 1: %d, %d rows", code, len(got))
	}
	code, got, body = domains("source=local&interface=none&route=direct&offset=500")
	if code != http.StatusOK || len(got) != 1 || got[0] != "h500.page.example" || !strings.Contains(body, ">501</td>") ||
		!strings.Contains(body, "Previous 500") || strings.Contains(body, "Next 500") {
		t.Errorf("page 2: %d %v", code, got)
	}

	// The last statistics write error shows on the overview.
	at := time.Date(2026, 11, 25, 11, 58, 0, 0, time.UTC)
	f.be.mu.Lock()
	f.be.status.StatsWriteError, f.be.status.StatsWriteErrorAt = "database is locked", &at
	f.be.status.Tunnels = []control.Tunnel{{Target: "big.example:443", Kind: "connect", Action: "direct", Source: "local", Interface: "eth-a", AgeS: 75, Up: 2048, Down: 4096}}
	f.be.status.Counters = map[string]int64{stats.TunnelsActive: 1}
	f.be.mu.Unlock()
	res = b.get("/")
	if body := bodyOf(t, res); !strings.Contains(body, "Last statistics write error at 2026-11-25 11:58:00 UTC: database is locked") {
		t.Errorf("overview without the write error:\n%s", body)
	}

	// /live lists the open tunnel and reloads every 5 s.
	res = b.get("/live")
	body = bodyOf(t, res)
	for _, want := range []string{
		`<meta http-equiv="refresh" content="5">`,
		`<td class="host">big.example:443</td><td>connect</td><td class="route">direct</td><td>local</td><td>eth-a</td><td class="n">1m15s</td><td class="n">2.0 KiB</td><td class="n">4.0 KiB</td>`,
		`<tr><th>tunnels_active</th><td class="n">1</td></tr>`,
	} {
		if res.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("/live lacks %q:\n%s", want, body)
		}
	}
	// Only /live refreshes.
	if res := b.get("/"); strings.Contains(bodyOf(t, res), "http-equiv") {
		t.Error("the overview refreshes")
	}
}

// TestBarAndFormat checks the bar classes and number formats.
func TestBarAndFormat(t *testing.T) {
	for _, c := range []struct {
		v, max int64
		want   string
	}{{0, 0, "w0"}, {0, 10, "w0"}, {1, 1000, "w1"}, {50, 100, "w10"}, {51, 100, "w11"}, {100, 100, "w20"}, {200, 100, "w20"}} {
		if got := barClass(c.v, c.max); got != c.want {
			t.Errorf("barClass(%d, %d) = %s, want %s", c.v, c.max, got, c.want)
		}
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 40: "3.0 TiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %s, want %s", n, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := formatNum(n); got != want {
			t.Errorf("formatNum(%d) = %s, want %s", n, got, want)
		}
	}
}
