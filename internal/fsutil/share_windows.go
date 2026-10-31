//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OpenShared opens path for reading with FILE_SHARE_READ, WRITE and DELETE
// (Go's os.Open leaves DELETE out), so another process can rename over or
// remove the file while this handle is open; the handle keeps reading the
// bytes it opened.
func OpenShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// retryEvery is the pause between two attempts.
const retryEvery = 50 * time.Millisecond

// held is a sharing violation or an access denied: a reader (an antivirus
// scan, a hook, an editor) has the file open without FILE_SHARE_DELETE.
func held(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// retry runs op again, every retryEvery, while it fails with held, for at
// most RetryFor.
func retry(op func() error) error {
	deadline := time.Now().Add(RetryFor)
	for {
		err := op()
		if err == nil || !held(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(retryEvery)
	}
}

// RemoveRetry removes path, retrying a file another process holds for
// RetryFor. A missing path is os.Remove's error.
func RemoveRetry(path string) error {
	return retry(func() error { return os.Remove(path) })
}

// renameInfo is FILE_RENAME_INFO (the Ex form's Flags in place of
// ReplaceIfExists).
type renameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// posixRename renames from over to with POSIX semantics (Windows 10 1809,
// NTFS): the old to is unlinked at once, and a reader that opened it with
// FILE_SHARE_DELETE keeps its bytes. MoveFileEx cannot replace a file that
// has any open handle, shared or not.
func posixRename(from, to string) error {
	full, err := filepath.Abs(to)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(full)
	if err != nil {
		return err
	}
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(src, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	n := len(name) - 1 // without the NUL
	buf := make([]byte, unsafe.Offsetof(renameInfo{}.FileName)+uintptr(n+1)*2)
	info := (*renameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32(n * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&info.FileName[0])), n+1), name)
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, &buf[0], uint32(len(buf)))
}

// renameRetry renames from over to: with POSIX semantics where the volume
// allows, else MoveFileEx, retrying for RetryFor while to is held without
// FILE_SHARE_DELETE.
func renameRetry(from, to string) error {
	return retry(func() error {
		if posixRename(from, to) == nil {
			return nil
		}
		return os.Rename(from, to)
	})
}
