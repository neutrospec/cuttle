package proxy

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

// rejectedPeer returns the i-th address of 198.51.100.0/24, the rejected
// sources of the test address plan (design 0007 D3), for i in 1-254.
func rejectedPeer(i int) netip.Addr {
	return netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
}

func countLines(logs *logBuffer, msg string) int {
	return strings.Count(logs.String(), `msg="`+msg+`"`)
}

// TestWarningBudget checks the limits of the rejected-source and
// connection-limit warnings (design 0003 D10, D12): one per address and
// reason per interval, at most warnBudget lines per interval for both kinds
// together, one summary line per interval with the number suppressed, and
// a full map that keeps suppressing instead of starting over.
func TestWarningBudget(t *testing.T) {
	t.Run("per address", func(t *testing.T) {
		logs := &logBuffer{}
		w := newWarnLimiter(newTestLogger(logs))
		p := &Proxy{warn: w}
		a := rejectedPeer(7)
		p.warnSource(a)
		p.warnSource(a)
		p.warnLimit(a, "proxy.max_connections", 16) // another reason
		p.warnLimit(a, "proxy.max_connections", 16)
		if countLines(logs, "rejected source") != 1 || countLines(logs, "connection limit reached") != 1 {
			t.Errorf("log:\n%s", logs.String())
		}
		w.stop()
		if countLines(logs, "warnings suppressed") != 0 {
			t.Errorf("repeats within the interval counted as suppressed:\n%s", logs.String())
		}
	})

	t.Run("shared budget", func(t *testing.T) {
		logs := &logBuffer{}
		w := newWarnLimiter(newTestLogger(logs))
		p := &Proxy{warn: w}
		for i := 1; i <= 200; i++ {
			p.warnSource(rejectedPeer(i))
		}
		for i := 1; i <= 50; i++ {
			p.warnLimit(netip.AddrFrom4([4]byte{10, 8, 0, byte(i)}), "proxy.remote.max_connections_per_source", 2)
		}
		p.warnSource(rejectedPeer(1)) // written earlier: per-address rule, not counted
		if got := countLines(logs, "rejected source") + countLines(logs, "connection limit reached"); got != warnBudget {
			t.Errorf("%d warning lines, want %d", got, warnBudget)
		}
		if countLines(logs, "connection limit reached") != 0 {
			t.Errorf("limit warnings written past the spent budget")
		}
		if len(w.seen) > warnBudget {
			t.Errorf("%d keys remembered", len(w.seen))
		}
		// Shutdown writes the pending summary at once.
		w.stop()
		if !strings.Contains(logs.String(), `msg="warnings suppressed" count=190 budget=60 interval=1m0s kinds="rejected source,connection limit"`) {
			t.Errorf("summary:\n%s", logs.String())
		}
	})

	t.Run("summary per interval", func(t *testing.T) {
		logs := &logBuffer{}
		w := newWarnLimiter(newTestLogger(logs))
		w.interval, w.budget = time.Second, 5
		defer w.stop()
		p := &Proxy{warn: w}
		for i := 1; i <= 25; i++ {
			p.warnSource(rejectedPeer(i))
		}
		if got := countLines(logs, "rejected source"); got != 5 {
			t.Fatalf("%d warning lines, want 5", got)
		}
		// The timer writes the summary when the interval ends, without a
		// further warning.
		waitForLog(t, logs, `msg="warnings suppressed" count=20 budget=5 interval=1s`)
		// The budget is fresh in the next interval, and a second flood gets
		// its own summary.
		for i := 26; i <= 40; i++ {
			p.warnSource(rejectedPeer(i))
		}
		if got := countLines(logs, "rejected source"); got != 10 {
			t.Errorf("%d warning lines after the second flood, want 10", got)
		}
		waitForLog(t, logs, `msg="warnings suppressed" count=10 budget=5 interval=1s`)
		time.Sleep(1500 * time.Millisecond)
		if got := countLines(logs, "warnings suppressed"); got != 2 {
			t.Errorf("%d summary lines, want 2:\n%s", got, logs.String())
		}
	})

	t.Run("full map keeps suppressing", func(t *testing.T) {
		logs := &logBuffer{}
		w := newWarnLimiter(newTestLogger(logs))
		w.budget, w.maxKeys = 1000, 8
		p := &Proxy{warn: w}
		for i := 1; i <= 20; i++ {
			p.warnSource(rejectedPeer(i))
		}
		// Every address seen before the map filled stays suppressed.
		for i := 1; i <= 8; i++ {
			p.warnSource(rejectedPeer(i))
		}
		if got := countLines(logs, "rejected source"); got != 8 {
			t.Errorf("%d warning lines, want 8:\n%s", got, logs.String())
		}
		if len(w.seen) != 8 {
			t.Errorf("%d keys remembered, want 8", len(w.seen))
		}
		w.stop()
		if !strings.Contains(logs.String(), `msg="warnings suppressed" count=12 `) {
			t.Errorf("summary:\n%s", logs.String())
		}
	})
}
