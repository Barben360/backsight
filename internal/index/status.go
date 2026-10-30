package index

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// statusName is the recovery state beside the index: times and a class,
// never an error's text.
const statusName = "status.json"

// recoveryState is <state>/index/status.json.
type recoveryState struct {
	CheckedAt       time.Time `json:"checked_at,omitzero"`        // last quick_check passed
	RebuiltAt       time.Time `json:"rebuilt_at,omitzero"`        // last automatic set-aside and rebuild
	StoppedAt       time.Time `json:"stopped_at,omitzero"`        // a second failure within 24 h
	MigrateFailedAt time.Time `json:"migrate_failed_at,omitzero"` // stats index migrate's last failure
	MigrateReason   string    `json:"migrate_reason,omitempty"`   // its class (classify)
}

func statusPath(d paths.Dirs) string { return filepath.Join(d.IndexDir(), statusName) }

// onStatusWait runs while an update waits for another writer's lock
// (tests).
var onStatusWait = func() {}

// statusMu serialises this process's read-modify-writes of status.json.
var statusMu sync.Mutex

// readStatus reads status.json: absent or unparsable, the zero state (a
// zero RebuiltAt allows one automatic rebuild, and that rebuild writes
// the file before it moves anything). Only a failed read is an error.
func readStatus(d paths.Dirs) (recoveryState, error) {
	var st recoveryState
	raw, err := os.ReadFile(statusPath(d))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if json.Unmarshal(raw, &st) != nil {
		return recoveryState{}, nil
	}
	return st, nil
}

// writeStatus writes status.json atomically, 0600, times in UTC.
func writeStatus(d paths.Dirs, st recoveryState) error {
	for _, t := range []*time.Time{&st.CheckedAt, &st.RebuiltAt, &st.StoppedAt, &st.MigrateFailedAt} {
		if !t.IsZero() {
			*t = t.UTC()
		}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(statusPath(d), append(raw, '\n'), 0o600)
}

// statusLockName is the lock of status.json's writers, across processes;
// statusLockWait bounds the wait for another writer (one holds it for a
// read and a write).
const (
	statusLockName = "status.lock"
	statusLockWait = 2 * time.Second
)

// lockStatus takes status.lock, waiting up to statusLockWait for a live
// writer; a lock whose owner is gone is taken over.
func lockStatus(d paths.Dirs) (*state.Lock, error) {
	path := filepath.Join(d.IndexDir(), statusLockName)
	deadline := time.Now().Add(statusLockWait)
	for {
		l, _, err := state.Acquire(path, "metrics index status")
		var held *state.HeldError
		if !errors.As(err, &held) || time.Now().After(deadline) {
			return l, err
		}
		onStatusWait()
		time.Sleep(10 * time.Millisecond)
	}
}

// updateStatus applies fn to status.json under statusMu and status.lock,
// so that two writers (two processes) never lose each other's fields: the
// read and the write are one step. fn returning false writes nothing.
func updateStatus(d paths.Dirs, fn func(*recoveryState) bool) error {
	statusMu.Lock()
	defer statusMu.Unlock()
	l, err := lockStatus(d)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	st, err := readStatus(d)
	if err != nil {
		return err
	}
	if !fn(&st) {
		return nil
	}
	return writeStatus(d, st)
}
