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

// unwrittenGrace is how long an incomplete runtime file counts as being
// written (a dashboard starting), as state/lock.go's.
const unwrittenGrace = 10 * time.Second

// claimLockWait is how long Claim waits for another starter's claim;
// claimLockPoll, how often it tries meanwhile.
var claimLockWait = 2 * time.Second

const claimLockPoll = 10 * time.Millisecond

// beforeStaleRemove runs once a runtime file is judged stale, before the
// re-read that guards its removal: tests write a fresh file there.
var beforeStaleRemove = func() {}

// claimLockBusy runs each time Claim finds the claim lock held: tests
// wait on it.
var claimLockBusy = func() {}

// RunningError refuses a claim while another dashboard runs or starts.
type RunningError struct{ Port int }

func (e *RunningError) Error() string {
	if e.Port == 0 {
		return "a dashboard is starting"
	}
	return fmt.Sprintf("a dashboard is already running on port %d", e.Port)
}

// Running reads the runtime file. No file: not running. An incomplete
// file (unparseable, or without a pid, a port or a start time) whose
// mtime is under unwrittenGrace from now, either way, is a dashboard
// starting (alive, port 0); otherwise it is stale. A complete file is
// alive while its process runs and is the one that wrote the file.
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
	if json.Unmarshal(raw, &r) != nil || !complete(r) {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return Runtime{}, nil, false, nil
		}
		if err != nil {
			return Runtime{}, nil, false, fmt.Errorf("read the dashboard runtime file: %w", err)
		}
		age := now.Sub(fi.ModTime())
		return Runtime{}, raw, age < unwrittenGrace && age > -unwrittenGrace, nil
	}
	return r, raw, state.ProcessAlive(r.PID, r.StartedAt), nil
}

// complete: r names a process, a port and a start time, as Claim writes.
func complete(r Runtime) bool { return r.PID > 0 && r.Port > 0 && !r.StartedAt.IsZero() }

// Claim writes r, which must be complete, as the runtime file, created
// exclusively. Starters claim one at a time, under an OS lock on
// dashboard/claim.lock: one waiting more than claimLockWait gets "a
// dashboard is starting". A file left by a dashboard that is gone is
// removed, only if it still holds the bytes it was judged on
// (state.BreakStale's rule), and the claim is retried once. A dashboard
// alive or starting: a *RunningError.
func Claim(d paths.Dirs, r Runtime, now time.Time) error {
	if !complete(r) {
		return fmt.Errorf("dashboard runtime %+v: a pid, a port and a start time are needed", r)
	}
	path := RuntimeFile(d)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the dashboard directory: %w", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	unlock, err := lockClaims(filepath.Join(filepath.Dir(path), "claim.lock"))
	if err != nil {
		return err
	}
	defer unlock()
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

// lockClaims takes the claim lock at path, waiting up to claimLockWait
// for another starter to finish its claim.
func lockClaims(path string) (func(), error) {
	deadline := time.Now().Add(claimLockWait)
	for {
		unlock, busy, err := tryLock(path)
		if err != nil {
			return nil, fmt.Errorf("lock the dashboard claim: %w", err)
		}
		if !busy {
			return unlock, nil
		}
		claimLockBusy()
		if !time.Now().Before(deadline) {
			return nil, &RunningError{}
		}
		time.Sleep(claimLockPoll)
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
