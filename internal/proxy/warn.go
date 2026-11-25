package proxy

import (
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// Limits of the rejected-source and connection-limit warnings (design 0003
// D10 and D12).
const (
	// warnInterval is the period of the per-address rule and of the budget.
	warnInterval = time.Minute
	// warnBudget is the most such warnings written per interval in total.
	warnBudget = 60
	// maxWarnKeys bounds the memory of the per-address rule. The budget
	// keeps it far below this; a full map suppresses, never resets.
	maxWarnKeys = 1024
)

// warnKey identifies a rate-limited warning: one per address and reason.
type warnKey struct {
	addr   netip.Addr
	reason string
}

// warnLimiter writes the rejected-source and connection-limit warnings: at
// most one per address and reason per interval, and at most budget lines
// per interval in total. A warning over the budget, or one whose key finds
// the map full, is counted as suppressed, and one summary line per
// interval reports the count. The summary is written when the interval
// ends, or at Shutdown if that comes first.
type warnLimiter struct {
	log      *slog.Logger
	interval time.Duration
	budget   int
	maxKeys  int

	mu         sync.Mutex
	seen       map[warnKey]time.Time // keys written within the last interval
	start      time.Time             // start of the budget interval; zero when none is open
	written    int
	suppressed int
	timer      *time.Timer
	stopped    bool
}

func newWarnLimiter(log *slog.Logger) *warnLimiter {
	return &warnLimiter{
		log:      log,
		interval: warnInterval,
		budget:   warnBudget,
		maxKeys:  maxWarnKeys,
		seen:     map[warnKey]time.Time{},
	}
}

// warn writes msg with args unless key was written within the interval,
// or counts it as suppressed when the budget is spent or the map is full.
func (w *warnLimiter) warn(key warnKey, msg string, args ...any) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.seen[key]; ok && now.Sub(last) < w.interval {
		return
	}
	if w.start.IsZero() || now.Sub(w.start) >= w.interval {
		w.summaryLocked()
		w.start, w.written, w.suppressed = now, 0, 0
	}
	if w.written >= w.budget {
		w.suppressLocked(now)
		return
	}
	if len(w.seen) >= w.maxKeys {
		for k, t := range w.seen {
			if now.Sub(t) >= w.interval {
				delete(w.seen, k)
			}
		}
		if len(w.seen) >= w.maxKeys {
			w.suppressLocked(now)
			return
		}
	}
	w.seen[key] = now
	w.written++
	w.log.Warn(msg, args...)
}

// suppressLocked counts a suppressed warning and makes sure a summary is
// due at the end of the interval.
func (w *warnLimiter) suppressLocked(now time.Time) {
	w.suppressed++
	if w.timer == nil && !w.stopped {
		w.timer = time.AfterFunc(w.start.Add(w.interval).Sub(now), w.onTimer)
	}
}

// onTimer writes the summary of an interval that has ended. When warn has
// already started a new interval, it waits for that one's end instead.
func (w *warnLimiter) onTimer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer = nil
	if w.suppressed == 0 || w.stopped {
		return
	}
	now := time.Now()
	if end := w.start.Add(w.interval); now.Before(end) {
		w.timer = time.AfterFunc(end.Sub(now), w.onTimer)
		return
	}
	w.summaryLocked()
	w.start, w.written, w.suppressed = time.Time{}, 0, 0
}

// summaryLocked writes the summary line when warnings were suppressed in
// the current interval.
func (w *warnLimiter) summaryLocked() {
	if w.suppressed > 0 {
		w.log.Warn("warnings suppressed", "count", w.suppressed, "budget", w.budget, "interval", w.interval.String(),
			"kinds", "rejected source,connection limit")
	}
}

// stop writes any pending summary and cancels the timer.
func (w *warnLimiter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.summaryLocked()
	w.suppressed = 0
}
