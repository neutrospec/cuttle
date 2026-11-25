package admin

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/stats"
)

// fakeConfig is a valid configuration for the fake backend.
const fakeConfig = `name = "cuttle-test"

[proxy]
listen = ["127.0.0.1:41728"]
default = "direct"

[admin]
listen = "127.0.0.1:41729"

[[upstream]]
name = "gateway"
address = "192.0.2.6:3128"
disclosure = "via"

[[rule]]
name = "restricted"
domains = ["internal.example"]
action = "upstream:gateway"
`

// fakeBackend is a running instance for the admin tests: a configuration
// file that Reload loads as cuttle serve does, a status, and optional
// statistics.
type fakeBackend struct {
	t    *testing.T
	path string

	mu       sync.Mutex
	cfg      *config.Config
	st       *stats.Stats
	status   control.Status
	reloads  int
	loadedAt time.Time
	lastErr  string
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config", "cuttle")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{t: t, path: filepath.Join(dir, "config.toml")}
	if err := os.WriteFile(b.path, []byte(fakeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(b.path)
	if err != nil {
		t.Fatal(err)
	}
	b.cfg = cfg
	b.loadedAt = time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	b.status = control.Status{Name: "cuttle-test", Version: "test", Stats: "unavailable", StatsErr: "test backend",
		Counters: map[string]int64{}, Admin: control.Admin{Address: testListen, State: control.AdminListening}}
	return b
}

func (b *fakeBackend) Status() control.Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.status
	st.Config = control.Config{Path: b.path, SHA256: b.cfg.SHA256, LoadedAt: b.loadedAt, RestartRequired: []string{}, LastReloadError: b.lastErr}
	if b.st != nil {
		st.Stats, st.StatsErr = "enabled", ""
	}
	return st
}

func (b *fakeBackend) Config() *config.Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

func (b *fakeBackend) ConfigPath() string { return b.path }

func (b *fakeBackend) Stats() *stats.Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st
}

// Reload loads the file as cuttle serve does and keeps the result.
func (b *fakeBackend) Reload() (control.ReloadResult, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reloads++
	res := control.ReloadResult{RestartRequired: []string{}, Warnings: []string{}}
	if err := config.CheckFileSecurity(b.path); err != nil {
		res.Errors = []string{err.Error()}
		b.lastErr = err.Error()
		return res, false
	}
	cfg, r, err := config.Load(b.path)
	if err != nil {
		for _, e := range r.Errors {
			res.Errors = append(res.Errors, e.String())
		}
		b.lastErr = strings.Join(res.Errors, "\n")
		return res, false
	}
	for _, w := range r.Warnings {
		res.Warnings = append(res.Warnings, w.String())
	}
	res.RestartRequired = config.RestartRequired(b.cfg, cfg)
	b.cfg, b.loadedAt, b.lastErr = cfg, time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), ""
	return res, true
}

func (b *fakeBackend) reloadCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reloads
}
