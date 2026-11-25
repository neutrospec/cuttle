package main

import (
	"context"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/stats"
)

const (
	// statsOpenTimeout bounds opening, migrating, and the first retention
	// run of the statistics database at start (design 0006 D5 step 5).
	statsOpenTimeout = 10 * time.Second
	// statsFlushTimeout bounds the statistics flush at shutdown, so the
	// drain, the handler wait, and the flush end within 7 s (design 0006 D6
	// step 5).
	statsFlushTimeout = time.Second
)

// Statistics states in cuttle status (design 0005 D4, 0006 D4).
const (
	statsEnabled     = "enabled"
	statsUnavailable = "unavailable"
)

// openStats opens the statistics database. A failure is not fatal: cuttle
// logs the reason and runs without statistics, and status reports
// unavailable with the reason (design 0005 D4).
func (s *serveState) openStats(cfg *config.Config, live *stats.Live) {
	ctx, cancel := context.WithTimeout(context.Background(), statsOpenTimeout)
	defer cancel()
	st, err := stats.Open(ctx, stats.Options{
		Path:            s.paths.Stats.Path,
		RetentionDays:   cfg.Stats.RetentionDays,
		MaxHostsPerHour: cfg.Stats.MaxHostsPerHour,
		Live:            live,
		Logger:          s.log,
		Interval:        s.statsInterval,
	})
	if err != nil {
		s.statsErr = err.Error()
		s.log.Error("statistics unavailable; running without them", "path", s.paths.Stats.Path, "error", err.Error())
		return
	}
	s.stats = st
}

// statsRecorder is the recorder the proxy records into, nil without
// statistics.
func (s *serveState) statsRecorder() *stats.Recorder {
	if s.stats == nil {
		return nil
	}
	return s.stats.Recorder()
}

// statsState returns the statistics state for status and, when
// unavailable, the reason.
func (s *serveState) statsState() (string, string) {
	if s.stats == nil {
		return statsUnavailable, s.statsErr
	}
	return statsEnabled, ""
}

// closeStats writes the pending statistics, the bytes of tunnels closed by
// shutdown included, within statsFlushTimeout and closes the database
// (design 0006 D6 step 5). It runs once.
func (s *serveState) closeStats() {
	s.statsOnce.Do(func() {
		if s.stats == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), statsFlushTimeout)
		defer cancel()
		s.stats.Close(ctx)
	})
}
