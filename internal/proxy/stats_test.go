package proxy

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"squid/internal/stats"
	"squid/internal/testenv"
)

// openStats opens a statistics service on a temporary database with the
// clock and no flush timer, so the test flushes with Flush (design 0005
// D4).
func openStats(t *testing.T, clock func() time.Time, live *stats.Live) (*stats.Stats, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := stats.Open(context.Background(), stats.Options{
		Path: path, RetentionDays: 30, MaxHostsPerHour: 1000, Live: live, Clock: clock, Interval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(context.Background()) })
	return st, path
}

// flushRows writes the pending rows and returns every row.
func flushRows(t *testing.T, st *stats.Stats) []stats.Row {
	t.Helper()
	ctx := context.Background()
	if err := st.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rows, err := st.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// rowCounts is a row without its times, for comparisons.
type rowCounts struct {
	bucket int64
	stats.RowKey
	connects, httpRequests, failures, up, down int64
}

func countsOf(rows []stats.Row) []rowCounts {
	out := make([]rowCounts, len(rows))
	for i, r := range rows {
		out[i] = rowCounts{r.Bucket, r.RowKey, r.Connects, r.HTTPRequests, r.Failures, r.BytesUp, r.BytesDown}
	}
	return out
}

// echoServer writes back every byte it reads.
func echoServer(t *testing.T) netip.AddrPort {
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
				c.SetDeadline(time.Now().Add(20 * time.Second))
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort()
}

// TestStatsRowsMatchTraffic checks S1: after direct CONNECT tunnels to two
// hosts, one pinned to a test interface, plain HTTP through upstream
// gateway, blocked requests, and a failed dial, the host_hour rows hold the
// expected counts, bytes, and interfaces, and agree with the live counters
// (design 0005 D1, D4).
func TestStatsRowsMatchTraffic(t *testing.T) {
	cfg := ifaceConfig("", `
[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"

[[rule]]
name = "restricted"
hosts = ["h"]
action = "upstream:gateway"

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
	clock := newFakeClock()
	clock.Advance(10*time.Hour + 20*time.Minute)
	bucket := stats.Bucket(clock.Now())
	live := stats.NewLive()
	st, _ := openStats(t, clock.Now, live)
	e, _, _ := newIfaceEnv(t, cfg, envOptions{stats: st.Recorder(), live: live})
	gw := e.recorder("10.8.0.6:3128", testenv.UpstreamOptions{})
	gw.SetMode("h", testenv.Pass())
	e.net.Map("bulk.cuttle.test", "203.0.113.50")
	e.net.Route("203.0.113.50:9000", byteServer(t, 1000, 5000))
	e.net.Map("files.cuttle.test", "203.0.113.51")
	e.net.Route("203.0.113.51:9000", byteServer(t, 300, 700))

	tunnel := func(target string, up, down int) {
		t.Helper()
		c := e.connectRaw(target)
		if _, err := c.Write(make([]byte, up)); err != nil {
			t.Fatal(err)
		}
		if n, err := io.Copy(io.Discard, c); err != nil || n != int64(down) {
			t.Fatalf("%s: read %d %v", target, n, err)
		}
		c.Close()
	}
	status := func(req string, want int) {
		t.Helper()
		if res, _ := e.raw(req); res.StatusCode != want {
			t.Fatalf("%q: status %d, want %d", firstLine(req), res.StatusCode, want)
		}
	}
	tunnel("bulk.cuttle.test:9000", 1000, 5000)
	tunnel("bulk.cuttle.test:9000", 1000, 5000)
	tunnel("files.cuttle.test:9000", 300, 700)
	for range 3 {
		status(plainGet("h", "/bytes?n=1234"), 200)
	}
	status("POST http://h/sink HTTP/1.1\r\nHost: h\r\nContent-Length: 500\r\nConnection: close\r\n\r\n"+strings.Repeat("x", 500), 200)
	status(connectReq("blocked.cuttle.test:443"), 403)
	status(connectReq("blocked.cuttle.test:443"), 403)
	status(plainGet("x.blocked.cuttle.test", "/"), 403)
	status(connectReq("nowhere.cuttle.test:443"), 502)
	// A refused target has no host and no row (design 0005 D1).
	status("GET /origin-form HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n", 400)
	ds := e.waitDecisions(12)
	for _, d := range ds {
		if d["target"] == "bulk.cuttle.test:9000" && d["interface"] != "eth-b" ||
			(d["target"] == "files.cuttle.test:9000" || d["target"] == "h:80") && d["interface"] != "other" {
			t.Errorf("decision interface %q for %s", d["interface"], d["target"])
		}
	}

	rows := flushRows(t, st)
	key := func(host, action, upstream, rule, iface string) stats.RowKey {
		return stats.RowKey{Host: host, Action: action, Upstream: upstream, Rule: rule, Source: "local", Interface: iface}
	}
	want := []rowCounts{
		{bucket, key("blocked.cuttle.test", "block", "", "blocked", ""), 2, 0, 0, 0, 0},
		{bucket, key("bulk.cuttle.test", "direct", "", "pin", "eth-b"), 2, 0, 0, 2000, 10000},
		{bucket, key("files.cuttle.test", "direct", "", "default", "other"), 1, 0, 0, 300, 700},
		{bucket, key("h", "upstream", "gateway", "restricted", "other"), 0, 4, 0, 500, 3*1234 + int64(len("500"))},
		{bucket, key("nowhere.cuttle.test", "direct", "", "default", ""), 1, 0, 1, 0, 0},
		{bucket, key("x.blocked.cuttle.test", "block", "", "blocked", ""), 0, 1, 0, 0, 0},
	}
	if got := countsOf(rows); !slices.Equal(got, want) {
		t.Errorf("rows:\n got %+v\nwant %+v", got, want)
	}
	for _, r := range rows {
		if now := clock.Now().Unix(); r.FirstSeen != now || r.LastSeen != now {
			t.Errorf("%s: first_seen %d last_seen %d, want %d", r.Host, r.FirstSeen, r.LastSeen, now)
		}
	}

	// The rows agree with the live counters: every decision with an action,
	// the bytes per action, the bytes per interface, and the failures.
	sums := map[string]int64{}
	var failures int64
	for _, r := range rows {
		sums[stats.Key(stats.DecisionsTotal, "kind", "connect", "action", r.Action)] += r.Connects
		sums[stats.Key(stats.DecisionsTotal, "kind", "http", "action", r.Action)] += r.HTTPRequests
		sums[stats.Key(stats.BytesTotal, "action", r.Action, "direction", "up")] += r.BytesUp
		sums[stats.Key(stats.BytesTotal, "action", r.Action, "direction", "down")] += r.BytesDown
		if r.Interface != "" {
			sums[stats.Key(stats.InterfaceBytesTotal, "interface", r.Interface, "direction", "up")] += r.BytesUp
			sums[stats.Key(stats.InterfaceBytesTotal, "interface", r.Interface, "direction", "down")] += r.BytesDown
		}
		failures += r.Failures
	}
	snap := live.Snapshot()
	for k, v := range sums {
		if snap[k] != v {
			t.Errorf("live %s = %d, rows sum to %d", k, snap[k], v)
		}
	}
	for k, v := range snap {
		counted := strings.HasPrefix(k, stats.DecisionsTotal+"{") && !strings.Contains(k, `action="none"`) ||
			strings.HasPrefix(k, stats.BytesTotal+"{") || strings.HasPrefix(k, stats.InterfaceBytesTotal+"{")
		if counted && v != 0 && sums[k] != v {
			t.Errorf("live %s = %d, rows sum to %d", k, v, sums[k])
		}
	}
	var liveFailures int64
	for outcome := range failedOutcomes {
		liveFailures += live.Get(stats.OutcomesTotal, "outcome", outcome)
	}
	if failures != liveFailures || failures != 1 {
		t.Errorf("failures: rows %d, live %d, want 1", failures, liveFailures)
	}
	if n := live.Get(stats.DecisionsTotal, "kind", "http", "action", "none"); n != 1 {
		t.Errorf("refused target: decisions_total action=none %d, want 1", n)
	}
}

// TestStatsNeverBlocksForwarding checks S2: while another connection holds
// BEGIN EXCLUSIVE on the database and a flush waits for the lock, 100
// proxied requests complete; a flush that gives up returns its rows, which
// are written once the lock is released (design 0005 D4).
func TestStatsNeverBlocksForwarding(t *testing.T) {
	ctx := context.Background()
	live := stats.NewLive()
	st, path := openStats(t, nil, live)
	e := newEnvWith(t, baseConfig, envOptions{stats: st.Recorder(), live: live})
	for range 10 {
		if res, _ := e.raw(connectReq("blocked.cuttle.test:443")); res.StatusCode != 403 {
			t.Fatalf("warm-up: %d", res.StatusCode)
		}
	}
	e.waitDecisions(10)

	lockDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer lockDB.Close()
	lock, err := lockDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			lock.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// A flush that gives up returns its rows to the recorder.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	err = st.Flush(short)
	cancel()
	if err == nil {
		t.Fatal("flush succeeded while the database was locked")
	}
	if n := live.Get(stats.StatsWriteErrorsTotal); n != 1 {
		t.Errorf("stats_write_errors_total %d, want 1", n)
	}

	// A flush that waits for the lock while the requests run.
	flushed := make(chan error, 1)
	go func() { flushed <- st.Flush(ctx) }()
	time.Sleep(50 * time.Millisecond)
	c, _ := e.goClient()
	start := time.Now()
	for i := range 100 {
		if i%2 == 0 {
			if res, _ := e.raw(connectReq("blocked.cuttle.test:443")); res.StatusCode != 403 {
				t.Fatalf("request %d: %d", i, res.StatusCode)
			}
			continue
		}
		res, err := c.Get("http://plain.cuttle.test/bytes?n=100")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	e.waitDecisions(110)
	elapsed := time.Since(start)
	select {
	case err := <-flushed:
		t.Fatalf("the waiting flush ended while the lock was held (%v), after %v", err, elapsed)
	default:
	}
	// SQLite's busy timeout is 5 s; the requests never waited for it.
	if elapsed > 3*time.Second {
		t.Errorf("100 requests took %v while the database was locked", elapsed)
	}
	t.Logf("100 requests completed in %v while the database was locked", elapsed)

	if _, err := lock.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := <-flushed; err != nil {
		t.Fatalf("flush after the lock was released: %v", err)
	}
	rows := flushRows(t, st)
	var blocked, plain int64
	for _, r := range rows {
		switch r.Host {
		case "blocked.cuttle.test":
			blocked += r.Connects
		case "plain.cuttle.test":
			plain += r.HTTPRequests
			if r.BytesDown != 50*100 {
				t.Errorf("plain.cuttle.test bytes_down %d, want %d", r.BytesDown, 50*100)
			}
		}
	}
	if blocked != 60 || plain != 50 {
		t.Errorf("connects %d (want 60), http_requests %d (want 50): %+v", blocked, plain, rows)
	}
	if n := live.Get(stats.StatsWriteErrorsTotal); n != 1 {
		t.Errorf("stats_write_errors_total %d, want 1", n)
	}
}

// TestStatsHourAttribution checks S3: a tunnel open across an hour
// boundary adds its bytes to the hour in which each flush found them, and
// its decision to the hour in which it completed (design 0005 D4).
func TestStatsHourAttribution(t *testing.T) {
	clock := newFakeClock()
	clock.Advance(10*time.Hour + 59*time.Minute + 50*time.Second)
	st, _ := openStats(t, clock.Now, nil)
	e := newEnvWith(t, baseConfig, envOptions{stats: st.Recorder()})
	e.net.Map("echo.cuttle.test", "203.0.113.60")
	e.net.Route("203.0.113.60:9000", echoServer(t))

	c := e.connectRaw("echo.cuttle.test:9000")
	echo := func(n int, total int64) {
		t.Helper()
		if _, err := c.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
			t.Fatal(err)
		}
		// The proxy counts a write once it returns, which can be after the
		// client read the bytes.
		deadline := time.Now().Add(5 * time.Second)
		for {
			ts := e.p.Tunnels()
			if len(ts) == 1 && ts[0].Up == total && ts[0].Down == total {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("tunnel counts %+v, want %d each way", ts, total)
			}
			time.Sleep(time.Millisecond)
		}
	}
	echo(1000, 1000)
	clock.Advance(5 * time.Second) // 10:59:55
	h10 := stats.Bucket(clock.Now())
	rows := flushRows(t, st)
	key := stats.RowKey{Host: "echo.cuttle.test", Action: "direct", Rule: "default", Source: "local"}
	if got, want := countsOf(rows), []rowCounts{{h10, key, 0, 0, 0, 1000, 1000}}; !slices.Equal(got, want) {
		t.Fatalf("before the boundary:\n got %+v\nwant %+v", got, want)
	}

	clock.Advance(10 * time.Second) // 11:00:05
	h11 := stats.Bucket(clock.Now())
	echo(2000, 3000)
	c.Close()
	checkDecision(t, e.lastDecision(1), map[string]string{"up": "3000", "down": "3000", "outcome": "ok"})
	rows = flushRows(t, st)
	want := []rowCounts{{h10, key, 0, 0, 0, 1000, 1000}, {h11, key, 1, 0, 0, 2000, 2000}}
	if got := countsOf(rows); !slices.Equal(got, want) {
		t.Errorf("after the boundary:\n got %+v\nwant %+v", got, want)
	}
	if h11-h10 != 3600 {
		t.Errorf("buckets %d and %d are not consecutive hours", h10, h11)
	}
	for _, r := range rows {
		if r.Bucket == h11 && (r.FirstSeen != clock.Now().Unix() || r.LastSeen != clock.Now().Unix()) {
			t.Errorf("hour 11: first_seen %d last_seen %d", r.FirstSeen, r.LastSeen)
		}
	}
}
