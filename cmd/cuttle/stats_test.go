package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/stats"
	"github.com/neutrospec/cuttle/internal/testenv"
)

// statsPath is the sandbox's statistics database (design 0006 D1).
func statsPath(sb *testenv.Sandbox) string {
	return filepath.Join(sb.StateHome, "cuttle", "stats.db")
}

// openTunnel sends CONNECT target through the proxy at addr, then one
// request for n bytes through the tunnel, and returns the open tunnel.
func openTunnel(t *testing.T, addr, target string, n int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(20 * time.Second))
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	if res, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}); err != nil || res.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", res, err)
	}
	io.WriteString(c, "GET /bytes?n="+strconv.Itoa(n)+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("request through the tunnel: %v %v", res, err)
	}
	if got, _ := io.Copy(io.Discard, res.Body); got != int64(n) {
		t.Fatalf("read %d bytes, want %d", got, n)
	}
	return c
}

// TestStatsShutdownFlush checks S7: the bytes of tunnels that shutdown
// closes are in the database after the process exits, together with their
// decisions (design 0005 D4, 0006 D6 step 5).
func TestStatsShutdownFlush(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	addr := testenv.Port(sb.FreePort())
	writeConfig(t, sb, serveConfig([]string{addr}, ""))
	proc := sb.Start(addr, "serve")
	if st := statusJSON(t, sb); st.Stats != statsEnabled || st.StatsErr != "" || st.Counters[stats.StatsWriteErrorsTotal] != 0 {
		t.Errorf("status stats %q %q, counters %v", st.Stats, st.StatsErr, st.Counters)
	}
	target := origin.Addr.String()
	openTunnel(t, addr, target, 70000)
	openTunnel(t, addr, target, 30000)

	start := time.Now()
	proc.Signal(syscall.SIGTERM)
	code, exited := proc.Wait(8 * time.Second)
	if !exited || code != 0 {
		t.Fatalf("exit %d after %v (exited %v)", code, time.Since(start), exited)
	}

	// The decision lines give each tunnel's bytes.
	log, _ := os.ReadFile(sb.LogPath())
	var up, down, tunnels int64
	for _, line := range strings.Split(string(log), "\n") {
		if !strings.Contains(line, "msg=decision") || !strings.Contains(line, "kind=connect target="+target) {
			continue
		}
		if !strings.Contains(line, "outcome=shutdown") {
			t.Errorf("decision line %q", line)
		}
		tunnels++
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "up="); ok {
				n, _ := strconv.ParseInt(v, 10, 64)
				up += n
			}
			if v, ok := strings.CutPrefix(f, "down="); ok {
				n, _ := strconv.ParseInt(v, 10, 64)
				down += n
			}
		}
	}
	if tunnels != 2 || down < 100000 || up == 0 {
		t.Fatalf("%d tunnel decisions, up %d down %d:\n%s", tunnels, up, down, log)
	}
	if i, j := strings.Index(string(log), "kind=connect target="+target), strings.Index(string(log), "msg=stopped"); i < 0 || j < i {
		t.Errorf("stopped before the decisions:\n%s", log)
	}

	s, err := stats.OpenStore(context.Background(), statsPath(sb))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Rows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got stats.Counts
	for _, r := range rows {
		if r.RowKey != (stats.RowKey{Host: "127.0.0.1", Action: "direct", Rule: "loopback", Source: "local"}) {
			t.Errorf("unexpected row %+v", r)
			continue
		}
		got.Connects += r.Connects
		got.BytesUp += r.BytesUp
		got.BytesDown += r.BytesDown
	}
	if got.Connects != 2 || got.BytesUp != up || got.BytesDown != down {
		t.Errorf("database: connects %d up %d down %d, want 2, %d, %d", got.Connects, got.BytesUp, got.BytesDown, up, down)
	}
}

// TestStatsUnavailable checks S5: with an unopenable database or one with
// a newer user_version, the proxy serves and status reports stats
// unavailable with the reason; the newer database is left untouched
// (design 0005 D2, D4).
func TestStatsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	origin := testenv.NewOrigin(t, testenv.OriginOptions{})
	cases := []struct {
		name    string
		prepare func(t *testing.T, path string)
		reason  string
	}{
		{"directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "stats.db"},
		{"newer schema", func(t *testing.T, path string) {
			os.MkdirAll(filepath.Dir(path), 0o700)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, q := range []string{"PRAGMA journal_mode = WAL", "CREATE TABLE host_hour (x INTEGER)", "PRAGMA user_version = 2"} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
		}, "newer schema: version 2, this binary knows 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
			path := statsPath(sb)
			tc.prepare(t, path)
			before, _ := os.ReadFile(path)
			addr := testenv.Port(sb.FreePort())
			writeConfig(t, sb, serveConfig([]string{addr}, ""))
			proc := sb.Start(addr, "serve")

			c := proxyClient(t, addr)
			if code := getStatus(t, c, "http://"+origin.Addr.String()+"/bytes?n=10"); code != 200 {
				t.Errorf("direct request: %d", code)
			}
			if code := getStatus(t, c, "http://blocked.example/"); code != 403 {
				t.Errorf("blocked request: %d", code)
			}
			st := statusJSON(t, sb)
			if st.Stats != statsUnavailable || !strings.Contains(st.StatsErr, tc.reason) {
				t.Errorf("status stats %q, error %q, want unavailable with %q", st.Stats, st.StatsErr, tc.reason)
			}
			if _, ok := st.Counters[stats.StatsWriteErrorsTotal]; ok {
				t.Errorf("counters without statistics: %v", st.Counters)
			}
			r := sb.Run(10*time.Second, "status")
			if !strings.Contains(r.Stdout, "unavailable: ") || !strings.Contains(r.Stdout, tc.reason) {
				t.Errorf("status text:\n%s", r.Stdout)
			}
			waitLog(t, sb, `msg="statistics unavailable; running without them"`)
			if code := proc.Stop(); code != 0 {
				t.Errorf("exit %d", code)
			}
			if tc.name == "newer schema" {
				if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
					t.Error("the newer database changed")
				}
			}
		})
	}
}
