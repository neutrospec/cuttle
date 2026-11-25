package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

// FoldedHost is the host of a row that holds folded hosts (design 0005 D4).
const FoldedHost = "*"

// MaxPending bounds the keys pending between flushes: a new key beyond it
// folds into FoldedHost (design 0005 D4).
const MaxPending = 20000

// RowKey identifies a statistics row within its hour (design 0005 D1, D2).
type RowKey struct {
	Host      string // normalized name or IP literal, IPv6 without brackets
	Action    string // direct, upstream, or block
	Upstream  string // set when Action is upstream
	Rule      string
	Source    string // local or remote
	Interface string // the decision-log interface value of design 0008 D15
}

// Counts are a row's values.
type Counts struct {
	Connects     int64
	HTTPRequests int64
	Failures     int64
	BytesUp      int64
	BytesDown    int64
	FirstSeen    int64 // Unix seconds
	LastSeen     int64 // Unix seconds
}

// add merges c into a.
func (a *Counts) add(c Counts) {
	a.Connects += c.Connects
	a.HTTPRequests += c.HTTPRequests
	a.Failures += c.Failures
	a.BytesUp += c.BytesUp
	a.BytesDown += c.BytesDown
	a.FirstSeen = min(a.FirstSeen, c.FirstSeen)
	a.LastSeen = max(a.LastSeen, c.LastSeen)
}

// Row is one row of host_hour.
type Row struct {
	Bucket int64 // start of the UTC hour, Unix seconds
	RowKey
	Counts
}

// Bucket returns the start of t's UTC hour in Unix seconds.
func Bucket(t time.Time) int64 {
	s := t.Unix()
	return s - s%3600
}

// Flow is an exchange whose bytes the recorder samples while it is open: a
// tunnel, a connection upgraded after a 101, or a plain-HTTP exchange.
type Flow interface {
	// StatsKey returns the flow's key. The recorder calls it from Open on,
	// so the fields it reads must be set before Open and the interface
	// before the first byte is counted.
	StatsKey() RowKey
	// StatsBytes returns the bytes counted so far, up and down. Each value
	// only grows.
	StatsBytes() (up, down int64)
}

// sampled is the part of an open flow's bytes already added to pending.
type sampled struct{ up, down int64 }

type pendingKey struct {
	bucket int64
	RowKey
}

// Recorder is the in-memory aggregator of the statistics write path (design
// 0005 D4). The forwarding path calls Open and Complete, which update a map
// under a short mutex and never touch SQLite; the flusher takes the pending
// rows with Take. A nil *Recorder records nothing, so the proxy runs the
// same without statistics.
type Recorder struct {
	clock    func() time.Time
	live     *Live
	maxHosts atomic.Int64

	mu      sync.Mutex
	pending map[pendingKey]*Counts
	// hosts holds the hosts admitted in each hour since start, for the
	// max_hosts_per_hour cap.
	hosts      map[int64]map[string]struct{}
	open       map[Flow]*sampled
	maxPending int
}

// NewRecorder returns an empty recorder that admits maxHosts distinct
// hosts per hour. clock nil means time.Now. live, when not nil, receives
// stats_folded_total.
func NewRecorder(clock func() time.Time, live *Live, maxHosts int) *Recorder {
	if clock == nil {
		clock = time.Now
	}
	r := &Recorder{
		clock:      clock,
		live:       live,
		pending:    map[pendingKey]*Counts{},
		hosts:      map[int64]map[string]struct{}{},
		open:       map[Flow]*sampled{},
		maxPending: MaxPending,
	}
	r.SetMaxHosts(maxHosts)
	if live != nil {
		live.Add(0, StatsFoldedTotal)
	}
	return r
}

// SetMaxHosts changes max_hosts_per_hour, as a reload does (design 0004
// D6). Hosts already admitted in the current hour stay admitted.
func (r *Recorder) SetMaxHosts(n int) {
	if r != nil {
		r.maxHosts.Store(int64(n))
	}
}

// Open starts sampling a flow's bytes at each Take, so a long tunnel adds
// its bytes to the hours in which they moved. The caller sets the flow's
// key fields before Open and calls Complete when the flow ends.
func (r *Recorder) Open(f Flow) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open[f] == nil {
		r.open[f] = &sampled{}
	}
}

// Complete records a completed decision: one CONNECT or plain-HTTP
// decision in the current hour, a failure when failed is set, and the
// flow's bytes not yet sampled. A flow that was never opened adds all its
// bytes.
func (r *Recorder) Complete(f Flow, connect, failed bool) {
	if r == nil {
		return
	}
	now := r.clock()
	t := now.Unix()
	c := Counts{FirstSeen: t, LastSeen: t}
	if connect {
		c.Connects = 1
	} else {
		c.HTTPRequests = 1
	}
	if failed {
		c.Failures = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.open[f]
	delete(r.open, f)
	if s == nil {
		s = &sampled{}
	}
	up, down := f.StatsBytes()
	c.BytesUp, c.BytesDown = up-s.up, down-s.down
	r.addLocked(Bucket(now), f.StatsKey(), c)
}

// addLocked adds c to the pending row of key k in bucket, folding the host
// into FoldedHost when the hour already admitted max_hosts_per_hour other
// hosts or when MaxPending keys are pending (design 0005 D4). The caller
// holds mu.
func (r *Recorder) addLocked(bucket int64, k RowKey, c Counts) {
	folded := false
	if k.Host != FoldedHost {
		hs := r.hosts[bucket]
		if hs == nil {
			hs = map[string]struct{}{}
			r.hosts[bucket] = hs
		}
		if _, ok := hs[k.Host]; !ok {
			if int64(len(hs)) >= r.maxHosts.Load() {
				k.Host, folded = FoldedHost, true
			} else {
				hs[k.Host] = struct{}{}
			}
		}
	}
	rk := pendingKey{bucket, k}
	row := r.pending[rk]
	if row == nil && k.Host != FoldedHost && len(r.pending) >= r.maxPending {
		rk.Host, folded = FoldedHost, true
		row = r.pending[rk]
	}
	if folded && r.live != nil {
		r.live.Add(1, StatsFoldedTotal)
	}
	if row == nil {
		row = &Counts{FirstSeen: c.FirstSeen, LastSeen: c.LastSeen}
		r.pending[rk] = row
	}
	row.add(c)
}

// Take samples the bytes of every open flow into the current hour, then
// returns the pending rows and starts an empty set. It also forgets the
// admitted hosts of earlier hours, since every record goes to the current
// hour.
func (r *Recorder) Take() []Row {
	if r == nil {
		return nil
	}
	now := r.clock()
	bucket, t := Bucket(now), now.Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for f, s := range r.open {
		up, down := f.StatsBytes()
		if up == s.up && down == s.down {
			continue
		}
		r.addLocked(bucket, f.StatsKey(), Counts{BytesUp: up - s.up, BytesDown: down - s.down, FirstSeen: t, LastSeen: t})
		s.up, s.down = up, down
	}
	rows := make([]Row, 0, len(r.pending))
	for k, c := range r.pending {
		rows = append(rows, Row{Bucket: k.bucket, RowKey: k.RowKey, Counts: *c})
	}
	r.pending = make(map[pendingKey]*Counts, len(rows))
	for b := range r.hosts {
		if b < bucket {
			delete(r.hosts, b)
		}
	}
	return rows
}

// Restore merges rows that could not be written back into the pending set,
// so the next flush retries them (design 0005 D4). Their keys were already
// admitted, so nothing folds here; new keys still fold once the pending
// set, restored keys included, holds MaxPending keys.
func (r *Recorder) Restore(rows []Row) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		rk := pendingKey{row.Bucket, row.RowKey}
		if c := r.pending[rk]; c != nil {
			c.add(row.Counts)
		} else {
			c := row.Counts
			r.pending[rk] = &c
		}
	}
}

// Pending returns the number of pending keys.
func (r *Recorder) Pending() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
