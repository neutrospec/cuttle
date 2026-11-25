package stats

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is an injected clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// flow is a Flow with fixed bytes.
type flow struct {
	key      RowKey
	up, down int64
	mu       sync.Mutex
}

func (f *flow) StatsKey() RowKey { return f.key }

func (f *flow) StatsBytes() (int64, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.up, f.down
}

func blockKey(host string) RowKey {
	return RowKey{Host: host, Action: "block", Rule: "blocked", Source: "local"}
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestStoreSchema checks the D2 schema and PRAGMAs and the file modes of
// design 0006 D1.
func TestStoreSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "cuttle", "stats.db")
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, want := range map[string]string{
		"journal_mode": "wal", "auto_vacuum": "2", "user_version": "1", "synchronous": "1", "busy_timeout": "5000",
	} {
		if got := pragma(t, s.db, name); got != want {
			t.Errorf("PRAGMA %s = %s, want %s", name, got, want)
		}
	}
	var sqlText string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_schema WHERE name = 'host_hour'").Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sqlText, "STRICT, WITHOUT ROWID") || !strings.Contains(sqlText, "interface     TEXT    NOT NULL DEFAULT ''") {
		t.Errorf("schema:\n%s", sqlText)
	}
	row := Row{Bucket: 3600, RowKey: blockKey("a.example"), Counts: Counts{Connects: 1, FirstSeen: 3601, LastSeen: 3601}}
	if err := s.Write(ctx, []Row{row}); err != nil {
		t.Fatal(err)
	}
	row.Counts = Counts{Connects: 2, BytesUp: 5, FirstSeen: 3500, LastSeen: 3700}
	if err := s.Write(ctx, []Row{row}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Rows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Counts != (Counts{Connects: 3, BytesUp: 5, FirstSeen: 3500, LastSeen: 3700}) {
		t.Errorf("upsert: %+v", rows)
	}
	// The CHECK constraints refuse other actions and sources.
	bad := row
	bad.Action = "none"
	if err := s.Write(ctx, []Row{bad}); err == nil {
		t.Error("action none accepted")
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %04o", filepath.Base(p), fi.Mode().Perm())
		}
	}
	fi, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %04o", fi.Mode().Perm())
	}

	// Reopening keeps the data and the schema.
	s.Close()
	s, err = OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.Rows(ctx); len(rows) != 1 {
		t.Errorf("reopened: %+v", rows)
	}
}

// TestStoreRefuses checks the store half of S5: a newer user_version is
// refused and left byte for byte untouched, and an unopenable path or a
// file that is not a database fails to open (design 0005 D2, D4).
func TestStoreRefuses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	newer := filepath.Join(dir, "newer.db")
	db, err := sql.Open("sqlite", newer)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE host_hour (x INTEGER)", "INSERT INTO host_hour VALUES (42)", "PRAGMA user_version = 2"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before, _ := os.ReadFile(newer)
	if _, err := OpenStore(ctx, newer); !errors.Is(err, ErrNewerSchema) {
		t.Errorf("newer schema: %v", err)
	}
	if after, _ := os.ReadFile(newer); !bytes.Equal(before, after) {
		t.Error("the newer database changed")
	}

	isDir := filepath.Join(dir, "dir.db")
	os.Mkdir(isDir, 0o700)
	if _, err := OpenStore(ctx, isDir); err == nil {
		t.Error("a directory opened as the database")
	}

	junk := filepath.Join(dir, "junk.db")
	os.WriteFile(junk, bytes.Repeat([]byte("not a database "), 100), 0o600)
	if _, err := OpenStore(ctx, junk); err == nil {
		t.Error("a non-database file opened")
	}

	foreign := filepath.Join(dir, "foreign.db")
	db, _ = sql.Open("sqlite", foreign)
	db.Exec("CREATE TABLE other (x INTEGER)")
	db.Close()
	if _, err := OpenStore(ctx, foreign); err == nil || !strings.Contains(err.Error(), "no schema version") {
		t.Errorf("a database with other tables: %v", err)
	}
}

// TestStatsRetentionAndFolding checks S4: rows older than the retention
// are deleted at start and every hour, in batches of 5,000 rows; hosts
// beyond max_hosts_per_hour and keys beyond 20,000 pending fold into *,
// counted by stats_folded_total (design 0005 D3, D4).
func TestStatsRetentionAndFolding(t *testing.T) {
	ctx := context.Background()
	t.Run("retention", func(t *testing.T) {
		now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
		clk := newClock(now)
		path := filepath.Join(t.TempDir(), "stats.db")
		s, err := OpenStore(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		hour := Bucket(now)
		day := int64(86400)
		// 12,000 rows two days and one hour old take three batches; the
		// rows of the oldest kept hour and later stay.
		var rows []Row
		for i := range 12000 {
			rows = append(rows, Row{Bucket: hour - 2*day - 3600, RowKey: blockKey(fmt.Sprintf("old%05d.example", i)), Counts: Counts{Connects: 1}})
		}
		for _, b := range []int64{hour - 2*day, hour - day, hour} {
			rows = append(rows, Row{Bucket: b, RowKey: blockKey("kept.example"), Counts: Counts{Connects: 1}})
		}
		if err := s.Write(ctx, rows); err != nil {
			t.Fatal(err)
		}
		s.Close()

		live := NewLive()
		st, err := Open(ctx, Options{Path: path, RetentionDays: 2, MaxHostsPerHour: 1000, Live: live, Clock: clk.Now, Interval: -1})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close(ctx)
		got, _ := st.Rows(ctx)
		if len(got) != 3 || got[0].Bucket != hour-2*day || got[2].Bucket != hour {
			t.Fatalf("after retention at start: %d rows, first %+v", len(got), got[0])
		}
		if v := pragma(t, st.store.db, "freelist_count"); v != "0" {
			t.Errorf("freelist_count %s after incremental_vacuum", v)
		}

		// After a reload shortens the retention to one day, the run a day
		// later keeps only the hour that was current at the start.
		st.SetLimits(1, 1000)
		clk.Set(now.Add(24 * time.Hour))
		if err := st.Prune(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ = st.Rows(ctx)
		if len(got) != 1 || got[0].Bucket != hour {
			t.Errorf("after the next run: %+v", got)
		}
	})

	t.Run("hourly", func(t *testing.T) {
		now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
		clk := newClock(now)
		path := filepath.Join(t.TempDir(), "stats.db")
		st, err := Open(ctx, Options{Path: path, RetentionDays: 1, MaxHostsPerHour: 1000, Clock: clk.Now, Interval: 5 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close(ctx)
		if err := st.store.Write(ctx, []Row{{Bucket: Bucket(now), RowKey: blockKey("a.example"), Counts: Counts{Connects: 1}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		if got, _ := st.Rows(ctx); len(got) != 1 {
			t.Fatalf("pruned before an hour passed: %+v", got)
		}
		clk.Set(now.Add(25 * time.Hour))
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, _ := st.Rows(ctx)
			if len(got) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the timer did not run retention: %+v", got)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	t.Run("max hosts per hour", func(t *testing.T) {
		now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
		clk := newClock(now)
		live := NewLive()
		r := NewRecorder(clk.Now, live, 3)
		for _, h := range []string{"a", "b", "c", "d", "e", "a"} {
			r.Complete(&flow{key: blockKey(h + ".example"), up: 1}, true, false)
		}
		// d and e fold; a, admitted earlier, keeps its row.
		got := map[string]int64{}
		for _, row := range r.Take() {
			got[row.Host] = row.Connects
			if row.Host == FoldedHost && (row.BytesUp != 2 || row.Action != "block" || row.Rule != "blocked") {
				t.Errorf("folded row %+v", row)
			}
		}
		want := map[string]int64{"a.example": 2, "b.example": 1, "c.example": 1, FoldedHost: 2}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("rows %v, want %v", got, want)
		}
		if n := live.Get(StatsFoldedTotal); n != 2 {
			t.Errorf("stats_folded_total %d, want 2", n)
		}
		// The next hour admits hosts again; a reload raises the cap.
		clk.Set(now.Add(time.Hour))
		r.SetMaxHosts(4)
		for _, h := range []string{"d", "e", "f", "g", "h"} {
			r.Complete(&flow{key: blockKey(h + ".example")}, true, false)
		}
		got = map[string]int64{}
		for _, row := range r.Take() {
			got[row.Host] = row.Connects
		}
		want = map[string]int64{"d.example": 1, "e.example": 1, "f.example": 1, "g.example": 1, FoldedHost: 1}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("next hour: rows %v, want %v", got, want)
		}
	})

	t.Run("pending keys", func(t *testing.T) {
		clk := newClock(time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC))
		live := NewLive()
		r := NewRecorder(clk.Now, live, 100000)
		for i := range MaxPending + 5 {
			r.Complete(&flow{key: blockKey(fmt.Sprintf("h%05d.example", i))}, true, false)
		}
		if n := r.Pending(); n != MaxPending+1 {
			t.Errorf("pending %d, want %d", n, MaxPending+1)
		}
		// An existing key still counts on its own row.
		r.Complete(&flow{key: blockKey("h00000.example")}, true, false)
		rows := r.Take()
		var folded, first int64
		for _, row := range rows {
			switch row.Host {
			case FoldedHost:
				folded = row.Connects
			case "h00000.example":
				first = row.Connects
			}
		}
		if folded != 5 || first != 2 || live.Get(StatsFoldedTotal) != 5 {
			t.Errorf("folded %d (want 5), first %d (want 2), stats_folded_total %d", folded, first, live.Get(StatsFoldedTotal))
		}

		// Rows merged back after a failed write count toward the cap.
		r.Restore(rows)
		r.Complete(&flow{key: blockKey("new.example")}, true, false)
		if n := live.Get(StatsFoldedTotal); n != 6 {
			t.Errorf("after restore: stats_folded_total %d, want 6", n)
		}

		// The folded rows reach the database.
		st, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "stats.db"), RetentionDays: 30,
			MaxHostsPerHour: 1000, Clock: clk.Now, Interval: -1})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close(context.Background())
		if err := st.store.Write(context.Background(), r.Take()); err != nil {
			t.Fatal(err)
		}
		var n int64
		st.store.db.QueryRow("SELECT connects FROM host_hour WHERE host = '*'").Scan(&n)
		if n != 6 {
			t.Errorf("database * row connects %d, want 6", n)
		}
	})
}

// TestStatsRowSize checks S6: 100,000 synthetic rows, each with an 8-byte
// interface name, take at most 160 bytes per row after a checkpoint, and
// reports the figure (design 0005 D3, assumption B3).
func TestStatsRowSize(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 100,000 rows")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stats.db")
	s, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const total = 100000
	start := time.Now()
	hour := Bucket(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC))
	var batch []Row
	var slowest time.Duration
	for i := range total {
		// 1,000 hosts an hour for 100 hours, as at the default cap.
		batch = append(batch, Row{
			Bucket: hour + int64(i/1000)*3600,
			RowKey: RowKey{Host: fmt.Sprintf("host%04d.cdn.example.com", i%1000), Action: "direct", Rule: "default",
				Source: "local", Interface: "eth-wifi"},
			Counts: Counts{Connects: int64(i % 7), HTTPRequests: int64(i % 3), BytesUp: int64(i * 37 % 100000),
				BytesDown: int64(i * 7919 % 10000000), FirstSeen: hour + 60, LastSeen: hour + 3000},
		})
		if len(batch) == MaxPending || i == total-1 {
			t0 := time.Now()
			if err := s.Write(ctx, batch); err != nil {
				t.Fatal(err)
			}
			slowest = max(slowest, time.Since(t0))
			batch = batch[:0]
		}
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	perRow := float64(fi.Size()) / total
	t.Logf("%d rows: %d bytes, %.1f bytes per row; writing took %v, the slowest %d-row flush %v",
		total, fi.Size(), perRow, time.Since(start).Round(time.Millisecond), MaxPending, slowest.Round(time.Millisecond))
	if perRow > 160 {
		t.Errorf("%.1f bytes per row, limit 160", perRow)
	}
}
