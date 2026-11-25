package iface

import (
	"syscall"
	"testing"
	"unsafe"
)

// TestNoticeConstants checks that the classifier's constants equal
// syscall's darwin values.
func TestNoticeConstants(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"RTM_ADD", rtmAdd, syscall.RTM_ADD},
		{"RTM_DELETE", rtmDelete, syscall.RTM_DELETE},
		{"RTM_CHANGE", rtmChange, syscall.RTM_CHANGE},
		{"RTM_NEWADDR", rtmNewAddr, syscall.RTM_NEWADDR},
		{"RTM_DELADDR", rtmDelAddr, syscall.RTM_DELADDR},
		{"RTM_IFINFO", rtmIfInfo, syscall.RTM_IFINFO},
		{"RTF_HOST", rtfHost, syscall.RTF_HOST},
		{"RTF_LLINFO", rtfLLInfo, syscall.RTF_LLINFO},
		{"sizeof(rt_msghdr)", rtMsghdrSize, syscall.SizeofRtMsghdr},
		{"RTM_VERSION", rtmVersion, syscall.RTM_VERSION},
		{"RTF_UP", rtfUp, syscall.RTF_UP},
		{"RTF_GATEWAY", rtfGateway, syscall.RTF_GATEWAY},
		{"RTF_IFSCOPE", rtfIfscope, syscall.RTF_IFSCOPE},
		{"RTA_DST", rtaDst, syscall.RTA_DST},
		{"RTA_GATEWAY", rtaGateway, syscall.RTA_GATEWAY},
		{"RTA_NETMASK", rtaNetmask, syscall.RTA_NETMASK},
		{"RTAX_NETMASK", rtaxNetmask, syscall.RTAX_NETMASK},
		{"AF_INET", afInet, syscall.AF_INET},
		{"AF_INET6", afInet6, syscall.AF_INET6},
		// The ioctl requests encode their argument size (sys/ioccom.h).
		{"sizeof(ifmediareq)", int(unsafe.Sizeof(ifmediareq{})), syscall.SIOCGIFMEDIA >> 16 & 0x1fff},
		{"sizeof(ifreq)", int(unsafe.Sizeof(ifreqFunctionalType{})), siocgiffunctionaltype >> 16 & 0x1fff},
		{"sizeof(kev_request)", int(unsafe.Sizeof(kevRequest{})), siocskevfilt >> 16 & 0x1fff},
		{"SIOCGIFFUNCTIONALTYPE", siocgiffunctionaltype, 0xc0000000 | 32<<16 | 'i'<<8 | 173},
		{"SIOCSKEVFILT", siocskevfilt, 0x80000000 | 12<<16 | 'e'<<8 | 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#x, syscall has %#x", c.name, c.got, c.want)
		}
	}
}
