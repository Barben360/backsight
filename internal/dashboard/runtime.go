package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// Runtime is the content of the runtime file: which process serves the
// dashboard, on which port, since when. It holds no secret.
type Runtime struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	StartedAt time.Time `json:"started_at"`
}

// RuntimeFile is <state>/dashboard/dashboard.json.
func RuntimeFile(d paths.Dirs) string { return filepath.Join(dashboardDir(d), "dashboard.json") }

// CodesDir is <state>/dashboard/codes, where login codes wait as hashes.
func CodesDir(d paths.Dirs) string { return filepath.Join(dashboardDir(d), "codes") }

func dashboardDir(d paths.Dirs) string { return filepath.Join(d.State, "dashboard") }

// unwrittenGrace is how long an unparseable runtime file counts as being
// written (a dashboard starting), as state/lock.go's.
const unwrittenGrace = 10 * time.Second

// beforeStaleRemove runs once a runtime file is judged stale, before the
// re-read that guards its removal: tests write a fresh file there.
var beforeStaleRemove = func() {}

// RunningError refuses a claim while another dashboard runs or starts.
type RunningError struct{ Port int }

func (e *RunningError) Error() string {
	if e.Port == 0 {
		return "a dashboard is starting"
	}
	return fmt.Sprintf("a dashboard is already running on port %d", e.Port)
}

// Running reads the runtime file. No file: not running. An unparseable
// file younger than unwrittenGrace is a dashboard starting (alive, port
// 0); older, it is stale. Otherwise the dashboard is alive while its
// process runs and is the one that wrote the file.
func Running(d paths.Dirs, now time.Time) (Runtime, bool, error) {
	r, _, alive, err := inspect(RuntimeFile(d), now)
	return r, alive, err
}

// inspect is Running, with the bytes it judged (nil when there is no file).
func inspect(path string, now time.Time) (Runtime, []byte, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Runtime{}, nil, false, nil
	}
	if err != nil {
		return Runtime{}, nil, false, fmt.Errorf("read the dashboard runtime file: %w", err)
	}
	var r Runtime
	if json.Unmarshal(raw, &r) != nil {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return Runtime{}, nil, false, nil
		}
		if err != nil {
			return Runtime{}, nil, false, fmt.Errorf("read the dashboard runtime file: %w", err)
		}
		return Runtime{}, raw, now.Sub(fi.ModTime()) <= unwrittenGrace, nil
	}
	return r, raw, state.ProcessAlive(r.PID, r.StartedAt), nil
}

// Claim writes r as the runtime file, created exclusively. A file left by
// a dashboard that is gone is removed, only if it still holds the bytes it
// was judged on (state.BreakStale's rule: of two starters, one claims),
// and the claim is retried once. A dashboard alive or starting: a
// *RunningError.
func Claim(d paths.Dirs, r Runtime, now time.Time) error {
	path := RuntimeFile(d)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the dashboard directory: %w", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	for retried := false; ; retried = true {
		err := create(path, raw)
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		cur, seen, alive, err := inspect(path, now)
		if err != nil {
			return err
		}
		if alive {
			return &RunningError{Port: cur.Port}
		}
		if retried {
			return fmt.Errorf("%s: a stale dashboard runtime file came back after its removal", path)
		}
		if seen == nil {
			continue // removed meanwhile: claim again
		}
		beforeStaleRemove()
		again, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read the dashboard runtime file: %w", err)
		}
		if !bytes.Equal(again, seen) {
			continue // rewritten since judged: judge it again
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the stale dashboard runtime file: %w", err)
		}
	}
}

// create writes raw to a new file at path (fs.ErrExist when there is one)
// and syncs it; a failed write leaves no file.
func create(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	if err != nil {
		return fmt.Errorf("create the dashboard runtime file: %w", err)
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write the dashboard runtime file: %w", err)
	}
	return nil
}

// Release removes the runtime file, only while it names pid.
func Release(d paths.Dirs, pid int) error {
	path := RuntimeFile(d)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the dashboard runtime file: %w", err)
	}
	var r Runtime
	if json.Unmarshal(raw, &r) != nil || r.PID != pid {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the dashboard runtime file: %w", err)
	}
	return nil
}
