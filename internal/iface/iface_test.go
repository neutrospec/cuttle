package iface_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"squid/internal/iface"
	"squid/internal/logx"
	"squid/internal/stats"
	"squid/internal/testenv"
)

func addrs(list ...string) []netip.Addr {
	out := make([]netip.Addr, len(list))
	for i, a := range list {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// TestInterfaceState checks matching and the online predicate (design 0008
// D3, D4, I2).
func TestInterfaceState(t *testing.T) {
	ethA := testenv.Row(7, "en7", 0x0a, "198.18.0.7")
	byMAC := iface.Def{Name: "eth-a", MAC: testenv.TestMAC(0x0a)}
	byDevice := iface.Def{Name: "eth-a", Device: "en7"}
	both := iface.Def{Name: "eth-a", MAC: testenv.TestMAC(0x0a), Device: "en7"}
	row := func(change func(*iface.Link)) iface.Link {
		l := testenv.Row(7, "en7", 0x0a, "198.18.0.7")
		change(&l)
		return l
	}
	lo := iface.Link{Index: 1, Device: "lo0", Up: true, Loopback: true, Addrs: addrs("127.0.0.1"), Default4: iface.RouteScoped}
	for _, c := range []struct {
		name     string
		def      iface.Def
		table    []iface.Link
		reason   string // "" for online
		families iface.Families
		primary  iface.Families
		devices  string
		otherMAC bool
	}{
		{name: "mac match", def: byMAC, table: []iface.Link{lo, ethA}, families: iface.IPv4},
		{name: "device match", def: byDevice, table: []iface.Link{ethA}, families: iface.IPv4},
		{name: "both keys", def: both, table: []iface.Link{ethA}, families: iface.IPv4},
		{name: "device with another MAC", def: both, table: []iface.Link{testenv.Row(7, "en7", 0x0b, "198.18.0.7")},
			reason: iface.ReasonAbsent, otherMAC: true},
		{name: "no row", def: byMAC, table: []iface.Link{lo}, reason: iface.ReasonAbsent},
		{name: "loopback never matches", def: iface.Def{Name: "lo", Device: "lo0"}, table: []iface.Link{lo}, reason: iface.ReasonAbsent},
		{name: "two rows with one MAC", def: byMAC, table: []iface.Link{ethA, testenv.Row(9, "vlan0", 0x0a, "198.18.0.9")},
			reason: iface.ReasonAmbiguous, devices: "en7,vlan0"},
		{name: "kind mismatch", def: iface.Def{Name: "wifi", Device: "en7", Kind: iface.KindWifi}, table: []iface.Link{ethA},
			reason: iface.ReasonKindMismatch},
		{name: "kind agrees", def: iface.Def{Name: "eth-a", Device: "en7", Kind: iface.KindWired}, table: []iface.Link{ethA},
			families: iface.IPv4},
		{name: "admin down", def: byMAC, table: []iface.Link{row(func(l *iface.Link) { l.Up = false })}, reason: iface.ReasonAdminDown},
		{name: "link down", def: byMAC, table: []iface.Link{row(func(l *iface.Link) { l.Media = iface.MediaInactive })},
			reason: iface.ReasonLinkDown},
		{name: "media not reported", def: byMAC, table: []iface.Link{row(func(l *iface.Link) { l.Media = iface.MediaUnknown })},
			families: iface.IPv4},
		{name: "only link-local", def: byMAC, table: []iface.Link{row(func(l *iface.Link) { l.Addrs = addrs("169.254.1.2", "fe80::1") })},
			reason: iface.ReasonNoAddress},
		{name: "no default route", def: byMAC, table: []iface.Link{row(func(l *iface.Link) { l.Default4 = iface.RouteNone })},
			reason: iface.ReasonNoRoute},
		{name: "ipv4 only", def: byMAC, table: []iface.Link{row(func(l *iface.Link) {
			l.Addrs = addrs("198.18.0.7", "2001:db8::7")
		})}, families: iface.IPv4},
		{name: "ipv6 only", def: byMAC, table: []iface.Link{row(func(l *iface.Link) {
			l.Addrs, l.Default4, l.Default6 = addrs("fe80::7", "2001:db8::7"), iface.RouteNone, iface.RouteScoped
		})}, families: iface.IPv6},
		{name: "both families", def: byMAC, table: []iface.Link{row(func(l *iface.Link) {
			l.Addrs, l.Default6 = addrs("198.18.0.7", "2001:db8::7"), iface.RouteScoped
		})}, families: iface.IPv4 | iface.IPv6},
		{name: "primary for ipv4 only", def: byMAC, table: []iface.Link{row(func(l *iface.Link) {
			l.Addrs, l.Default4, l.Default6 = addrs("198.18.0.7", "2001:db8::7"), iface.RoutePrimary, iface.RouteScoped
		})}, families: iface.IPv4 | iface.IPv6, primary: iface.IPv4},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := iface.Evaluate(c.def, c.table, true)
			if m.Online != (c.reason == "") || m.Reason != c.reason {
				t.Errorf("online %v reason %q, want reason %q", m.Online, m.Reason, c.reason)
			}
			if m.Families != c.families || m.Primary != c.primary {
				t.Errorf("families %q primary %q, want %q %q", m.Families, m.Primary, c.families, c.primary)
			}
			if got := strings.Join(m.Devices, ","); got != c.devices {
				t.Errorf("devices %q, want %q", got, c.devices)
			}
			if m.OtherMAC != c.otherMAC {
				t.Errorf("other MAC %v", m.OtherMAC)
			}
		})
	}
	if m := iface.Evaluate(byMAC, []iface.Link{ethA}, false); m.Online || m.Reason != iface.ReasonUnknown {
		t.Errorf("unreadable table: %+v", m)
	}
}

// routeMsg builds a routing message with an rt_msghdr of 92 bytes, or a
// short one.
func routeMsg(typ byte, flags uint32, size int) []byte {
	b := make([]byte, size)
	if size >= 2 {
		binary.NativeEndian.PutUint16(b, uint16(size))
	}
	if size >= 3 {
		b[2] = 5 // RTM_VERSION
	}
	if size >= 4 {
		b[3] = typ
	}
	if size >= 12 {
		binary.NativeEndian.PutUint32(b[8:], flags)
	}
	return b
}

// TestRouteNotice checks the routing socket classifier against fixtures
// (design 0008 D5, I17).
func TestRouteNotice(t *testing.T) {
	const (
		rtmAdd, rtmDelete, rtmChange, rtmGet = 1, 2, 3, 4
		rtmNewAddr, rtmDelAddr, rtmIfInfo    = 0xc, 0xd, 0xe
		rtfUp, rtfGateway, rtfHost, rtfLL    = 0x1, 0x2, 0x4, 0x400
		rtfIfscope                           = 0x1000000
	)
	for _, c := range []struct {
		name string
		msg  []byte
		want bool
	}{
		{"RTM_NEWADDR", routeMsg(rtmNewAddr, 0, 20), true},
		{"RTM_DELADDR", routeMsg(rtmDelAddr, 0, 20), true},
		{"RTM_IFINFO", routeMsg(rtmIfInfo, 0, 112), true},
		{"default route RTM_ADD", routeMsg(rtmAdd, rtfUp|rtfGateway|rtfIfscope, 140), true},
		{"default route RTM_DELETE", routeMsg(rtmDelete, rtfUp|rtfGateway, 140), true},
		{"RTM_CHANGE", routeMsg(rtmChange, rtfUp|rtfGateway, 140), true},
		{"RTM_ADD with RTF_LLINFO", routeMsg(rtmAdd, rtfUp|rtfHost|rtfLL, 140), false},
		{"RTM_ADD with RTF_HOST", routeMsg(rtmAdd, rtfUp|rtfHost, 140), false},
		{"RTM_DELETE with RTF_LLINFO", routeMsg(rtmDelete, rtfUp|rtfLL, 140), false},
		{"RTM_DELETE with RTF_HOST", routeMsg(rtmDelete, rtfUp|rtfHost|rtfGateway, 140), false},
		{"RTM_GET", routeMsg(rtmGet, rtfUp|rtfGateway, 140), false},
		{"shorter than the type", routeMsg(0, 0, 2), true},
		{"RTM_ADD shorter than rt_msghdr", routeMsg(rtmAdd, 0, 40), true},
		{"empty", nil, true},
	} {
		if got := iface.RouteNotice(c.msg); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func kernelMsg(vendor, class, subclass uint32, size int) []byte {
	b := make([]byte, size)
	if size >= 24 {
		binary.NativeEndian.PutUint32(b, uint32(size))
		binary.NativeEndian.PutUint32(b[4:], vendor)
		binary.NativeEndian.PutUint32(b[8:], class)
		binary.NativeEndian.PutUint32(b[12:], subclass)
		binary.NativeEndian.PutUint32(b[20:], 1) // event code
	}
	return b
}

// TestKernelNotice checks the kernel event classifier against fixtures
// (design 0008 D5, I17).
func TestKernelNotice(t *testing.T) {
	for _, c := range []struct {
		name string
		msg  []byte
		want bool
	}{
		{"data link", kernelMsg(1, 1, 2, 48), true},
		{"ipv4", kernelMsg(1, 1, 1, 48), true},
		{"ipv6", kernelMsg(1, 1, 6, 48), true},
		{"another subclass", kernelMsg(1, 1, 3, 48), false},
		{"another class", kernelMsg(1, 2, 2, 48), false},
		{"another vendor", kernelMsg(2, 1, 2, 48), false},
		{"shorter than the header", kernelMsg(1, 1, 2, 10), true},
	} {
		if got := iface.KernelNotice(c.msg); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// defs3 are the test definitions eth-a, eth-b, and wifi.
func defs3() []iface.Def {
	return []iface.Def{
		{Name: "eth-a", MAC: testenv.TestMAC(0x0a)},
		{Name: "eth-b", MAC: testenv.TestMAC(0x0b)},
		{Name: "wifi", Device: "en0", Kind: iface.KindWifi},
	}
}

func rows3() []iface.Link {
	wifi := testenv.Row(4, "en0", 0x01, "198.18.0.4")
	wifi.Kind = iface.KindWifi
	return []iface.Link{testenv.Row(7, "en7", 0x0a, "198.18.0.7"), testenv.Row(8, "en8", 0x0b, "198.18.0.8"), wifi}
}

// TestInterfaceAttribution checks that an unbound connection's local
// address names the definition whose interface holds it, or other when
// none or several do (design 0008 D15, I14).
func TestInterfaceAttribution(t *testing.T) {
	rows := rows3()
	// A phone that hands out the same address as eth-b, and an unmatched
	// row.
	twin := testenv.Row(9, "en9", 0x1a, "198.18.0.8")
	stray := testenv.Row(10, "en10", 0x1b, "198.18.0.10")
	src := testenv.NewInterfaces(append(rows, twin, stray)...)
	m := iface.NewMonitor(iface.Options{Source: src, Timers: &testenv.Timers{}})
	defs := append(defs3(), iface.Def{Name: "phone-a", MAC: testenv.TestMAC(0x1a)})
	if err := m.Configure(defs, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	for addr, want := range map[string]string{
		"198.18.0.7":  "eth-a",
		"198.18.0.4":  "wifi",
		"198.18.0.8":  "other", // held by eth-b and phone-a
		"198.18.0.10": "other", // a row no definition matches
		"127.0.0.1":   "other",
	} {
		if got := m.Attribute(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
}

// recordingSource wraps the fake source and records the order of Watch
// and Read calls.
type recordingSource struct {
	*testenv.Interfaces
	mu    sync.Mutex
	calls []string
}

func (r *recordingSource) Read() ([]iface.Link, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "read")
	r.mu.Unlock()
	return r.Interfaces.Read()
}

func (r *recordingSource) Watch(ctx context.Context) <-chan struct{} {
	r.mu.Lock()
	r.calls = append(r.calls, "watch")
	r.mu.Unlock()
	return r.Interfaces.Watch(ctx)
}

// TestMonitor checks the monitor's re-reads with the fake source and an
// injected timer (design 0008 D5, I16). The darwin source's sockets are
// checked in system_darwin_test.go.
func TestMonitor(t *testing.T) {
	src := &recordingSource{Interfaces: testenv.NewInterfaces(rows3()...)}
	timers := &testenv.Timers{}
	var selfChanged int
	var mu sync.Mutex
	var logs bytes.Buffer
	m := iface.NewMonitor(iface.Options{
		Source: src, Timers: timers, Live: stats.NewLive(),
		Logger:      logx.NewLogger(&syncWriter{w: &logs, mu: &mu}, slog.LevelDebug),
		SelfChanged: func() { mu.Lock(); selfChanged++; mu.Unlock() },
	})
	defer m.Stop()

	// A configuration without definitions makes no Watch call.
	if err := m.Configure(nil, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if src.WatchCalls() != 0 || src.Reads() != 0 {
		t.Fatalf("without definitions: %d Watch calls, %d reads", src.WatchCalls(), src.Reads())
	}

	// The first definition calls Watch before the first Read, so no change
	// falls between them (TASK-0008 K-R4).
	if err := m.Configure(defs3(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	order := strings.Join(src.calls, ",")
	src.mu.Unlock()
	if order != "watch,read" {
		t.Fatalf("start order %q, want watch,read", order)
	}
	selfAfter := func() int { mu.Lock(); defer mu.Unlock(); return selfChanged }
	if selfAfter() != 1 {
		t.Fatalf("SelfChanged after the first read: %d", selfAfter())
	}

	// 20 notices within 100 ms share one re-read when the timer reaches
	// 200 ms after the first.
	start := time.Now()
	for range 20 {
		if !src.Notice() {
			t.Fatal("no active Watch call")
		}
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Logf("20 notices took %v", el)
	}
	if !timers.WaitPending(2, time.Second) { // the debounce and the safety timer
		t.Fatalf("no debounce timer: %d pending", timers.Pending())
	}
	timers.Advance(iface.Debounce - time.Millisecond)
	if n := src.Reads(); n != 1 {
		t.Fatalf("reads before 200 ms: %d", n)
	}
	timers.Advance(time.Millisecond)
	if n := src.Reads(); n != 2 {
		t.Fatalf("reads at 200 ms: %d, want 2", n)
	}
	if selfAfter() != 2 {
		t.Errorf("SelfChanged after the re-read: %d", selfAfter())
	}

	// A notice sent for a socket read error is a notice like any other.
	src.Notice()
	timers.WaitPending(2, time.Second)
	timers.Advance(iface.Debounce)
	if n := src.Reads(); n != 3 {
		t.Fatalf("reads after a read-error notice: %d, want 3", n)
	}

	// The safety re-read: one per 30 s.
	timers.Advance(iface.SafetyInterval)
	if n := src.Reads(); n != 4 {
		t.Fatalf("reads after 30 s: %d, want 4", n)
	}
	if selfAfter() != 4 {
		t.Errorf("SelfChanged after four successful reads: %d", selfAfter())
	}

	// A failed re-read calls no SelfChanged and keeps the table.
	src.FailRead(errors.New("injected read failure"))
	src.Notice()
	timers.WaitPending(2, time.Second)
	timers.Advance(iface.Debounce)
	if n := src.Reads(); n != 5 || selfAfter() != 4 {
		t.Fatalf("failed re-read: reads %d SelfChanged %d", n, selfAfter())
	}
	if st := m.PlanState("eth-a"); !st.Online {
		t.Errorf("a failed re-read lost the table: %+v", st)
	}
	src.FailRead(nil)

	// Removing the last definition ends the Watch call's context.
	if err := m.Configure(nil, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if !src.WaitWatchEnded(time.Second) || src.ActiveWatches() != 0 {
		t.Fatalf("Watch context still active after the last definition was removed")
	}
	if src.Notice() {
		t.Errorf("a notice reached a stopped monitor")
	}

	// A source is required for definitions.
	none := iface.NewMonitor(iface.Options{})
	if err := none.Configure(defs3(), time.Second); !errors.Is(err, iface.ErrNoSource) {
		t.Errorf("definitions without a source: %v", err)
	}

	// A socket read error sends a notice. After ENOBUFS or EINTR the
	// reader keeps reading; after any other error it logs once and stops,
	// so the loop never spins (TASK-0008 K-R4).
	t.Run("socket read errors", func(t *testing.T) {
		msg := routeMsg(0xc, 0, 20)
		steps := []struct {
			n   int
			err error
		}{{0, syscall.ENOBUFS}, {len(msg), nil}, {0, syscall.EINTR}, {0, syscall.EBADF}}
		reads := 0
		read := func(b []byte) (int, error) {
			if reads >= len(steps) {
				t.Fatal("read after a non-transient error")
			}
			s := steps[reads]
			reads++
			copy(b, msg)
			return s.n, s.err
		}
		sent := 0
		var buf bytes.Buffer
		iface.ReadNotices(context.Background(), "routing", read, iface.RouteNotice, func() bool { sent++; return true },
			logx.NewLogger(&buf, slog.LevelDebug))
		if reads != len(steps) || sent != 4 {
			t.Errorf("reads %d notices %d, want %d and 4", reads, sent, len(steps))
		}
		if n := strings.Count(buf.String(), "interface notice socket failed"); n != 1 {
			t.Errorf("%d failure lines:\n%s", n, buf.String())
		}
	})

	// On darwin, Watch opens the routing socket and the kernel event
	// socket without privileges and closes both when its context ends.
	t.Run("darwin sockets", func(t *testing.T) {
		if testing.Short() {
			t.Skip("opens notice sockets; skipped with -short")
		}
		var kinds []string
		var fds []int
		*iface.WatchOpened = func(kind string, fd int) { kinds, fds = append(kinds, kind), append(fds, fd) }
		defer func() { *iface.WatchOpened = nil }()
		ctx, cancel := context.WithCancel(context.Background())
		ch := iface.System{}.Watch(ctx)
		if strings.Join(kinds, ",") != "routing,kernel event" {
			cancel()
			t.Fatalf("opened %q", kinds)
		}
		for _, fd := range fds {
			if typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || typ != syscall.SOCK_RAW {
				t.Errorf("descriptor %d: type %d %v", fd, typ, err)
			}
		}
		cancel()
		done := make(chan struct{})
		go func() {
			for range ch {
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the channel did not close after the context ended")
		}
		for _, fd := range fds {
			if _, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); !errors.Is(err, syscall.EBADF) {
				t.Errorf("descriptor %d still open: %v", fd, err)
			}
		}
	})
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// rtMessage builds a routing table dump entry: rt_msghdr and its
// sockaddrs, each padded to 4 bytes.
func rtMessage(index uint16, flags uint32, version byte, sockaddrs ...[]byte) []byte {
	var addrs uint32
	body := []byte{}
	for i, sa := range sockaddrs {
		if sa == nil {
			continue
		}
		addrs |= 1 << i
		step := (len(sa) + 3) &^ 3
		if len(sa) == 0 {
			step = 4
		}
		padded := make([]byte, step)
		copy(padded, sa)
		body = append(body, padded...)
	}
	b := make([]byte, 92, 92+len(body))
	binary.NativeEndian.PutUint16(b[0:], uint16(92+len(body)))
	b[2], b[3] = version, 4 // RTM_GET
	binary.NativeEndian.PutUint16(b[4:], index)
	binary.NativeEndian.PutUint32(b[8:], flags)
	binary.NativeEndian.PutUint32(b[12:], addrs)
	return append(b, body...)
}

func sin(a string) []byte {
	b := make([]byte, 16)
	b[0], b[1] = 16, 2
	ip := netip.MustParseAddr(a).As4()
	copy(b[4:], ip[:])
	return b
}

func sin6(a string) []byte {
	b := make([]byte, 28)
	b[0], b[1] = 28, 30
	ip := netip.MustParseAddr(a).As16()
	copy(b[8:], ip[:])
	return b
}

// TestDefaultRoutes checks the routing table parser against fixtures
// (design 0008 D5, D16).
func TestDefaultRoutes(t *testing.T) {
	const up, gw, host, scoped = 0x1, 0x2, 0x4, 0x1000000
	link := []byte{20, 18, 7, 0, 6, 0, 0, 0, 'e', 'n', '7', 0, 0, 0, 0, 0, 0, 0, 0, 0}
	var rib []byte
	for _, m := range [][]byte{
		rtMessage(4, up|gw|scoped, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{}),
		rtMessage(7, up|gw|scoped, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{}),
		rtMessage(7, up|gw, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{}),
		rtMessage(20, up|gw|scoped, 5, sin6("::"), link, []byte{}),
		rtMessage(9, up|gw, 5, sin("10.0.0.0"), sin("198.18.0.1"), []byte{8, 2, 0, 0, 255, 0, 0, 0}),
		rtMessage(10, up|gw|host, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{}),
		rtMessage(11, gw, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{}),
		rtMessage(12, up, 5, sin("0.0.0.0"), link, []byte{}),
		rtMessage(13, up|gw, 4, sin("0.0.0.0"), sin("198.18.0.1"), []byte{}),
		rtMessage(14, up|gw, 5, sin6("2001:db8::"), link, []byte{0}),
		rtMessage(15, up|gw, 5, sin("0.0.0.0"), sin("198.18.0.1"), nil),
		// A zero destination with a mask that is not /0, here 0.0.0.0/1,
		// is no default route.
		rtMessage(17, up|gw, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{8, 2, 0, 0, 128, 0, 0, 0}),
		rtMessage(18, up|gw, 5, sin6("::"), link, []byte{9, 30, 0, 0, 0, 0, 0, 0, 128}),
	} {
		rib = append(rib, m...)
	}
	// A truncated last message ends the parse.
	rib = append(rib, rtMessage(16, up|gw, 5, sin("0.0.0.0"), sin("198.18.0.1"), []byte{})[:60]...)
	d4, d6 := iface.DefaultRoutes(rib)
	want4 := map[int]iface.Route{4: iface.RouteScoped, 7: iface.RoutePrimary, 15: iface.RoutePrimary}
	want6 := map[int]iface.Route{20: iface.RouteScoped}
	if fmt.Sprint(d4) != fmt.Sprint(want4) || fmt.Sprint(d6) != fmt.Sprint(want6) {
		t.Errorf("defaults %v %v, want %v %v", d4, d6, want4, want6)
	}
}

// testClock is an injected clock for demotion.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// demotionMonitor returns a monitor over rows3 with a captured log and an
// injected clock.
func demotionMonitor(t *testing.T, demotion time.Duration) (*iface.Monitor, *testClock, *stats.Live, func() string) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	var logs bytes.Buffer
	live := stats.NewLive()
	m := iface.NewMonitor(iface.Options{
		Source: testenv.NewInterfaces(rows3()...), Timers: &testenv.Timers{}, Live: live, Clock: clock.now,
		Logger: logx.NewLogger(&syncWriter{w: &logs, mu: &mu}, slog.LevelDebug),
	})
	if err := m.Configure(defs3(), demotion); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m, clock, live, func() string { mu.Lock(); defer mu.Unlock(); return logs.String() }
}

// demote reports a dial in which name timed out and wifi connected.
func demote(m *iface.Monitor, name string) {
	m.Report([]iface.Tried{{Interface: name, Result: iface.ResultTimeout}, {Interface: "wifi", Result: iface.ResultConnected}}, "wifi")
}

// TestDemotionRules checks that interface_demotion = "0s" disables
// demotion, that a changed definition starts without the demotion of the
// old one while an unchanged one keeps it (design 0008 D9, D14; TASK-0009
// N-R4 MC, MD), and that a demotion that expired unobserved ends with its
// restored line before the next one starts (N-R7).
func TestDemotionRules(t *testing.T) {
	t.Run("0s disables demotion", func(t *testing.T) {
		m, _, live, logs := demotionMonitor(t, 0)
		demote(m, "eth-a")
		if m.PlanState("eth-a").Demoted || strings.Contains(logs(), "interface demoted") ||
			live.Get(stats.InterfaceState, "interface", "eth-a", "state", iface.StateDemoted) != 0 {
			t.Errorf("interface_demotion = 0s demoted eth-a:\n%s", logs())
		}
	})

	t.Run("a changed definition starts afresh", func(t *testing.T) {
		m, _, _, logs := demotionMonitor(t, 30*time.Second)
		demote(m, "eth-a")
		demote(m, "eth-b")
		changed := defs3()
		changed[0] = iface.Def{Name: "eth-a", Device: "en7"} // the same row, found by device now
		if err := m.Configure(changed, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if st := m.PlanState("eth-a"); !st.Online || st.Demoted {
			t.Errorf("changed eth-a kept its demotion: %+v", st)
		}
		if !m.PlanState("eth-b").Demoted {
			t.Errorf("unchanged eth-b lost its demotion")
		}
		if n := strings.Count(logs(), `msg="interface state" interface=eth-a`); n != 2 {
			t.Errorf("%d state lines for eth-a, want 2 (start and the changed definition)", n)
		}
	})

	t.Run("an expired demotion ends before the next", func(t *testing.T) {
		m, clock, _, logs := demotionMonitor(t, time.Second)
		demote(m, "eth-a")
		clock.advance(2 * time.Second)
		demote(m, "eth-a") // no plan or status read looked at eth-a in between
		l := logs()
		restored := strings.Index(l, `msg="interface restored" interface=eth-a cause=expired`)
		second := strings.LastIndex(l, `msg="interface demoted" interface=eth-a`)
		if strings.Count(l, `msg="interface demoted" interface=eth-a`) != 2 || restored < 0 || restored > second {
			t.Errorf("want demoted, restored with cause=expired, demoted:\n%s", l)
		}
		if !m.PlanState("eth-a").Demoted {
			t.Errorf("eth-a not demoted again")
		}
	})
}
