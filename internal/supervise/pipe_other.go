//go:build !windows

package supervise

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// The control channel's stand-in for the tests: a unix socket in runDir.

func socketPath(project string) (string, error) {
	dir, err := runDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, project+".sock"), nil
}

type unixListener struct{ l net.Listener }

func (u unixListener) Accept() (conn, error) { return u.l.Accept() }
func (u unixListener) Close() error          { return u.l.Close() }

// listen opens the project's socket; the caller holds the lock, so a
// socket left there is a dead supervisor's.
func listen(project string) (listener, error) {
	path, err := socketPath(project)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return unixListener{l}, nil
}

func dial(ctx context.Context, project string) (conn, error) {
	path, err := socketPath(project)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return nil, ErrNotRunning
	}
	if err != nil {
		return nil, err
	}
	return c.(*net.UnixConn), nil
}
