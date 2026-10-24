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
)

// Absent is the hash of a file that does not exist.
const Absent = ""

// ErrConflict means the file changed since its hash was taken.
var ErrConflict = errors.New("file changed since it was read")

// syncFile is swapped by tests to simulate a failure before the rename.
var syncFile = (*os.File).Sync

// HashFile returns the hex SHA-256 of path, or Absent when it does not exist.
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Absent, nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// WriteAtomic writes data to path through a synced temp file in the same
// directory and a rename, creating parent directories (0700) as needed.
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
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
	return nil
}

// ReplaceIfUnchanged writes path only if its current hash is want (Absent:
// the file must not exist), so a concurrent edit by the user is never lost.
// The check and the rename are not one atomic step; the window is the time
// to write one small file.
func ReplaceIfUnchanged(path string, data []byte, perm fs.FileMode, want string) error {
	got, err := HashFile(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: %w", path, ErrConflict)
	}
	return WriteAtomic(path, data, perm)
}
