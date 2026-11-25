package proxy

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
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

var summaryRe = regexp.MustCompile(`msg="warnings suppressed" count=(\d+) budget=60 interval=1m0s kinds="rejected source,connection limit"`)

// summaries returns the count of every summary line, in order.
func summaries(logs *logBuffer) []int {
	var out []int
	for _, m := range summaryRe.FindAllStringSubmatch(logs.String(), -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

func sum(list []int) int {
	n := 0
	for _, v := range list {
		n += v
	}
	return n
}

// pauseTimer models a summary timer whose callback runs late, as it can
// under load with real time: it stops the pending timer but leaves it set,
// so warn arms no other, and returns the callback for the test to run when
// it chooses.
func pauseTimer(t *testing.T, w *warnLimiter) func() {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer == nil {
		t.Fatal("no summary timer pending")
	}
	w.timer.Stop()
	return w.onTimer
}

// TestWarningBudget checks the limits of the rejected-source and
// connection-limit warnings (design 0003 D10, D12) with the production
// values, one minute and 60 lines, in synctest bubbles, whose fake clock
// makes the minutes pass at once: one warning per address and reason per
// minute, at most 60 lines per minute for both kinds together, one summary
// line per minute whose count holds every warning left out, written at the
// next warning or by the timer, whichever comes first, and a full map that
// keeps suppressing instead of starting over.
func TestWarningBudget(t *testing.T) {
	t.Run("per address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
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
			// A minute later the address is warned about again.
			time.Sleep(time.Minute)
			p.warnSource(a)
			if countLines(logs, "rejected source") != 2 {
				t.Errorf("no warning after a minute:\n%s", logs.String())
			}
			w.stop()
			if countLines(logs, "warnings suppressed") != 0 {
				t.Errorf("repeats within the minute counted as suppressed:\n%s", logs.String())
			}
		})
	})

	t.Run("shared budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := &logBuffer{}
			w := newWarnLimiter(newTestLogger(logs))
			p := &Proxy{warn: w}
			for i := 1; i <= 200; i++ {
				p.warnSource(rejectedPeer(i))
			}
			for i := 1; i <= 50; i++ {
				p.warnLimit(netip.AddrFrom4([4]byte{10, 8, 0, byte(i)}), "proxy.remote.max_connections_per_source", 2)
			}
			p.warnSource(rejectedPeer(1)) // covered by its line: neither written nor counted
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
			if got := summaries(logs); len(got) != 1 || got[0] != 190 {
				t.Errorf("summaries %v, want [190]:\n%s", got, logs.String())
			}
		})
	})

	t.Run("summary by the timer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := &logBuffer{}
			w := newWarnLimiter(newTestLogger(logs))
			defer w.stop()
			p := &Proxy{warn: w}
			for i := 1; i <= 100; i++ {
				p.warnSource(rejectedPeer(i))
			}
			if got := countLines(logs, "rejected source"); got != 60 {
				t.Fatalf("%d warning lines, want 60", got)
			}
			time.Sleep(time.Minute - time.Millisecond)
			if got := summaries(logs); len(got) != 0 {
				t.Fatalf("summary %v before the minute ended", got)
			}
			// With no further warning, the timer writes the summary when the
			// minute ends.
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if got := summaries(logs); len(got) != 1 || got[0] != 40 {
				t.Fatalf("summaries %v, want [40]", got)
			}
			// The budget is fresh in the next minute, and a second flood gets
			// its own summary.
			time.Sleep(10 * time.Second)
			for i := 101; i <= 175; i++ {
				p.warnSource(rejectedPeer(i))
			}
			if got := countLines(logs, "rejected source"); got != 120 {
				t.Errorf("%d warning lines after the second flood, want 120", got)
			}
			time.Sleep(time.Minute)
			synctest.Wait()
			time.Sleep(5 * time.Minute)
			if got := summaries(logs); len(got) != 2 || got[1] != 15 {
				t.Errorf("summaries %v, want [40 15]", got)
			}
		})
	})

	t.Run("rollover before the timer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := &logBuffer{}
			w := newWarnLimiter(newTestLogger(logs))
			defer w.stop()
			p := &Proxy{warn: w}
			for i := 1; i <= 100; i++ {
				p.warnSource(rejectedPeer(i))
			}
			late := pauseTimer(t, w)
			time.Sleep(time.Minute)
			// The timer has not run, so the first warning of the next minute
			// writes the summary of the last one.
			p.warnSource(rejectedPeer(101))
			if got := summaries(logs); len(got) != 1 || got[0] != 40 {
				t.Fatalf("summaries %v after the rollover, want [40]", got)
			}
			for i := 102; i <= 200; i++ {
				p.warnSource(rejectedPeer(i))
			}
			// The late callback finds warnings left out in the new minute,
			// writes nothing early, and waits for that minute's end.
			time.Sleep(10 * time.Second)
			late()
			if got := summaries(logs); len(got) != 1 {
				t.Fatalf("summaries %v after the late timer, want one", got)
			}
			time.Sleep(50*time.Second - time.Millisecond)
			if got := summaries(logs); len(got) != 1 {
				t.Fatalf("summaries %v before the minute ended", got)
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if got := summaries(logs); len(got) != 2 || got[1] != 40 {
				t.Errorf("summaries %v, want [40 40]", got)
			}
			if written := countLines(logs, "rejected source"); written+sum(summaries(logs)) != 200 {
				t.Errorf("%d lines and %v left out for 200 warnings", written, summaries(logs))
			}
		})
	})

	t.Run("sustained flood", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := &logBuffer{}
			w := newWarnLimiter(newTestLogger(logs))
			defer w.stop()
			p := &Proxy{warn: w}
			// Three rejected connections a second for five minutes, from 180
			// addresses that recur once a minute. In every minute but the
			// last, the summary timer runs late, so the first warning of the
			// next minute writes the summary and the late callback, run half
			// a minute later, reschedules itself to the end of that minute.
			const minutes, perSecond = 5, 3
			calls := 0
			var late func()
			for m := range minutes {
				for s := range 60 {
					for c := range perSecond {
						p.warnSource(rejectedPeer(s*perSecond + c + 1))
						calls++
					}
					if s == 0 {
						if got := len(summaries(logs)); got != m {
							t.Fatalf("minute %d: %d summaries, want %d", m, got, m)
						}
					}
					if s == 30 && late != nil {
						late()
						late = nil
						if got := len(summaries(logs)); got != m {
							t.Fatalf("minute %d: late timer wrote a summary early", m)
						}
					}
					if s == 59 && m < minutes-1 {
						late = pauseTimer(t, w)
					}
					time.Sleep(time.Second)
				}
			}
			synctest.Wait()
			got := summaries(logs)
			written := countLines(logs, "rejected source")
			if len(got) != minutes {
				t.Errorf("%d summaries for %d minutes: %v", len(got), minutes, got)
			}
			for i, n := range got {
				if n != 60*perSecond-warnBudget {
					t.Errorf("minute %d: summary count %d, want %d", i, n, 60*perSecond-warnBudget)
				}
			}
			if written != minutes*warnBudget || written+sum(got) != calls {
				t.Errorf("%d lines and %d left out for %d warnings", written, sum(got), calls)
			}
		})
	})

	t.Run("full map keeps suppressing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
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
			// A minute later the old keys are pruned for new ones.
			time.Sleep(time.Minute)
			p.warnSource(rejectedPeer(21))
			if got := countLines(logs, "rejected source"); got != 9 || len(w.seen) != 1 {
				t.Errorf("%d warning lines and %d keys after a minute, want 9 and 1", got, len(w.seen))
			}
			w.stop()
			if !strings.Contains(logs.String(), `msg="warnings suppressed" count=12 `) {
				t.Errorf("summary:\n%s", logs.String())
			}
		})
	})
}
