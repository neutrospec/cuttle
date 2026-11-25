package testenv

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/neutrospec/cuttle/internal/iface"
)

// Interfaces is the injected interface source of design 0008 for tests
// (design 0007 D3): the test sets its rows, sends change notices, and can
// make Read fail. It records each Read and each Watch call, and when that
// call's context ends.
type Interfaces struct {
	mu      sync.Mutex
	rows    []iface.Link
	readErr error
	reads   int
	watches []*watchCall
}

type watchCall struct {
	ctx     context.Context
	ch      chan struct{}
	senders sync.WaitGroup
	ended   chan struct{}
}

// NewInterfaces returns a source with these rows.
func NewInterfaces(rows ...iface.Link) *Interfaces {
	return &Interfaces{rows: rows}
}

// Set replaces the rows.
func (f *Interfaces) Set(rows ...iface.Link) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

// Update changes the row of one device in place.
func (f *Interfaces) Update(device string, change func(*iface.Link)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].Device == device {
			change(&f.rows[i])
			return
		}
	}
	panic("testenv: no interface row " + device)
}

// Remove deletes the row of one device, as when it is detached.
func (f *Interfaces) Remove(device string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = slices.DeleteFunc(f.rows, func(l iface.Link) bool { return l.Device == device })
}

// FailRead makes Read fail with err; nil makes it succeed again.
func (f *Interfaces) FailRead(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
}

// Read returns a copy of the rows, or the injected error.
func (f *Interfaces) Read() ([]iface.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]iface.Link, len(f.rows))
	for i, l := range f.rows {
		l.Addrs = slices.Clone(l.Addrs)
		l.MAC = slices.Clone(l.MAC)
		out[i] = l
	}
	return out, nil
}

// Reads returns how many times Read was called.
func (f *Interfaces) Reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// Watch records the call and returns its notice channel, which closes
// once ctx ends.
func (f *Interfaces) Watch(ctx context.Context) <-chan struct{} {
	w := &watchCall{ctx: ctx, ch: make(chan struct{}), ended: make(chan struct{})}
	f.mu.Lock()
	f.watches = append(f.watches, w)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		w.senders.Wait()
		close(w.ch)
		close(w.ended)
	}()
	return w.ch
}

// WatchCalls returns how many times Watch was called.
func (f *Interfaces) WatchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watches)
}

// ActiveWatches returns how many Watch calls have a context that has not
// ended.
func (f *Interfaces) ActiveWatches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.watches {
		if w.ctx.Err() == nil {
			n++
		}
	}
	return n
}

// WaitWatchEnded waits until the context of every Watch call has ended
// and its channel is closed.
func (f *Interfaces) WaitWatchEnded(timeout time.Duration) bool {
	f.mu.Lock()
	ws := slices.Clone(f.watches)
	f.mu.Unlock()
	deadline := time.After(timeout)
	for _, w := range ws {
		select {
		case <-w.ended:
		case <-deadline:
			return false
		}
	}
	return true
}

// Notice sends one change notice on the newest active Watch channel and
// returns once the monitor received it. It reports false when no Watch
// call is active.
func (f *Interfaces) Notice() bool {
	f.mu.Lock()
	var w *watchCall
	for i := len(f.watches) - 1; i >= 0; i-- {
		if f.watches[i].ctx.Err() == nil {
			w = f.watches[i]
			break
		}
	}
	if w != nil {
		w.senders.Add(1)
	}
	f.mu.Unlock()
	if w == nil {
		return false
	}
	defer w.senders.Done()
	select {
	case w.ch <- struct{}{}:
		return true
	case <-w.ctx.Done():
		return false
	}
}

// TestMAC returns the documentation MAC 00:00:5e:00:53:<last> of RFC 9542.
func TestMAC(last byte) net.HardwareAddr {
	return net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x53, last}
}

// Row returns an online wired interface row with a documentation MAC, an
// address in 198.18.0.0/15, and a scoped IPv4 default route (design 0008
// acceptance criteria).
func Row(index int, device string, mac byte, addrs ...string) iface.Link {
	l := iface.Link{Index: index, Device: device, MAC: TestMAC(mac), Up: true, Kind: iface.KindWired,
		Media: iface.MediaActive, Default4: iface.RouteScoped}
	for _, a := range addrs {
		l.Addrs = append(l.Addrs, netip.MustParseAddr(a))
	}
	return l
}

// Timers is an injected timer for the monitor's debounce and safety
// re-reads (design 0007 D3). Time moves only by Advance, which runs due
// callbacks in the calling goroutine, in time order, without holding its
// lock.
type Timers struct {
	mu      sync.Mutex
	now     time.Duration
	seq     int
	pending []*fakeTimer
}

type fakeTimer struct {
	at      time.Duration
	seq     int
	f       func()
	stopped bool
}

// AfterFunc schedules f after d of injected time.
func (t *Timers) AfterFunc(d time.Duration, f func()) func() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	ft := &fakeTimer{at: t.now + d, seq: t.seq, f: f}
	t.pending = append(t.pending, ft)
	return func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		was := !ft.stopped && slices.Contains(t.pending, ft)
		ft.stopped = true
		t.pending = slices.DeleteFunc(t.pending, func(x *fakeTimer) bool { return x == ft })
		return was
	}
}

// Advance moves injected time forward by d and runs every callback that
// falls due, including callbacks scheduled by earlier ones.
func (t *Timers) Advance(d time.Duration) {
	t.mu.Lock()
	target := t.now + d
	t.mu.Unlock()
	for {
		t.mu.Lock()
		sort.Slice(t.pending, func(i, j int) bool {
			if t.pending[i].at != t.pending[j].at {
				return t.pending[i].at < t.pending[j].at
			}
			return t.pending[i].seq < t.pending[j].seq
		})
		if len(t.pending) == 0 || t.pending[0].at > target {
			t.now = target
			t.mu.Unlock()
			return
		}
		ft := t.pending[0]
		t.pending = t.pending[1:]
		t.now = ft.at
		t.mu.Unlock()
		ft.f()
	}
}

// Pending returns how many callbacks are scheduled.
func (t *Timers) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// WaitPending waits until at least n callbacks are scheduled.
func (t *Timers) WaitPending(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for t.Pending() < n {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
