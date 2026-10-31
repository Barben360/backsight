//go:build !windows

package dashboard

import (
	"errors"
	"net"
	"syscall"
)

// listenConfig is the listener's configuration: the default elsewhere
// than Windows.
func listenConfig() net.ListenConfig { return net.ListenConfig{} }

// addrInUse: the bind failed because another socket holds the port.
func addrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
