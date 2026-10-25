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
	// RemoveBinary: Binary's directory was created by this update, and a
	// rollback removes it.
	RemoveBinary bool `json:"remove_binary,omitempty"`
	// Committed: the new binary's server upgrade passed, so the update is
	// never rolled back; only its record is missing (rerun update apply).
	Committed bool `json:"committed,omitempty"`
	// Safety: the server upgrade's safety archive (the server as it was).
	Safety string `json:"safety_archive,omitempty"`
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

// ClearUpdateMarker removes the marker, then its snapshot; an absent one
// is not an error.
func ClearUpdateMarker(d paths.Dirs) error {
	if err := os.Remove(d.UpdateMarker()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(d.UpdateSnapshot()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// updateSnapshot is <state>/update-snapshot.json: each file's content
// before the update, null for a file that did not exist.
type updateSnapshot struct {
	Files map[string]*[]byte `json:"files"`
}

// WriteUpdateSnapshot persists files (path → content, nil: absent) next to
// the marker, atomically (0600), before the marker is written: an update
// killed half way is rolled back or resumed from it.
func WriteUpdateSnapshot(d paths.Dirs, files map[string][]byte) error {
	s := updateSnapshot{Files: map[string]*[]byte{}}
	for p, raw := range files {
		if raw == nil {
			s.Files[p] = nil
			continue
		}
		b := raw
		s.Files[p] = &b
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.State, 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(d.UpdateSnapshot(), raw, 0o600)
}

// ReadUpdateSnapshot reads the persisted snapshot: nil, nil when there is
// none.
func ReadUpdateSnapshot(d paths.Dirs) (map[string][]byte, error) {
	raw, err := os.ReadFile(d.UpdateSnapshot())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s updateSnapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", d.UpdateSnapshot(), err)
	}
	files := map[string][]byte{}
	for p, b := range s.Files {
		if b == nil {
			files[p] = nil
			continue
		}
		files[p] = append([]byte{}, *b...)
	}
	return files, nil
}

// UpdateIncomplete reports whether the marker exists (readable or not).
// While it does every session and backfill send holds, as with the
// restore marker.
func UpdateIncomplete(d paths.Dirs) bool {
	_, err := os.Stat(d.UpdateMarker())
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
