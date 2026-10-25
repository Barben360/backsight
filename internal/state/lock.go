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
	owner := Owner{PID: os.Getpid(), Command: command, Since: time.Now().UTC().Truncate(time.Second)}
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
	e.Stale = e.Owner.PID > 0 && !processAlive(e.Owner.PID)
	return e
}

// BreakStale removes the lock e reported, only if it was stale and the file
// still holds exactly what was reported. The caller reports the takeover.
// PID reuse can make a dead owner look alive; that errs on the side of held.
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
