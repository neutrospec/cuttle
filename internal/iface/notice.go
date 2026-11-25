package iface

import "encoding/binary"

// Routing message types and flags (net/route.h). They equal syscall's
// darwin values; notice_darwin_test.go checks that.
const (
	rtmAdd     = 0x1   // RTM_ADD
	rtmDelete  = 0x2   // RTM_DELETE
	rtmChange  = 0x3   // RTM_CHANGE
	rtmNewAddr = 0xc   // RTM_NEWADDR
	rtmDelAddr = 0xd   // RTM_DELADDR
	rtmIfInfo  = 0xe   // RTM_IFINFO
	rtfHost    = 0x4   // RTF_HOST
	rtfLLInfo  = 0x400 // RTF_LLINFO

	// rtMsghdrSize is sizeof(struct rt_msghdr); rtm_type is at offset 3
	// and rtm_flags, an int, at offset 8.
	rtMsghdrSize = 92
)

// RouteNotice classifies one message read from the routing socket (design
// 0008 D5). RTM_NEWADDR, RTM_DELADDR, and RTM_IFINFO are notices. RTM_ADD,
// RTM_DELETE, and RTM_CHANGE are notices only when neither RTF_LLINFO nor
// RTF_HOST is set, because macOS keeps ARP and neighbor entries as host
// routes. Other types are ignored. A message too short for its header is a
// notice, so a malformed read never hides a change.
func RouteNotice(msg []byte) bool {
	if len(msg) < 4 {
		return true
	}
	switch msg[3] {
	case rtmNewAddr, rtmDelAddr, rtmIfInfo:
		return true
	case rtmAdd, rtmDelete, rtmChange:
		if len(msg) < rtMsghdrSize {
			return true
		}
		flags := binary.NativeEndian.Uint32(msg[8:12])
		return flags&(rtfHost|rtfLLInfo) == 0
	}
	return false
}

// Kernel event constants (sys/kern_event.h, net/net_kev.h).
const (
	kevVendorApple   = 1 // KEV_VENDOR_APPLE
	kevNetworkClass  = 1 // KEV_NETWORK_CLASS
	kevInetSubclass  = 1 // KEV_INET_SUBCLASS
	kevDLSubclass    = 2 // KEV_DL_SUBCLASS
	kevInet6Subclass = 6 // KEV_INET6_SUBCLASS

	// kevHeaderSize is KEV_MSG_HEADER_SIZE: total_size, vendor_code,
	// kev_class, kev_subclass, id, and event_code, each a u_int32_t.
	kevHeaderSize = 24
)

// KernelNotice classifies one message read from the kernel event socket
// (design 0008 D5): vendor Apple, the network class, and the data-link,
// IPv4, or IPv6 subclass. The event body is never parsed. A message too
// short for its header is a notice, as for the routing socket.
func KernelNotice(msg []byte) bool {
	if len(msg) < kevHeaderSize {
		return true
	}
	vendor := binary.NativeEndian.Uint32(msg[4:8])
	class := binary.NativeEndian.Uint32(msg[8:12])
	subclass := binary.NativeEndian.Uint32(msg[12:16])
	if vendor != kevVendorApple || class != kevNetworkClass {
		return false
	}
	switch subclass {
	case kevDLSubclass, kevInetSubclass, kevInet6Subclass:
		return true
	}
	return false
}
