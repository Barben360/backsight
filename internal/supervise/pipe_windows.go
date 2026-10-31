//go:build windows

package supervise

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The control channel: the named pipe \\.\pipe\backsight-<project>-<user
// SID>, its DACL the user's alone, the first instance this process's
// (FILE_FLAG_FIRST_PIPE_INSTANCE: nobody squats the name before it), no
// remote client (E34 W7). Overlapped, so Close can cancel a wait for a
// client and the connections take deadlines.

var procWaitNamedPipe = kernel32.NewProc("WaitNamedPipeW")

func pipeName(project string) (string, error) {
	name, err := objectName(project)
	if err != nil {
		return "", err
	}
	return `\\.\pipe\` + name, nil
}

type pipeListener struct {
	name  string
	sa    *windows.SecurityAttributes
	stop  windows.Handle // event set by Close
	next  windows.Handle // the instance waiting for the next client
	mu    sync.Mutex
	done  bool
	close sync.Once
}

func listen(project string) (listener, error) {
	name, err := pipeName(project)
	if err != nil {
		return nil, err
	}
	sa, _, err := userOnly()
	if err != nil {
		return nil, err
	}
	l := &pipeListener{name: name, sa: sa}
	if l.next, err = l.instance(true); err != nil {
		return nil, err
	}
	if l.stop, err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
		_ = windows.CloseHandle(l.next)
		return nil, err
	}
	return l, nil
}

func (l *pipeListener) instance(first bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	mode := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		mode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	return windows.CreateNamedPipe(p, mode,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, maxRequest, maxRequest, 0, l.sa)
}

var errListenerClosed = errors.New("the control channel is closed")

// Accept waits for a client on the waiting instance, then makes the next
// one before handing this one out: the name always has an instance to
// connect to while the supervisor runs.
func (l *pipeListener) Accept() (conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return nil, errListenerClosed
	}
	h := l.next
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(ev) }()
	ov := windows.Overlapped{HEvent: ev}
	switch err := windows.ConnectNamedPipe(h, &ov); {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
	case errors.Is(err, windows.ERROR_IO_PENDING):
		w, err := windows.WaitForMultipleObjects([]windows.Handle{ev, l.stop}, false, windows.INFINITE)
		if err != nil || w != windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(h, &ov)
			var n uint32
			_ = windows.GetOverlappedResult(h, &ov, &n, true)
			l.shut()
			return nil, errListenerClosed
		}
		var n uint32
		if err := windows.GetOverlappedResult(h, &ov, &n, false); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			l.shut()
			return nil, err
		}
	default:
		l.shut()
		return nil, err
	}
	next, err := l.instance(false)
	if err != nil {
		l.shut()
		return nil, err
	}
	l.next = next
	return os.NewFile(uintptr(h), l.name), nil
}

// shut closes the waiting instance; l.mu held.
func (l *pipeListener) shut() {
	if !l.done {
		l.done = true
		_ = windows.CloseHandle(l.next)
	}
}

// Close stops a waiting Accept, then closes the waiting instance.
func (l *pipeListener) Close() error {
	l.close.Do(func() {
		_ = windows.SetEvent(l.stop)
		l.mu.Lock()
		l.shut()
		l.mu.Unlock()
		_ = windows.CloseHandle(l.stop)
	})
	return nil
}

// dial connects to the project's pipe: ErrNotRunning when no supervisor
// has it, a wait while every instance is busy. The server may only
// identify the client, never act as it.
func dial(ctx context.Context, project string) (conn, error) {
	name, err := pipeName(project)
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for {
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		switch {
		case err == nil:
			return os.NewFile(uintptr(h), name), nil
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
			return nil, ErrNotRunning
		case errors.Is(err, windows.ERROR_PIPE_BUSY):
			_, _, _ = procWaitNamedPipe.Call(uintptr(unsafe.Pointer(p)), 100)
		default:
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
