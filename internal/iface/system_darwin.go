//go:build darwin

package iface

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// Constants and structures that syscall lacks, each from its public macOS
// header (design 0008 D16).
const (
	// IFM_AVALID and IFM_ACTIVE (net/if_media.h).
	ifmAvalid = 0x1
	ifmActive = 0x2
	// SIOCGIFFUNCTIONALTYPE is _IOWR('i', 173, struct ifreq)
	// (sys/sockio.h).
	siocgiffunctionaltype = 0xc02069ad
	// SYSPROTO_EVENT (sys/sys_domain.h).
	sysprotoEvent = 1
	// SIOCSKEVFILT is _IOW('e', 2, struct kev_request) (sys/kern_event.h).
	siocskevfilt = 0x800c6502
)

// ifmediareq is struct ifmediareq (net/if.h), which the header packs to 4
// bytes: 44 bytes, the size syscall.SIOCGIFMEDIA encodes.
type ifmediareq struct {
	name    [syscall.IFNAMSIZ]byte
	current int32
	mask    int32
	status  int32
	active  int32
	count   int32
	ulist   [8]byte // int *ifm_ulist, 4-byte aligned under pack(4)
}

// ifreqFunctionalType is struct ifreq (net/if.h) with its
// ifru_functional_type member: a 16-byte name and a 16-byte union.
type ifreqFunctionalType struct {
	name [syscall.IFNAMSIZ]byte
	typ  uint32
	_    [12]byte
}

// kevRequest is struct kev_request (sys/kern_event.h).
type kevRequest struct {
	vendor, class, subclass uint32
}

// System reads the interface table and its change notices through system
// calls, without privileges and without sending a packet (design 0008
// D5).
type System struct {
	// Logger receives the warnings of Watch; nil discards them.
	Logger *slog.Logger
}

func (s System) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// Read builds the whole interface table: index, BSD name, MAC, and flags
// from net.Interfaces, addresses, media status and kind by ioctl, and the
// default routes of each family from the routing table dump.
func (s System) Read() ([]Link, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	d4, d6, err := defaultRoutes()
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	defer syscall.Close(fd)
	links := make([]Link, 0, len(ifs))
	for _, ifc := range ifs {
		l := Link{
			Index:    ifc.Index,
			Device:   ifc.Name,
			MAC:      ifc.HardwareAddr,
			Up:       ifc.Flags&net.FlagUp != 0,
			Loopback: ifc.Flags&net.FlagLoopback != 0,
			Media:    media(fd, ifc.Name),
			Kind:     KindOf(functionalType(fd, ifc.Name)),
			Default4: d4[ifc.Index],
			Default6: d6[ifc.Index],
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					l.Addrs = append(l.Addrs, ip.Unmap())
				}
			}
		}
		links = append(links, l)
	}
	return links, nil
}

// media reads the media status. A failed ioctl or a clear IFM_AVALID
// means that the interface reports none.
func media(fd int, name string) Media {
	var req ifmediareq
	copy(req.name[:], name)
	if ioctl(fd, syscall.SIOCGIFMEDIA, unsafe.Pointer(&req)) != nil || req.status&ifmAvalid == 0 {
		return MediaUnknown
	}
	if req.status&ifmActive != 0 {
		return MediaActive
	}
	return MediaInactive
}

// functionalType reads the functional type; a failed ioctl gives 0,
// unknown.
func functionalType(fd int, name string) uint32 {
	var req ifreqFunctionalType
	copy(req.name[:], name)
	if ioctl(fd, siocgiffunctionaltype, unsafe.Pointer(&req)) != nil {
		return 0
	}
	return req.typ
}

// defaultRoutes reads the routing table dump and returns the default
// routes of each family per interface index.
func defaultRoutes() (map[int]Route, map[int]Route, error) {
	rib, err := syscall.RouteRIB(syscall.NET_RT_DUMP, 0)
	if err != nil {
		return nil, nil, os.NewSyscallError("sysctl NET_RT_DUMP", err)
	}
	d4, d6 := DefaultRoutes(rib)
	return d4, d6, nil
}

// watchOpened, when set by a test, is told the descriptor of each notice
// socket that Watch opens.
var watchOpened func(kind string, fd int)

// Watch opens the routing socket and the kernel event socket for this call
// and sends one value per accepted message or socket read error until ctx
// ends; it then closes both sockets and the channel. A socket that cannot
// be opened is logged once and sends nothing (design 0008 D5).
func (s System) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	log := s.logger()
	send := func() bool {
		select {
		case ch <- struct{}{}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var wg sync.WaitGroup
	var files []*os.File
	for _, sock := range []struct {
		kind   string
		open   func() (int, error)
		accept func([]byte) bool
	}{
		{"routing", openRouting, RouteNotice},
		{"kernel event", openKernelEvents, KernelNotice},
	} {
		fd, err := sock.open()
		if err != nil {
			log.Warn("interface notice socket unavailable; relying on the periodic re-read", "socket", sock.kind, "error", err.Error())
			continue
		}
		if watchOpened != nil {
			watchOpened(sock.kind, fd)
		}
		f := os.NewFile(uintptr(fd), sock.kind+" socket")
		files = append(files, f)
		wg.Add(1)
		go func() {
			defer wg.Done()
			readNotices(ctx, sock.kind, f.Read, sock.accept, send, log)
		}()
	}
	go func() {
		<-ctx.Done()
		for _, f := range files {
			f.Close()
		}
		wg.Wait()
		close(ch)
	}()
	return ch
}

// newSocket opens a non-blocking socket with close-on-exec, so os.NewFile
// returns a pollable file whose Close ends a pending Read.
func newSocket(domain, typ, proto int) (int, error) {
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(domain, typ, proto)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, os.NewSyscallError("socket", err)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return -1, os.NewSyscallError("setnonblock", err)
	}
	return fd, nil
}

func openRouting() (int, error) {
	return newSocket(syscall.AF_ROUTE, syscall.SOCK_RAW, syscall.AF_UNSPEC)
}

// openKernelEvents opens the kernel event socket filtered to vendor Apple
// and the network class.
func openKernelEvents() (int, error) {
	fd, err := newSocket(syscall.AF_SYSTEM, syscall.SOCK_RAW, sysprotoEvent)
	if err != nil {
		return -1, err
	}
	req := kevRequest{vendor: kevVendorApple, class: kevNetworkClass}
	if err := ioctl(fd, siocskevfilt, unsafe.Pointer(&req)); err != nil {
		syscall.Close(fd)
		return -1, os.NewSyscallError("ioctl SIOCSKEVFILT", err)
	}
	return fd, nil
}

// readNotices reads one socket until ctx ends. An accepted message and a
// read error each send a notice. After ENOBUFS or EINTR it keeps reading;
// after any other error it logs once and stops reading this socket, so the
// loop never spins and the periodic re-read remains (design 0008 D5).
func readNotices(ctx context.Context, kind string, read func([]byte) (int, error), accept func([]byte) bool,
	send func() bool, log *slog.Logger) {
	buf := make([]byte, 8<<10)
	for {
		n, err := read(buf)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !send() {
				return
			}
			if errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.EINTR) {
				continue
			}
			log.Warn("interface notice socket failed; relying on the periodic re-read", "socket", kind, "error", err.Error())
			return
		}
		if accept(buf[:n]) && !send() {
			return
		}
	}
}
