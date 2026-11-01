// Package fsutil writes files so that readers see either the old or the new
// content, never a partial one (SPEC §11.3).
package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Absent is the hash of a file that does not exist.
const Absent = ""

// ErrConflict means the file changed since its hash was taken.
var ErrConflict = errors.New("file changed since it was read")

// syncFile is swapped by tests to simulate a failure before the rename.
var syncFile = (*os.File).Sync

// ReadHashed returns path's content and its hex SHA-256 from one read, so a
// caller edits exactly the bytes it will later compare against. A missing
// file is (nil, Absent, nil).
func ReadHashed(path string) ([]byte, string, error) {
	b, err := ReadFileShared(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, Absent, nil
	}
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// HashFile returns the hex SHA-256 of path, or Absent when it does not exist.
func HashFile(path string) (string, error) {
	_, h, err := ReadHashed(path)
	return h, err
}

// resolve follows symlinks so a write replaces the link's target and keeps
// the link (dotfile managers symlink Claude Code's files). A missing path is
// returned as is; a dangling link is an error.
func resolve(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s is a symlink to a missing file: %w", path, err)
	}
	return resolved, nil
}

// WriteAtomic writes data to path through a synced temp file in the same
// directory and a rename, creating parent directories (0700) as needed. The
// file gets perm, whatever it had before (state and secrets need 0600).
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	return write(target, data, perm, nil, false)
}

// WritePrivate is WriteAtomic for a secret written where the user chose
// (a connection token's --out, a keys export), whose folder may be open
// to others: the temp file is created private (mode 0600; on Windows you,
// SYSTEM and Administrators only, inheritance off, set by CreateFile
// itself), and the rename keeps that DACL.
func WritePrivate(path string, data []byte) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	return write(target, data, 0o600, nil, true)
}

// WritePrivateNoFollow is WriteAtomicNoFollow made private as WritePrivate.
func WritePrivateNoFollow(path string, data []byte) error {
	if err := RegularOrAbsent(path); err != nil {
		return err
	}
	return write(path, data, 0o600, func() error { return RegularOrAbsent(path) }, true)
}

// ErrNotRegular: a path that is a symlink or not a regular file, which
// WriteAtomicNoFollow refuses.
var ErrNotRegular = errors.New("not a regular file")

// RegularOrAbsent is nil when path does not exist or is a regular file (not
// a symlink); an error wrapping ErrNotRegular, or the Lstat error,
// otherwise.
func RegularOrAbsent(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink: %w", path, ErrNotRegular)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is %w", path, ErrNotRegular)
	}
	return nil
}

// WriteAtomicNoFollow is WriteAtomic for a path another user may have
// prepared (a file in /tmp): path itself is replaced, never what a
// symlink there points to, and a symlink or a non-regular file there is
// refused (RegularOrAbsent), first and again right before the rename.
func WriteAtomicNoFollow(path string, data []byte, perm fs.FileMode) error {
	if err := RegularOrAbsent(path); err != nil {
		return err
	}
	return write(path, data, perm, func() error { return RegularOrAbsent(path) }, false)
}

// write puts data at path through a synced temp file; check, when set, runs
// right before the rename and aborts it with its error. private creates
// the temp file private (createPrivateTemp): on Windows its DACL is set
// at creation, so no other user ever holds a handle to it.
func write(path string, data []byte, perm fs.FileMode, check func() error, private bool) error {
	dir := filepath.Dir(path)
	if err := MkdirPrivate(dir); err != nil {
		return err
	}
	create := os.CreateTemp
	if private {
		create = createPrivateTemp
	}
	tmp, err := create(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if err := renameRetry(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable; best effort, Windows cannot sync a dir.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// ReplaceIfUnchanged writes path only if its current hash is want (Absent:
// the file must not exist), so a concurrent edit by the user is never lost.
// An existing file keeps its mode; perm applies to a new file. Symlinks are
// followed. The hash is checked again after the temp file is synced, right
// before the rename, so the window left is one stat-and-rename.
func ReplaceIfUnchanged(path string, data []byte, perm fs.FileMode, want string) error {
	target, err := resolve(path)
	if err != nil {
		return err
	}
	got, err := HashFile(target)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: %w", path, ErrConflict)
	}
	if fi, err := os.Stat(target); err == nil {
		perm = fi.Mode().Perm()
	}
	return write(target, data, perm, func() error {
		if got, err := HashFile(target); err != nil || got != want {
			return fmt.Errorf("%s: %w", path, ErrConflict)
		}
		return nil
	}, false)
}
