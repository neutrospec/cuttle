//go:build darwin

package netx

import (
	"fmt"
	"os"
	"syscall"
)

// BindControl returns a net.Dialer control function that binds the socket
// to the interface with this index before connect: IP_BOUND_IF for a tcp4
// socket and IPV6_BOUND_IF for a tcp6 socket (design 0008 D7). Go passes
// tcp4 or tcp6, never tcp, because cuttle dials address literals. An index
// that names no interface fails with ENXIO.
func BindControl(index int) func(network, address string, c syscall.RawConn) error {
	return func(network, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			switch network {
			case "tcp4":
				serr = os.NewSyscallError("setsockopt IP_BOUND_IF", syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, index))
			case "tcp6":
				serr = os.NewSyscallError("setsockopt IPV6_BOUND_IF", syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF, index))
			default:
				serr = fmt.Errorf("bind to interface: unexpected network %q", network)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
}
