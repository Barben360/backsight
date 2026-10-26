package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Owner is the content of a lock file.
type Owner struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Since   time.Time `json:"since"`
	// StartTicks and BootID identify the owner process beyond its PID
	// (Linux: its start time in ticks since boot, and that boot): a PID
	// reused by another process, or after a reboot, does not match.
	StartTicks int64  `json:"start_ticks,omitempty"`
	BootID     string `json:"boot_id,omitempty"`
}

// self is this process as a lock owner.
func self(command string) Owner {
	o := Owner{PID: os.Getpid(), Command: command, Since: time.Now().UTC().Truncate(time.Second)}
	if ticks, ok := startTicks(o.PID); ok {
		o.StartTicks, o.BootID = ticks, bootID()
	}
	return o
}

// HeldError reports a lock held by someone else. Stale means its owner
// process is gone (or it was never written and is older than
// unwrittenGrace): the lock is reported, and only BreakStale removes it.
type HeldError struct {
	Path  string
	Owner Owner
	Stale bool
	raw   []byte // content when reported, so BreakStale removes only that lock
}

// unwrittenGrace is how long an unparseable lock counts as being written.
const unwrittenGrace = 10 * time.Second

func (e *HeldError) Error() string {
	if e.Stale {
		return fmt.Sprintf("%s is held by %q (pid %d, since %s) whose process is gone: remove it once you have checked nothing is running",
			e.Path, e.Owner.Command, e.Owner.PID, e.Owner.Since.Format(time.RFC3339))
	}
	return fmt.Sprintf("%s is held by %q (pid %d, since %s)", e.Path, e.Owner.Command, e.Owner.PID, e.Owner.Since.Format(time.RFC3339))
}

// Lock is a held lock file.
type Lock struct {
	path  string
	owner Owner
}

// TryAcquire creates the lock file exclusively or returns a *HeldError.
func TryAcquire(path, command string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	owner := self(command)
	raw, err := json.Marshal(owner)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, held(path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &Lock{path: path, owner: owner}, nil
}

// Acquire is TryAcquire that takes over a lock whose owner is gone (a
// crash, an OOM kill, a timer's timeout): it breaks it once and retries.
// broke reports the takeover, for the caller to say so.
func Acquire(path, command string) (l *Lock, broke bool, err error) {
	l, err = TryAcquire(path, command)
	var held *HeldError
	if errors.As(err, &held) && held.Stale && BreakStale(held) == nil {
		l, err = TryAcquire(path, command)
		broke = err == nil
	}
	return l, broke, err
}

// MaintenanceHeld reports whether a live process holds the maintenance lock
// at path. A lock left by a dead owner (a crash mid-backup) does not stop
// the hooks and jobs: doctor reports it, and the next backup breaks it.
func MaintenanceHeld(path string) bool {
	h := Inspect(path)
	return h != nil && !h.Stale
}

// Inspect reports who holds the lock at path, nil when nobody does; it
// never takes the lock (a check must not look like maintenance to others).
func Inspect(path string) *HeldError {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var e *HeldError
	errors.As(held(path), &e)
	return e
}

func held(path string) error {
	e := &HeldError{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		return e // unreadable: held
	}
	e.raw = raw
	if json.Unmarshal(raw, &e.Owner) != nil {
		// Being written right now, or left empty by a crash between create
		// and write: stale only once it is old enough.
		fi, err := os.Stat(path)
		e.Stale = err == nil && time.Since(fi.ModTime()) > unwrittenGrace
		return e
	}
	e.Stale = e.Owner.PID > 0 && !ownerAlive(e.Owner)
	return e
}

// startSlack: a lock's Since is its owner's clock, truncated to the
// second, and a start time read from /proc is rounded to the tick and the
// boot second.
const startSlack = 5 * time.Second

// ownerAlive: a process with the owner's PID runs and it is the owner. A
// lock with the owner's start ticks and boot id is compared with them
// exactly: no clock step changes them. A lock without them (written by an
// older build) compares the process's start time with Since, as
// heldByUpdate does: a process that started after the lock was taken got a
// dead owner's PID reused. A lock without Since, or a system that does not
// tell a start time, falls back on the PID alone.
func ownerAlive(o Owner) bool {
	if !processAlive(o.PID) {
		return false
	}
	if o.StartTicks != 0 {
		if boot := bootID(); o.BootID != "" && boot != "" && boot != o.BootID {
			return false // another boot: the PID is not the owner's
		}
		ticks, ok := startTicks(o.PID)
		return !ok || ticks == o.StartTicks
	}
	if o.Since.IsZero() {
		return true
	}
	start, ok := processStart(o.PID)
	return !ok || !start.After(o.Since.Add(startSlack))
}

// BreakStale removes the lock e reported, only if it was stale and the file
// still holds exactly what was reported. The caller reports the takeover.
// A reused PID is told by its process's start time where the system gives
// it (ownerAlive); elsewhere it errs on the side of held.
func BreakStale(e *HeldError) error {
	if !e.Stale {
		return fmt.Errorf("%s: not stale, refusing to break it", e.Path)
	}
	raw, err := os.ReadFile(e.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(raw) != string(e.raw) {
		return fmt.Errorf("%s changed since it was reported, refusing to break it", e.Path)
	}
	return os.Remove(e.Path)
}

// Owner is what the lock file holds.
func (l *Lock) Owner() Owner { return l.owner }

// Release removes the lock file if it still names this lock's owner.
func (l *Lock) Release() error {
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cur Owner
	if json.Unmarshal(raw, &cur) != nil || cur.PID != l.owner.PID || !cur.Since.Equal(l.owner.Since) {
		return nil
	}
	return os.Remove(l.path)
}
