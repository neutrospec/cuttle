package stats

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatsWindows checks the dashboard windows of design 0005 D9: a
// preset covers whole hours up to the end of the current one; a date
// range covers the hours that start within its local dates, cut to the
// retention period and the current hour.
func TestStatsWindows(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 34, 56, 0, time.UTC)
	cur := Bucket(now)
	for preset, hours := range map[string]int64{"1h": 1, "24h": 24, "7d": 168, "30d": 720} {
		w, err := PresetWindow(preset, now)
		if err != nil || w.To != cur+3600 || w.From != cur+3600-hours*3600 || w.Preset != preset {
			t.Errorf("%s: %+v %v", preset, w, err)
		}
	}
	for _, bad := range []string{"", "2h", "24H", "1d"} {
		if _, err := PresetWindow(bad, now); err == nil {
			t.Errorf("preset %q accepted", bad)
		}
	}

	day := func(s string, loc *time.Location) int64 {
		d, err := time.ParseInLocation(DateLayout, s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return d.Unix()
	}
	east := time.FixedZone("UTC+3", 3*3600)
	half := time.FixedZone("UTC+5:30", 5*3600+1800)
	for _, c := range []struct {
		name, from, to string
		loc            *time.Location
		wantFrom       int64
		wantTo         int64
	}{
		{"one past day", "2026-11-20", "2026-11-20", time.UTC, day("2026-11-20", time.UTC), day("2026-11-21", time.UTC)},
		{"today ends at the current hour", "2026-11-24", "2026-11-25", time.UTC, day("2026-11-24", time.UTC), cur + 3600},
		{"local dates", "2026-11-20", "2026-11-21", east, day("2026-11-20", east), day("2026-11-22", east)},
		{"half-hour zone takes whole hours", "2026-11-20", "2026-11-20", half, day("2026-11-20", half) + 1800, day("2026-11-21", half) + 1800},
		{"cut to the retention", "2026-10-01", "2026-10-30", time.UTC, cur - 30*86400, day("2026-10-31", time.UTC)},
	} {
		w, err := DateWindow(c.from, c.to, now, c.loc, 30)
		if err != nil || w.From != c.wantFrom || w.To != c.wantTo || w.Preset != "" || w.FromDate != c.from || w.ToDate != c.to {
			t.Errorf("%s: %+v %v, want [%d, %d)", c.name, w, err, c.wantFrom, c.wantTo)
		}
		if w.From%3600 != 0 || w.To%3600 != 0 {
			t.Errorf("%s: not whole hours: %+v", c.name, w)
		}
	}
	for _, c := range [][2]string{{"2026-11-25", "2026-11-24"}, {"2026-13-01", "2026-13-02"}, {"25.11.2026", "2026-11-25"},
		{"2026-01-01", "2026-01-02"}, {"2026-11-26", "2026-11-27"}} {
		if w, err := DateWindow(c[0], c[1], now, time.UTC, 30); err == nil {
			t.Errorf("%v accepted: %+v", c, w)
		}
	}
}

// TestStatsFilterCheck checks the filter values the queries accept.
func TestStatsFilterCheck(t *testing.T) {
	w, _ := PresetWindow("24h", time.Unix(7200, 0))
	for _, f := range []Filter{
		{Window: w}, {Window: w, Route: "all", Source: "all", Interface: "all"},
		{Window: w, Route: "upstream:gateway", Source: "remote", Interface: "eth-a"},
		{Window: w, Route: "block", Interface: "none"}, {Window: w, Route: "upstream", Interface: "other"},
		{Window: w, Rule: "all"}, {Window: w, Rule: "restricted"}, {Window: w, Rule: "default"}, {Window: w, Rule: "lab.v2_x-1"},
	} {
		if err := f.Check(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
	for _, f := range []Filter{
		{Window: w, Route: "upstream:"}, {Window: w, Route: "upstream:Gateway"}, {Window: w, Route: "none"},
		{Window: w, Source: "both"}, {Window: w, Interface: "en0 "}, {Window: w, Interface: "Wi-Fi"}, {},
		{Window: w, Rule: "Ads"}, {Window: w, Rule: "-x"}, {Window: w, Rule: "x'; DROP"}, {Window: w, Rule: strings.Repeat("a", 65)},
	} {
		if err := f.Check(); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
}

// TestStatsLastWriteError checks that a failed flush is kept with its time
// for cuttle status and the dashboard, and stays after a later success
// (OI-0006 Q-R5).
func TestStatsLastWriteError(t *testing.T) {
	clock := newClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "stats.db"), RetentionDays: 30,
		MaxHostsPerHour: 1000, Clock: clock.Now, Interval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	if st.LastWriteError() != nil {
		t.Fatal("write error before any flush")
	}
	st.Recorder().Complete(&flow{key: blockKey("a.example")}, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.Flush(ctx); err == nil {
		t.Fatal("flush with a cancelled context succeeded")
	}
	we := st.LastWriteError()
	if we == nil || !strings.Contains(we.Msg, "context canceled") || !we.At.Equal(clock.Now()) {
		t.Fatalf("last write error %+v", we)
	}
	clock.Set(clock.Now().Add(time.Minute))
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.Rows(context.Background()); len(rows) != 1 {
		t.Errorf("rows after the retry: %+v", rows)
	}
	if got := st.LastWriteError(); got == nil || !got.At.Equal(we.At) {
		t.Errorf("a successful flush changed the last write error: %+v", got)
	}
}

// TestStatsQueryDuringBlockedFlush checks that the dashboard queries run
// on their own read-only connections: while another connection holds
// BEGIN EXCLUSIVE and a flush waits for the lock on the writer
// connection, a query still answers from the written rows.
func TestStatsQueryDuringBlockedFlush(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a lock")
	}
	clock := newClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := Open(context.Background(), Options{Path: path, RetentionDays: 30, MaxHostsPerHour: 1000, Clock: clock.Now, Interval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	st.Recorder().Complete(&flow{key: blockKey("written.example")}, true, false)
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The query connection is read-only.
	if _, err := st.store.reader.Exec("DELETE FROM host_hour"); err == nil {
		t.Error("the reader connection wrote")
	}

	lock, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	lock.SetMaxOpenConns(1)
	conn, err := lock.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	st.Recorder().Complete(&flow{key: blockKey("pending.example")}, true, false)
	flushed := make(chan error, 1)
	go func() { flushed <- st.Flush(context.Background()) }()
	time.Sleep(200 * time.Millisecond) // the flush now waits for the lock

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w, _ := PresetWindow("24h", clock.Now())
	rows, err := st.Domains(ctx, Filter{Window: w}, MaxDomainRows, 0)
	if err != nil || len(rows) != 1 || rows[0].Host != "written.example" {
		t.Errorf("query during the blocked flush: %+v %v", rows, err)
	}
	totals, err := st.RouteTotals(ctx, Filter{Window: w})
	if err != nil || len(totals) != 1 || totals[0].Requests != 1 {
		t.Errorf("totals during the blocked flush: %+v %v", totals, err)
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("flush after the lock: %v", err)
	}
	rows, _ = st.Domains(context.Background(), Filter{Window: w}, MaxDomainRows, 0)
	if len(rows) != 2 {
		t.Errorf("rows after the flush: %+v", rows)
	}
}

// TestStatsRuleTotals checks the totals per rule of design 0009 D9 and the
// rule filter: each rule's requests, failures, bytes, and distinct hosts
// in the window, ordered by name; the filter narrows the totals and the
// host table to one rule.
func TestStatsRuleTotals(t *testing.T) {
	clock := newClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "stats.db"), RetentionDays: 30,
		MaxHostsPerHour: 1000, Clock: clock.Now, Interval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	rec := func(host, action, upstream, rule string, n int, up, down int64, failed bool) {
		for range n {
			st.Recorder().Complete(&flow{key: RowKey{Host: host, Action: action, Upstream: upstream, Rule: rule, Source: "local"}, up: up, down: down}, true, failed)
		}
	}
	rec("wiki.internal.example", "upstream", "gateway", "restricted", 3, 10, 100, false)
	rec("git.internal.example", "upstream", "gateway", "restricted", 2, 5, 50, true)
	rec("ads.example", "block", "", "ads", 4, 0, 0, false)
	rec("example.com", "direct", "", "default", 1, 1, 2, false)
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// An hour earlier than the 1h window, inside the 24h window.
	clock.Set(clock.Now().Add(-2 * time.Hour))
	rec("old.internal.example", "upstream", "gateway", "restricted", 1, 1, 1, false)
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Set(clock.Now().Add(2 * time.Hour))

	day, _ := PresetWindow("24h", clock.Now())
	got, err := st.RuleTotals(context.Background(), Filter{Window: day})
	if err != nil {
		t.Fatal(err)
	}
	want := []Total{
		{Rule: "ads", Hosts: 1, Requests: 4},
		{Rule: "default", Hosts: 1, Requests: 1, BytesUp: 1, BytesDown: 2},
		{Rule: "restricted", Hosts: 3, Requests: 6, Failures: 2, BytesUp: 41, BytesDown: 401},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rule totals:\n got %+v\nwant %+v", got, want)
	}
	hour, _ := PresetWindow("1h", clock.Now())
	got, _ = st.RuleTotals(context.Background(), Filter{Window: hour, Rule: "restricted"})
	if len(got) != 1 || got[0].Rule != "restricted" || got[0].Requests != 5 || got[0].Hosts != 2 {
		t.Errorf("restricted in the last hour: %+v", got)
	}
	rows, err := st.Domains(context.Background(), Filter{Window: day, Rule: "restricted"}, 10, 0)
	if err != nil || len(rows) != 3 || rows[0].Host != "wiki.internal.example" {
		t.Errorf("hosts of restricted: %+v %v", rows, err)
	}
	for _, r := range rows {
		if len(r.Rules) != 1 || r.Rules[0] != "restricted" {
			t.Errorf("a row of another rule: %+v", r)
		}
	}
	if _, err := st.RuleTotals(context.Background(), Filter{Window: day, Rule: "Bad Name"}); err == nil {
		t.Error("an invalid rule filter was accepted")
	}
}
