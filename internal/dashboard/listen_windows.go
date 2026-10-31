//go:build windows

package dashboard

import (
	"errors"
	"net"
	"syscall"
)

// soExclusiveAddrUse is SO_EXCLUSIVEADDRUSE (^SO_REUSEADDR), which the
// standard library does not name.
const soExclusiveAddrUse = -5

// wsaeAddrInUse is WSAEADDRINUSE, which the standard library does not
// name.
const wsaeAddrInUse syscall.Errno = 10048

// listenConfig binds the port exclusively: no other socket can bind it,
// even with SO_REUSEADDR, while the dashboard holds it.
func listenConfig() net.ListenConfig {
	return net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
}

// addrInUse: the bind failed because another socket holds the port
// (WSAEACCES when that socket holds it exclusively, or the port is in a
// range the system reserved).
func addrInUse(err error) bool {
	return errors.Is(err, wsaeAddrInUse) || errors.Is(err, syscall.WSAEACCES)
}
