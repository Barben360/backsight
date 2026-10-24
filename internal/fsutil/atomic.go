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
	b, err := os.ReadFile(path)
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
	return write(target, data, perm)
}

func write(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
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
// followed. The check and the rename are not one atomic step; the window is
// the time to write one small file.
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
	return write(target, data, perm)
}
