//go:build windows

package supervise

import (
	"context"
	"errors"
	"fmt"
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

// Accept waits for a client on the waiting instance, then makes the next
// one before handing this one out: the name always has an instance to
// connect to while the supervisor runs. A client that came and went
// before the connection (ERROR_NO_DATA), or any other trouble with one
// instance, costs that instance only: it is replaced and Accept waits
// again. Only Close ends it (errListenerClosed).
func (l *pipeListener) Accept() (conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		if l.done {
			return nil, errListenerClosed
		}
		h, err := l.connect()
		if errors.Is(err, errListenerClosed) {
			l.shut()
			return nil, err
		}
		if err != nil {
			// This instance only: a fresh one in its place.
			if rerr := l.renew(); rerr != nil {
				if l.pause() {
					l.shut()
					return nil, errListenerClosed
				}
			}
			continue
		}
		next, err := l.instance(false)
		if err != nil {
			// The connected client is still answered; the next Accept
			// renews the waiting instance.
			l.next = windows.InvalidHandle
			return os.NewFile(uintptr(h), l.name), nil
		}
		l.next = next
		return os.NewFile(uintptr(h), l.name), nil
	}
}

// connect waits for a client on l.next: its handle once connected,
// errListenerClosed on Close, another error for this instance's trouble.
func (l *pipeListener) connect() (windows.Handle, error) {
	if l.next == windows.InvalidHandle {
		return 0, errors.New("no waiting instance")
	}
	h := l.next
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(ev) }()
	ov := windows.Overlapped{HEvent: ev}
	switch err := windows.ConnectNamedPipe(h, &ov); {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
		return h, nil
	case errors.Is(err, windows.ERROR_IO_PENDING):
		w, err := windows.WaitForMultipleObjects([]windows.Handle{ev, l.stop}, false, windows.INFINITE)
		if err != nil || w != windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(h, &ov)
			var n uint32
			_ = windows.GetOverlappedResult(h, &ov, &n, true)
			if err == nil && w == windows.WAIT_OBJECT_0+1 {
				return 0, errListenerClosed
			}
			return 0, fmt.Errorf("waiting for a client: %v", err)
		}
		var n uint32
		if err := windows.GetOverlappedResult(h, &ov, &n, false); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return 0, err
		}
		return h, nil
	default:
		return 0, err // ERROR_NO_DATA: the client already closed its end
	}
}

// renew replaces the waiting instance with a fresh one, made before the
// old one is closed so the name never goes missing.
func (l *pipeListener) renew() error {
	next, err := l.instance(false)
	if err != nil {
		return err
	}
	if l.next != windows.InvalidHandle {
		_ = windows.CloseHandle(l.next)
	}
	l.next = next
	return nil
}

// pause waits a moment before another try, true when Close came.
func (l *pipeListener) pause() bool {
	w, _ := windows.WaitForSingleObject(l.stop, 100)
	return w == windows.WAIT_OBJECT_0
}

// shut closes the waiting instance; l.mu held.
func (l *pipeListener) shut() {
	if !l.done {
		l.done = true
		if l.next != windows.InvalidHandle {
			_ = windows.CloseHandle(l.next)
		}
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
