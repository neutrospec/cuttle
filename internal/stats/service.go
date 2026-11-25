package stats

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Write path timing (design 0005 D3, D4).
const (
	// FlushInterval is how often the flusher writes the pending rows.
	FlushInterval = 10 * time.Second
	// PruneInterval is how often retention runs after the one at start.
	PruneInterval = time.Hour
	// errorLogInterval bounds write error lines to one a minute.
	errorLogInterval = time.Minute
)

// Options configure Open.
type Options struct {
	Path            string
	RetentionDays   int
	MaxHostsPerHour int
	// Live receives stats_write_errors_total and stats_folded_total.
	Live   *Live
	Logger *slog.Logger
	// Clock is the time source for hours, retention, and the error log
	// limit; nil means time.Now. Tests inject a clock.
	Clock func() time.Time
	// Interval is the flush interval: 0 means FlushInterval, and a
	// negative value starts no timer, so a test flushes with Flush.
	Interval time.Duration
}

// Stats is the statistics service: the in-memory recorder, the SQLite
// store, and the flusher that writes the one to the other every
// FlushInterval and runs retention every hour (design 0005 D3, D4).
type Stats struct {
	rec       *Recorder
	store     *Store
	log       *slog.Logger
	live      *Live
	clock     func() time.Time
	retention atomic.Int64 // days

	// mu serializes flushes and retention on the single writer connection,
	// and guards the fields below.
	mu         sync.Mutex
	lastPrune  time.Time
	lastErrLog time.Time
	closed     bool

	// ctx ends an in-progress write of the timer's flush when Close starts.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// Open opens the database, runs retention once, which also shows that the
// database is writable, and starts the flusher. An error means cuttle runs
// without statistics (design 0005 D4).
func Open(ctx context.Context, opts Options) (*Stats, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Live == nil {
		opts.Live = NewLive()
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Interval == 0 {
		opts.Interval = FlushInterval
	}
	store, err := OpenStore(ctx, opts.Path)
	if err != nil {
		return nil, err
	}
	s := &Stats{
		store: store,
		log:   opts.Logger,
		live:  opts.Live,
		clock: opts.Clock,
		done:  make(chan struct{}),
	}
	s.retention.Store(int64(opts.RetentionDays))
	if err := s.Prune(ctx); err != nil {
		store.Close()
		return nil, err
	}
	s.rec = NewRecorder(opts.Clock, opts.Live, opts.MaxHostsPerHour)
	s.live.Add(0, StatsWriteErrorsTotal)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.loop(opts.Interval)
	return s, nil
}

// Recorder returns the in-memory aggregator the proxy records into.
func (s *Stats) Recorder() *Recorder { return s.rec }

// Path returns the database path.
func (s *Stats) Path() string { return s.store.Path() }

// Rows returns every written row in key order.
func (s *Stats) Rows(ctx context.Context) ([]Row, error) { return s.store.Rows(ctx) }

// SetLimits applies the [stats] settings of a reloaded configuration
// (design 0004 D6): the retention takes effect at the next hourly run.
func (s *Stats) SetLimits(retentionDays, maxHostsPerHour int) {
	s.retention.Store(int64(retentionDays))
	s.rec.SetMaxHosts(maxHostsPerHour)
}

func (s *Stats) loop(interval time.Duration) {
	defer close(s.done)
	if interval < 0 {
		<-s.ctx.Done()
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.Flush(s.ctx)
		s.mu.Lock()
		due := s.clock().Sub(s.lastPrune) >= PruneInterval
		s.mu.Unlock()
		if due {
			s.Prune(s.ctx)
		}
	}
}

// Flush writes the pending rows, with the bytes that open flows moved since
// the last flush, in one transaction. If the write fails, the rows go back
// to the recorder for the next flush, stats_write_errors_total increases,
// and an error is logged at most once a minute (design 0005 D4).
func (s *Stats) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("statistics closed")
	}
	return s.flushLocked(ctx)
}

func (s *Stats) flushLocked(ctx context.Context) error {
	rows := s.rec.Take()
	if len(rows) == 0 {
		return nil
	}
	err := s.store.Write(ctx, rows)
	if err != nil {
		s.rec.Restore(rows)
		s.live.Add(1, StatsWriteErrorsTotal)
		if now := s.clock(); s.lastErrLog.IsZero() || now.Sub(s.lastErrLog) >= errorLogInterval {
			s.lastErrLog = now
			s.log.Warn("statistics write failed; retrying at the next flush", "rows", len(rows), "error", err.Error())
		}
	}
	return err
}

// Prune deletes the rows older than stats.retention_days (design 0005 D3).
func (s *Stats) Prune(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.lastPrune = now
	cutoff := Bucket(now) - s.retention.Load()*86400
	n, err := s.store.Prune(ctx, cutoff)
	if err != nil {
		s.log.Warn("statistics retention failed", "error", err.Error())
		return err
	}
	if n > 0 {
		s.log.Info("statistics retention", "deleted_rows", n, "before", time.Unix(cutoff, 0).UTC().Format(time.RFC3339))
	}
	return nil
}

// Close stops the flusher, writes everything pending, the final bytes of
// flows closed by shutdown included, and closes the database. ctx bounds
// the whole step: a write still waiting for a lock gives up when ctx ends,
// and its rows are lost (design 0006 D6 step 5).
func (s *Stats) Close(ctx context.Context) error {
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		s.log.Error("statistics flush at shutdown timed out; pending rows lost", "error", ctx.Err().Error())
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.flushLocked(ctx)
	if err != nil {
		s.log.Error("statistics flush at shutdown failed; pending rows lost", "rows", s.rec.Pending(), "error", err.Error())
	}
	if cerr := s.store.Close(); err == nil {
		err = cerr
	}
	return err
}
