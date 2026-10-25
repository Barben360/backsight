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

// UpdateMarker is <state>/update-incomplete.json: `update apply` writes it
// before the shim switches and removes it once the update finished or was
// rolled back. One left behind (the process was killed) is a doctor error
// naming the way back: the previous binary's `client install`.
type UpdateMarker struct {
	From      string    `json:"from"`     // the version being replaced
	To        string    `json:"to"`       // the version being installed
	Previous  string    `json:"previous"` // the binary the shim ran before
	Binary    string    `json:"binary"`   // the binary being installed
	WayBack   string    `json:"way_back"` // the command that returns to From
	StartedAt time.Time `json:"started_at"`
}

// WriteUpdateMarker writes m atomically (0600).
func WriteUpdateMarker(d paths.Dirs, m UpdateMarker) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.UpdateMarker(), append(raw, '\n'), 0o600)
}

// ReadUpdateMarker reads the marker: nil, nil when there is none; one
// that cannot be parsed is returned empty with the error.
func ReadUpdateMarker(d paths.Dirs) (*UpdateMarker, error) {
	raw, err := os.ReadFile(d.UpdateMarker())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	m := &UpdateMarker{}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, m); err != nil {
		return &UpdateMarker{}, fmt.Errorf("%s: %w", d.UpdateMarker(), err)
	}
	return m, nil
}

// ClearUpdateMarker removes the marker; an absent one is not an error.
func ClearUpdateMarker(d paths.Dirs) error {
	if err := os.Remove(d.UpdateMarker()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
