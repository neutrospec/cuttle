package iface

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"squid/internal/logx"
	"squid/internal/stats"
)

// Re-read timing (design 0008 D5).
const (
	// Debounce is the delay from the first notice after the last re-read
	// to the re-read that every notice in the window shares.
	Debounce = 200 * time.Millisecond
	// SafetyInterval is the period of the re-read that covers lost notices
	// and sockets that could not be opened.
	SafetyInterval = 30 * time.Second
)

// Interface states for status and the interface_state gauge (design 0008
// D15).
const (
	StateOnline  = "online"
	StateDemoted = "demoted"
	StateOffline = "offline"
)

var displayStates = [...]string{StateOnline, StateDemoted, StateOffline}

// Causes that end a demotion (design 0008 D9).
const (
	CauseExpired      = "expired"
	CauseStateChanged = "state_changed"
	CauseConnected    = "connected"
)

// Timers schedules the monitor's re-reads. RealTimers uses time.AfterFunc;
// tests inject a timer they advance (design 0007 D3).
type Timers interface {
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// RealTimers schedules with time.AfterFunc.
type RealTimers struct{}

// AfterFunc calls f in its own goroutine after d.
func (RealTimers) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Options are the monitor's dependencies.
type Options struct {
	// Source is the interface table; it may be nil only while no
	// definition exists.
	Source Source
	Logger *slog.Logger
	Live   *stats.Live
	// Clock times state changes and demotion; nil means time.Now.
	Clock  func() time.Time
	Timers Timers // nil means RealTimers
	// SelfChanged is called after each successful re-read, so the self
	// address class follows new addresses (design 0008 D5).
	SelfChanged func()
	// Reconnect is called, outside the monitor's lock, when an interface
	// comes online and when its demotion ends with cause state_changed or
	// connected, so the transports whose lists hold it close their idle
	// connections (design 0008 D12).
	Reconnect func(name string)
}

// ErrNoSource refuses definitions without an interface source (design
// 0008 Interfaces).
var ErrNoSource = errors.New("interface definitions need an interface source")

// Monitor keeps the state of the configured interfaces: it reads the
// table at start, at every reload, after notices and bound-dial failures
// that show a stale table, and every SafetyInterval; it logs transitions,
// publishes the interface_state gauge, and remembers demotions (design
// 0008 D3-D5, D9, D15). It runs only while definitions exist.
type Monitor struct {
	opt Options

	// readMu serializes re-reads, so their results apply in order.
	readMu sync.Mutex

	mu       sync.Mutex
	defs     []Def
	entries  map[string]*entry
	demotion time.Duration
	table    []Link
	readable bool // the table was read successfully at least once
	failing  bool // in a run of failed re-reads
	running  bool
	cancel   context.CancelFunc
	gen      int // changes when the monitor stops, so stale timers do nothing
	pending  bool
	stopDeb  func() bool
	stopSafe func() bool
}

// entry is the state of one definition.
type entry struct {
	def   Def
	m     Match
	since time.Time // start of the displayed state
	// until is the end of a demotion, zero when not demoted.
	until    time.Time
	identity string // the D3 identity condition last logged
}

// NewMonitor returns a monitor without definitions.
func NewMonitor(opt Options) *Monitor {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Live == nil {
		opt.Live = stats.NewLive()
	}
	if opt.Clock == nil {
		opt.Clock = time.Now
	}
	if opt.Timers == nil {
		opt.Timers = RealTimers{}
	}
	return &Monitor{opt: opt, entries: map[string]*entry{}}
}

// Check reports whether Configure would accept defs.
func (m *Monitor) Check(defs []Def) error {
	if len(defs) > 0 && m.opt.Source == nil {
		return ErrNoSource
	}
	return nil
}

// Configure applies the definitions of a configuration, at start and at
// every reload, before the new snapshot is used (design 0008 D14). The
// first definition starts the monitor: Watch is called before the table is
// read, so no change falls between them. With definitions already in
// place the table is read again. A definition whose mac, device, and kind
// are unchanged keeps its state, since time, and demotion. Removing the
// last definition ends the Watch call's context.
func (m *Monitor) Configure(defs []Def, demotion time.Duration) error {
	if err := m.Check(defs); err != nil {
		return err
	}
	m.mu.Lock()
	if len(defs) == 0 {
		m.stopLocked()
		for name := range m.entries {
			m.deleteGaugesLocked(name)
		}
		m.defs, m.entries, m.demotion = nil, map[string]*entry{}, demotion
		// A later first definition starts from a fresh read, as at start.
		m.table, m.readable, m.failing = nil, false, false
		m.mu.Unlock()
		return nil
	}
	start := !m.running
	if start {
		ctx, cancel := context.WithCancel(context.Background())
		m.running, m.cancel = true, cancel
		ch := m.opt.Source.Watch(ctx)
		go func() {
			for range ch {
				m.notice()
			}
		}()
	}
	m.mu.Unlock()

	m.readMu.Lock()
	links, err := m.opt.Source.Read()
	m.mu.Lock()
	var after []func()
	m.applyReadLocked(links, err, start, &after)
	now := m.opt.Clock()
	next := make(map[string]*entry, len(defs))
	for _, d := range defs {
		if e := m.entries[d.Name]; e != nil && e.def.Same(d) {
			e.def = d
			m.updateLocked(e, now, &after)
			next[d.Name] = e
			continue
		}
		e := &entry{def: d, since: now}
		e.m = Evaluate(d, m.table, m.readable)
		m.logStateLocked(e)
		m.identityLocked(e)
		next[d.Name] = e
	}
	for name := range m.entries {
		if next[name] == nil {
			m.deleteGaugesLocked(name)
		}
	}
	m.defs, m.entries, m.demotion = defs, next, demotion
	for _, e := range m.entries {
		m.publishLocked(e, now)
	}
	if start {
		m.armSafetyLocked()
	}
	m.mu.Unlock()
	m.readMu.Unlock()
	run(after)
	return nil
}

// Stop ends the Watch call and the timers. The proxy calls it at
// shutdown.
func (m *Monitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *Monitor) stopLocked() {
	if !m.running {
		return
	}
	m.cancel()
	m.running, m.cancel, m.pending = false, nil, false
	m.gen++
	if m.stopDeb != nil {
		m.stopDeb()
		m.stopDeb = nil
	}
	if m.stopSafe != nil {
		m.stopSafe()
		m.stopSafe = nil
	}
}

func run(fs []func()) {
	for _, f := range fs {
		f()
	}
}

// notice schedules a re-read Debounce after the first notice since the
// last re-read; later notices in the window share it.
func (m *Monitor) notice() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduleLocked()
}

func (m *Monitor) scheduleLocked() {
	if !m.running || m.pending {
		return
	}
	m.pending = true
	gen := m.gen
	m.stopDeb = m.opt.Timers.AfterFunc(Debounce, func() {
		m.mu.Lock()
		if gen != m.gen {
			m.mu.Unlock()
			return
		}
		m.pending, m.stopDeb = false, nil
		m.mu.Unlock()
		m.reread(gen)
	})
}

func (m *Monitor) armSafetyLocked() {
	gen := m.gen
	m.stopSafe = m.opt.Timers.AfterFunc(SafetyInterval, func() {
		m.mu.Lock()
		if gen != m.gen {
			m.mu.Unlock()
			return
		}
		m.armSafetyLocked()
		m.mu.Unlock()
		m.reread(gen)
	})
}

// StaleTable schedules a re-read after a bound dial failed with ENXIO,
// EADDRNOTAVAIL, or ENETDOWN, sharing the notice window (design 0008 D5).
func (m *Monitor) StaleTable() { m.notice() }

// reread reads the table and applies it, unless the monitor stopped since
// the re-read was scheduled.
func (m *Monitor) reread(gen int) {
	m.readMu.Lock()
	defer m.readMu.Unlock()
	m.mu.Lock()
	if gen != m.gen {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	links, err := m.opt.Source.Read()
	m.mu.Lock()
	if gen != m.gen {
		m.mu.Unlock()
		return
	}
	var after []func()
	m.applyReadLocked(links, err, false, &after)
	now := m.opt.Clock()
	for _, d := range m.defs {
		m.updateLocked(m.entries[d.Name], now, &after)
	}
	m.mu.Unlock()
	run(after)
}

// applyReadLocked stores a successful read, or logs a failed one: at start
// one warning names the error, and later failures keep the previous table
// and log once per run of failures (design 0008 D5).
func (m *Monitor) applyReadLocked(links []Link, err error, start bool, after *[]func()) {
	if err != nil {
		if !m.failing {
			if start && !m.readable {
				m.opt.Logger.Warn("interface table unreadable", "error", err.Error())
			} else {
				m.opt.Logger.Warn("interface table re-read failed; keeping the previous table", "error", err.Error())
			}
		}
		m.failing = true
		return
	}
	if m.failing && m.readable {
		m.opt.Logger.Info("interface table readable again")
	}
	m.table, m.readable, m.failing = links, true, false
	if f := m.opt.SelfChanged; f != nil {
		*after = append(*after, f)
	}
}

// updateLocked evaluates one definition against the current table and
// handles its transitions.
func (m *Monitor) updateLocked(e *entry, now time.Time, after *[]func()) {
	old := e.m
	e.m = Evaluate(e.def, m.table, m.readable)
	name := e.def.Name
	reconnect := false
	switch {
	case old.Online && !e.m.Online:
		device := e.m.Link.Device
		if device == "" {
			device = old.Link.Device
		}
		m.opt.Logger.Warn("interface offline", "interface", name, "device", device, "reason", e.m.Reason)
		e.since = now
	case !old.Online && e.m.Online:
		m.opt.Logger.Info("interface online", "interface", name, "device", e.m.Link.Device,
			"families", e.m.Families.String(), "offline_for", logx.Duration(now.Sub(e.since)))
		e.since = now
		reconnect = true
	}
	m.identityLocked(e)
	if !e.until.IsZero() && rowChanged(old, e.m) {
		m.restoreLocked(e, now, CauseStateChanged)
		reconnect = true
	}
	m.publishLocked(e, now)
	if reconnect && m.opt.Reconnect != nil {
		f := m.opt.Reconnect
		*after = append(*after, func() { f(name) })
	}
}

// rowChanged reports a change of the matched row that ends a demotion:
// its index, device, families, or reason (design 0008 D9).
func rowChanged(a, b Match) bool {
	return a.Online != b.Online || a.Reason != b.Reason || a.Families != b.Families ||
		a.Link.Index != b.Link.Index || a.Link.Device != b.Link.Device
}

// logStateLocked writes the info line that gives a definition's state when
// it is first configured (design 0008 D15).
func (m *Monitor) logStateLocked(e *entry) {
	attrs := []any{"interface", e.def.Name, "device", e.m.Link.Device}
	if e.m.Online {
		attrs = append(attrs, "state", StateOnline, "families", e.m.Families.String())
	} else {
		attrs = append(attrs, "state", StateOffline, "reason", e.m.Reason)
	}
	m.opt.Logger.Info("interface state", attrs...)
}

// identityLocked writes a D3 identity warning once when its condition
// starts. Messages name definitions, BSD names, and kinds, never a MAC.
func (m *Monitor) identityLocked(e *entry) {
	var cond string
	var log func()
	name := e.def.Name
	switch {
	case e.m.Reason == ReasonAmbiguous:
		devices := strings.Join(e.m.Devices, ",")
		cond = "ambiguous:" + devices
		log = func() {
			m.opt.Logger.Warn("interface definition matches several devices", "interface", name, "devices", devices)
		}
	case e.m.Reason == ReasonKindMismatch:
		l := e.m.Link
		cond = "kind:" + l.Device + ":" + l.Kind.String()
		log = func() {
			m.opt.Logger.Warn("interface kind mismatch", "interface", name, "device", l.Device,
				"kind", l.Kind.String(), "expected", e.def.Kind.String())
		}
	case e.m.OtherMAC:
		cond = "mac:" + e.def.Device
		log = func() {
			m.opt.Logger.Warn("interface device has a different MAC", "interface", name, "device", e.def.Device)
		}
	}
	if cond != e.identity {
		e.identity = cond
		if log != nil {
			log()
		}
	}
}

// displayLocked returns the displayed state, ending an expired demotion
// first: expiry is observed at the next read that looks at the interface
// (design 0008 D9).
func (m *Monitor) displayLocked(e *entry, now time.Time) string {
	if !e.until.IsZero() && !now.Before(e.until) {
		m.restoreLocked(e, now, CauseExpired)
		m.publishLocked(e, now)
	}
	switch {
	case !e.m.Online:
		return StateOffline
	case !e.until.IsZero():
		return StateDemoted
	}
	return StateOnline
}

func (m *Monitor) restoreLocked(e *entry, now time.Time, cause string) {
	e.until = time.Time{}
	e.since = now
	m.opt.Logger.Info("interface restored", "interface", e.def.Name, "cause", cause)
}

// publishLocked sets the interface_state gauge: 1 for the displayed state
// and 0 for the others.
func (m *Monitor) publishLocked(e *entry, now time.Time) {
	state := StateOffline
	if e.m.Online {
		state = StateOnline
		if !e.until.IsZero() {
			state = StateDemoted
		}
	}
	for _, s := range displayStates {
		v := int64(0)
		if s == state {
			v = 1
		}
		m.opt.Live.Set(v, stats.InterfaceState, "interface", e.def.Name, "state", s)
	}
}

func (m *Monitor) deleteGaugesLocked(name string) {
	for _, s := range displayStates {
		m.opt.Live.Delete(stats.InterfaceState, "interface", name, "state", s)
	}
}

// Defined reports whether any definition exists.
func (m *Monitor) Defined() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.defs) > 0
}

// PlanState returns what a plan needs to know about a listed interface.
func (m *Monitor) PlanState(name string) PlanState {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[name]
	if e == nil {
		return PlanState{Reason: ReasonAbsent}
	}
	state := m.displayLocked(e, m.opt.Clock())
	return PlanState{
		Online:   e.m.Online,
		Reason:   e.m.Reason,
		Families: e.m.Families,
		Index:    e.m.Link.Index,
		Demoted:  state == StateDemoted,
	}
}

// Report records the outcome of a dial (design 0008 D9): when a bound
// group connected, every interface that failed over before it is demoted,
// and a demotion of the carrying interface ends with cause connected.
func (m *Monitor) Report(tried []Tried, carried string) {
	if carried == "" {
		return
	}
	m.mu.Lock()
	now := m.opt.Clock()
	var after []func()
	for _, t := range tried {
		e := m.entries[t.Interface]
		if e == nil {
			continue
		}
		switch t.Result {
		case ResultTimeout, ResultUnreachable:
			m.demoteLocked(e, now, t.Result)
		case ResultConnected:
			if !e.until.IsZero() {
				m.restoreLocked(e, now, CauseConnected)
				m.publishLocked(e, now)
				if f := m.opt.Reconnect; f != nil {
					name := e.def.Name
					after = append(after, func() { f(name) })
				}
			}
		}
	}
	m.mu.Unlock()
	run(after)
}

// demoteLocked puts an interface behind the others for the demotion
// period. Concurrent dials that demote it write one line.
func (m *Monitor) demoteLocked(e *entry, now time.Time, reason string) {
	if m.demotion <= 0 || !e.m.Online {
		return
	}
	// A demotion that expired unobserved ends first, so its restored line
	// comes before the next demoted line.
	if !e.until.IsZero() && !now.Before(e.until) {
		m.restoreLocked(e, now, CauseExpired)
	}
	already := !e.until.IsZero() && now.Before(e.until)
	e.until = now.Add(m.demotion)
	if already {
		return
	}
	e.since = now
	m.opt.Logger.Warn("interface demoted", "interface", e.def.Name, "device", e.m.Link.Device,
		"reason", reason, "for", m.demotion.String())
	m.publishLocked(e, now)
}

// Attribute names the definition whose interface holds the local address
// of an unbound or pooled connection in the current table, or "other"
// when none or several do (design 0008 D15).
func (m *Monitor) Attribute(local netip.Addr) string {
	local = local.Unmap().WithZone("")
	m.mu.Lock()
	defer m.mu.Unlock()
	owner := ""
	for _, d := range m.defs {
		e := m.entries[d.Name]
		switch e.m.Reason {
		case ReasonAbsent, ReasonAmbiguous, ReasonUnknown, ReasonKindMismatch:
			continue
		}
		if !slices.ContainsFunc(e.m.Link.Addrs, func(a netip.Addr) bool { return a.Unmap().WithZone("") == local }) {
			continue
		}
		if owner != "" {
			return "other"
		}
		owner = d.Name
	}
	if owner == "" {
		return "other"
	}
	return owner
}

// Status is one definition's state for status displays (design 0008 D15).
type Status struct {
	Name         string
	Device       string
	State        string // StateOnline, StateDemoted, or StateOffline
	Reason       string // offline reason; "" otherwise
	Families     Families
	Primary      Families
	Since        time.Time
	DemotedUntil time.Time // zero unless demoted
}

// Status returns every definition's state in configuration order.
func (m *Monitor) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opt.Clock()
	out := make([]Status, 0, len(m.defs))
	for _, d := range m.defs {
		e := m.entries[d.Name]
		st := Status{Name: d.Name, State: m.displayLocked(e, now), Since: e.since, DemotedUntil: e.until}
		switch e.m.Reason {
		case ReasonAbsent, ReasonAmbiguous, ReasonUnknown:
		default:
			st.Device = e.m.Link.Device
		}
		if e.m.Online {
			st.Families, st.Primary = e.m.Families, e.m.Primary
		} else {
			st.Reason = e.m.Reason
		}
		out = append(out, st)
	}
	return out
}
