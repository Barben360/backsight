package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
)

// RestoreMarker is <state>/restore-incomplete.json: `restore` writes it
// before the server stops and removes it only once the new restore epoch is
// written and the server runs on its API key again, or a rollback finished
// (decision 0044, block 1 review; issue 285). While it
// exists every session and backfill send holds, whatever the state of the
// maintenance lock's owner, and doctor names the manual step.
type RestoreMarker struct {
	Command   string    `json:"command"`
	Safety    string    `json:"safety"`  // the safety archive of the server as it was
	Archive   string    `json:"archive"` // the archive being restored
	StartedAt time.Time `json:"started_at"`
	// Step is "key" once the epoch is written: only starting the server on
	// its API key is left, which `server start` does (and then removes
	// the marker).
	Step string `json:"step,omitempty"`
	// Image is the Hindsight image the server's data is for at the key
	// step (a server upgrade's rollback put back the version before), and
	// RolledBack that a rollback put the safety archive back: server start
	// starts that image (issue 285 review M4).
	Image      string `json:"image,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`
	// PurgeBanks: at the purge step, the banks whose purged documents are
	// still to purge again once their queues are done.
	PurgeBanks []string `json:"purge_banks,omitempty"`
	// Stuck: at the purge step, the queued operations that did not move
	// for stallAfter when the wait stopped (issue 285 review MED-A).
	Stuck []string `json:"stuck,omitempty"`
}

// KeyStep: the marker's restore wrote its epoch; the server is still to
// start on its API key.
const KeyStep = "key"

// PurgeStep: the marker's restore wrote its epochs and waits for the
// restored queues before purging again (a queued retain would recreate a
// purged document), the server on a temporary key; server start resumes
// it (issue 285 review MED-1).
const PurgeStep = "purge"

// WriteRestoreMarker writes m atomically (0600).
func WriteRestoreMarker(d paths.Dirs, m RestoreMarker) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.RestoreMarker(), append(raw, '\n'), 0o600)
}

// ReadRestoreMarker reads the marker: nil, nil when there is none. One that
// cannot be read or parsed is returned (empty) with the error: it still
// counts as a restore that did not finish.
func ReadRestoreMarker(d paths.Dirs) (*RestoreMarker, error) {
	raw, err := os.ReadFile(d.RestoreMarker())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	m := &RestoreMarker{}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, m); err != nil {
		return &RestoreMarker{}, fmt.Errorf("%s: %w", d.RestoreMarker(), err)
	}
	return m, nil
}

// RestoreIncomplete reports whether the marker exists (readable or not).
func RestoreIncomplete(d paths.Dirs) bool {
	_, err := os.Lstat(d.RestoreMarker())
	return !errors.Is(err, fs.ErrNotExist)
}

// ClearRestoreMarker removes the marker; an absent one is not an error.
func ClearRestoreMarker(d paths.Dirs) error {
	if err := os.Remove(d.RestoreMarker()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
