// Package indextest gives tests a metrics index already migrated to this
// binary's head. Building one from nothing runs every migration, about
// 0.9 s under -race (0006's 32 ALTER TABLE alone 0.6 s); a package whose
// tests each open a fresh index pays that hundreds of times. Tests of the
// building and of the migrations themselves do not use it.
package indextest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/paths"
)

var (
	once     sync.Once
	template []byte
	buildErr error
)

// Seed puts an empty metrics.db at this binary's head in d's index
// directory, unless a file is already there: index.Open on d then applies
// no migration, and its catch-up ingests d's sources as on a fresh build.
// The file is built once per test binary by a real index.Open and Close
// (which checkpoints it into the main file, no WAL left) and copied.
func Seed(tb testing.TB, d paths.Dirs) {
	tb.Helper()
	once.Do(func() { template, buildErr = build() })
	if buildErr != nil {
		tb.Fatalf("indextest: building the template index: %v", buildErr)
	}
	path := filepath.Join(d.IndexDir(), index.FileName)
	if _, err := os.Lstat(path); err == nil {
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		tb.Fatalf("indextest: %v", err)
	}
	if err := os.MkdirAll(d.IndexDir(), 0o700); err != nil {
		tb.Fatalf("indextest: %v", err)
	}
	if err := os.WriteFile(path, template, 0o600); err != nil {
		tb.Fatalf("indextest: %v", err)
	}
}

// build opens and closes an index in a directory of its own and returns
// its file.
func build() ([]byte, error) {
	dir, err := os.MkdirTemp("", "backsight-indextest-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	d := paths.Dirs{Data: filepath.Join(dir, "data"), Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state"), Bin: filepath.Join(dir, "bin")}
	ix, err := index.Open(context.Background(), index.Options{Dirs: d, Home: dir, Getenv: func(string) string { return "" }})
	if err != nil {
		return nil, err
	}
	if ix.Side() {
		_ = ix.Close()
		return nil, errors.New("the template opened a side file")
	}
	if err := ix.Close(); err != nil {
		return nil, err
	}
	path := filepath.Join(d.IndexDir(), index.FileName)
	for _, side := range []string{"-wal", "-journal"} {
		if st, err := os.Stat(path + side); err == nil && st.Size() > 0 {
			return nil, fmt.Errorf("%s%s left after close (%d bytes): the file alone is not the index", index.FileName, side, st.Size())
		}
	}
	return os.ReadFile(path)
}
