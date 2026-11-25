package iface

import "encoding/binary"

// Routing table constants (net/route.h, sys/socket.h). They equal
// syscall's darwin values; notice_darwin_test.go checks that.
const (
	rtmVersion   = 5         // RTM_VERSION
	rtfUp        = 0x1       // RTF_UP
	rtfGateway   = 0x2       // RTF_GATEWAY
	rtfIfscope   = 0x1000000 // RTF_IFSCOPE
	rtaDst       = 0x1       // RTA_DST
	rtaGateway   = 0x2       // RTA_GATEWAY
	rtaNetmask   = 0x4       // RTA_NETMASK
	afInet       = 2         // AF_INET
	afInet6      = 30        // AF_INET6
	rtaxNetmask  = 2         // RTAX_NETMASK
	sockaddrAlgn = 4         // darwin aligns routing sockaddrs to 32 bits
)

// DefaultRoutes parses a NET_RT_DUMP routing table dump and returns, per
// interface index, the IPv4 and IPv6 default routes: messages with RTF_UP
// and RTF_GATEWAY, without RTF_HOST, whose destination and mask are
// 0.0.0.0/0 or ::/0. RTF_IFSCOPE separates scoped routes from the
// primary's; the primary's wins when an index has both (design 0008 D5).
//
// It reads rt_msghdr itself, the fallback design 0008 D16 names:
// syscall.ParseRoutingMessage converts each 4-byte aligned message to a
// struct that needs 8-byte alignment, which the race detector's pointer
// checks reject. Malformed messages are skipped.
func DefaultRoutes(rib []byte) (d4, d6 map[int]Route) {
	d4, d6 = map[int]Route{}, map[int]Route{}
	for len(rib) >= 4 {
		msglen := int(binary.NativeEndian.Uint16(rib[0:2]))
		if msglen < 4 || msglen > len(rib) {
			break
		}
		msg := rib[:msglen]
		rib = rib[msglen:]
		if msg[2] != rtmVersion || len(msg) < rtMsghdrSize {
			continue
		}
		index := int(binary.NativeEndian.Uint16(msg[4:6]))
		flags := binary.NativeEndian.Uint32(msg[8:12])
		addrs := binary.NativeEndian.Uint32(msg[12:16])
		if flags&rtfUp == 0 || flags&rtfGateway == 0 || flags&rtfHost != 0 || addrs&rtaDst == 0 {
			continue
		}
		var m map[int]Route
		switch family, ok := defaultDestination(msg[rtMsghdrSize:], addrs); {
		case !ok:
			continue
		case family == afInet:
			m = d4
		case family == afInet6:
			m = d6
		default:
			continue
		}
		r := RoutePrimary
		if flags&rtfIfscope != 0 {
			r = RouteScoped
		}
		if r > m[index] {
			m[index] = r
		}
	}
	return d4, d6
}

// defaultDestination reads the sockaddrs after rt_msghdr and reports the
// destination's family and whether destination and mask are both all
// zero. An absent or zero-length mask is /0.
func defaultDestination(b []byte, addrs uint32) (family byte, ok bool) {
	for i := 0; i <= rtaxNetmask; i++ {
		if addrs&(1<<i) == 0 {
			continue
		}
		if len(b) < 1 {
			return 0, false
		}
		salen := int(b[0])
		step := sockaddrAlgn
		if salen > 0 {
			step = (salen + sockaddrAlgn - 1) &^ (sockaddrAlgn - 1)
		}
		if salen > len(b) {
			return 0, false
		}
		sa := b[:salen]
		switch i {
		case 0: // RTAX_DST
			if salen < 2 {
				return 0, false
			}
			family = sa[1]
			var addr []byte
			switch {
			case family == afInet && salen >= 8:
				addr = sa[4:8]
			case family == afInet6 && salen >= 24:
				addr = sa[8:24]
			default:
				return 0, false
			}
			if !allZero(addr) {
				return 0, false
			}
		case rtaxNetmask:
			if salen > 2 && !allZero(sa[2:]) {
				return 0, false
			}
		}
		if step > len(b) {
			b = nil
		} else {
			b = b[step:]
		}
	}
	return family, family != 0
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
