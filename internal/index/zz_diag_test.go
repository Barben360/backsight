package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/paths"
)

func TestZZDiagFirstBuild(t *testing.T) {
	if os.Getenv("BS_DIAG") == "" {
		t.Skip("diag")
	}
	state := t.TempDir()
	if d := os.Getenv("BS_DIAG_DIR"); d != "" {
		state = filepath.Join(d, fmt.Sprint(time.Now().UnixNano()))
		_ = os.MkdirAll(state, 0o700)
		defer os.RemoveAll(state)
	}
	g0 := time.Now()
	indexgen.Generate(t, state, budgetSpec)
	t.Logf("generate: %v", time.Since(g0))
	db := filepath.Join(paths.Dirs{State: state}.IndexDir(), FileName)
	var stamps []time.Time
	var wal []int64
	var ix *Index
	every, _ := strconv.Atoi(os.Getenv("BS_DIAG_EVERY"))
	var ckpts []time.Duration
	var ckptFrames int64
	old := afterBatch
	afterBatch = func() {
		if every > 0 && (len(stamps)+1)%every == 0 {
			c0 := time.Now()
			var busy, log, done int64
			if err := ix.sh.w.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &log, &done); err != nil {
				t.Error(err)
			}
			ckpts = append(ckpts, time.Since(c0))
			ckptFrames += done
		}
		stamps = append(stamps, time.Now())
		if fi, err := os.Stat(db + "-wal"); err == nil {
			wal = append(wal, fi.Size())
		} else {
			wal = append(wal, -1)
		}
	}
	defer func() { afterBatch = old }()
	start := time.Now()
	budget := time.Hour
	if b := os.Getenv("BS_DIAG_BUDGET"); b != "" {
		budget, _ = time.ParseDuration(b)
	}
	ix, err := Open(context.Background(), benchOptions(t, state))
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	o := ix.opts
	o.Now = func() time.Time { return genNow.Add(time.Since(opened)) }
	ix.opts = o
	fr, err := ix.CatchUp(context.Background(), budget)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	_ = ix.Close()
	closed := time.Now()
	var ds []time.Duration
	prev := opened
	for _, s := range stamps {
		ds = append(ds, s.Sub(prev))
		prev = s
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	fi, _ := os.Stat(db)
	var dbSize int64
	if fi != nil {
		dbSize = fi.Size()
	}
	t.Logf("open %v, catchup %v (lines %d, partial %v), close %v, commits %d, db %d MiB", opened.Sub(start), end.Sub(opened), fr.Lines, fr.Partial, closed.Sub(end), len(stamps), dbSize>>20)
	if n := len(sorted); n > 0 {
		t.Logf("per-commit: min %v p50 %v p90 %v max %v; tail after last commit %v", sorted[0], sorted[n/2], sorted[n*9/10], sorted[n-1], end.Sub(prev))
	}
	if len(ckpts) > 0 {
		var tot time.Duration
		for _, c := range ckpts {
			tot += c
		}
		t.Logf("manual checkpoints: %d, total %v, frames %d (%d MiB)", len(ckpts), tot, ckptFrames, ckptFrames*4096>>20)
	}
	if os.Getenv("BS_DIAG_V") != "" {
		for i, d := range ds {
			t.Logf("%3d %8v wal=%d", i, d.Round(time.Millisecond), wal[i])
		}
	}
}

// TestZZDiagFlush: FlushFileBuffers (fsync) cost against the bytes written since the last one.
func TestZZDiagFlush(t *testing.T) {
	if os.Getenv("BS_DIAG_FLUSH") == "" {
		t.Skip("diag")
	}
	dir := t.TempDir()
	if d := os.Getenv("BS_DIAG_DIR"); d != "" {
		dir = d
	}
	f, err := os.Create(filepath.Join(dir, "flush.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	page := make([]byte, 4096)
	for _, mib := range []int{0, 1, 4, 16, 64, 0, 4} {
		var off int64
		for range 3 {
			w0 := time.Now()
			for i := 0; i < mib*256; i++ {
				page[0] = byte(i)
				if _, err := f.WriteAt(page, off); err != nil {
					t.Fatal(err)
				}
				off += 4096
			}
			wd := time.Since(w0)
			s0 := time.Now()
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			t.Logf("write %3d MiB in %v (%d WriteFile), sync %v", mib, wd.Round(time.Millisecond), mib*256, time.Since(s0).Round(time.Millisecond))
		}
	}
}

// TestZZDiagRandomFlush: fsync after N random 4 KiB page writes in a 256 MiB file.
func TestZZDiagRandomFlush(t *testing.T) {
	if os.Getenv("BS_DIAG_FLUSH") == "" {
		t.Skip("diag")
	}
	dir := t.TempDir()
	if d := os.Getenv("BS_DIAG_DIR"); d != "" {
		dir = d
	}
	f, err := os.Create(filepath.Join(dir, "rflush.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	page := make([]byte, 4096)
	const pages = 65536
	if err := f.Truncate(pages * 4096); err != nil {
		t.Fatal(err)
	}
	s0 := time.Now()
	for i := 0; i < pages; i += 256 {
		if _, err := f.WriteAt(make([]byte, 1<<20), int64(i)*4096); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Sync()
	t.Logf("fill 256 MiB + sync: %v", time.Since(s0).Round(time.Millisecond))
	x := uint64(88172645463325252)
	for _, n := range []int{256, 1024, 4096, 16384} {
		w0 := time.Now()
		for i := 0; i < n; i++ {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			if _, err := f.WriteAt(page, int64(x%pages)*4096); err != nil {
				t.Fatal(err)
			}
		}
		wd := time.Since(w0)
		s0 := time.Now()
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		t.Logf("random write %5d pages in %v, sync %v", n, wd.Round(time.Millisecond), time.Since(s0).Round(time.Millisecond))
	}
}
