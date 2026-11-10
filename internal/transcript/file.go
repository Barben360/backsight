package transcript

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SessionOf is the session id of the Claude Code transcript at path: its
// file name without .jsonl (Claude Code names it after the session).
func SessionOf(path string) string { return strings.TrimSuffix(filepath.Base(path), ".jsonl") }

// Size is the transcript's size: its bytes are its content.
func Size(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Open is the transcript at path from byte from on.
func Open(path string, from int64) (io.ReadCloser, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := fh.Seek(from, io.SeekStart); err != nil {
		_ = fh.Close()
		return nil, err
	}
	return fh, nil
}
