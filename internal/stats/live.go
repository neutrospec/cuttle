// Package stats holds cuttle's in-memory live counters (design 0002 D7). The
// hourly SQLite statistics arrive in M2.
package stats

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter names from design 0002 D7.
const (
	ConnectionsActive        = "connections_active"
	TunnelsActive            = "tunnels_active"
	HTTPRequestsActive       = "http_requests_active"
	ConnectionsTotal         = "connections_total"
	ConnectionsRejectedTotal = "connections_rejected_total"
	DecisionsTotal           = "decisions_total"
	OutcomesTotal            = "outcomes_total"
	BytesTotal               = "bytes_total"
	UpstreamOutcomesTotal    = "upstream_outcomes_total"
	// UpstreamState is a gauge per upstream and state: 1 for the current
	// state, 0 for the others (design 0003 D8).
	UpstreamState = "upstream_state"
)

// Live is a set of counters keyed by name and labels. Labels are bounded:
// hosts never appear in them.
type Live struct {
	mu sync.RWMutex
	m  map[string]*atomic.Int64
}

// NewLive returns an empty counter set.
func NewLive() *Live {
	return &Live{m: map[string]*atomic.Int64{}}
}

// Key builds a counter key such as `bytes_total{action="direct",direction="up"}`
// from a name and label pairs.
func Key(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i])
		b.WriteString(`="`)
		b.WriteString(labels[i+1])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// Counter returns the counter for name and label pairs, creating it.
func (l *Live) Counter(name string, labels ...string) *atomic.Int64 {
	k := Key(name, labels...)
	l.mu.RLock()
	c := l.m[k]
	l.mu.RUnlock()
	if c != nil {
		return c
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if c = l.m[k]; c == nil {
		c = new(atomic.Int64)
		l.m[k] = c
	}
	return c
}

// Add adds n to a counter.
func (l *Live) Add(n int64, name string, labels ...string) {
	l.Counter(name, labels...).Add(n)
}

// Set stores v in a counter, for gauges.
func (l *Live) Set(v int64, name string, labels ...string) {
	l.Counter(name, labels...).Store(v)
}

// Get returns a counter's value, 0 if it does not exist.
func (l *Live) Get(name string, labels ...string) int64 {
	k := Key(name, labels...)
	l.mu.RLock()
	defer l.mu.RUnlock()
	if c := l.m[k]; c != nil {
		return c.Load()
	}
	return 0
}

// Snapshot returns every counter by key.
func (l *Live) Snapshot() map[string]int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]int64, len(l.m))
	for k, c := range l.m {
		out[k] = c.Load()
	}
	return out
}

// Sum returns the total of every counter with this name, over all labels.
func (l *Live) Sum(name string) int64 {
	var n int64
	for k, v := range l.Snapshot() {
		if k == name || strings.HasPrefix(k, name+"{") {
			n += v
		}
	}
	return n
}

// Keys returns the sorted counter keys.
func (l *Live) Keys() []string {
	s := l.Snapshot()
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
