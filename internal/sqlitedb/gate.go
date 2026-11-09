package sqlitedb

import (
	"errors"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// ErrUpdateInProgress: a database needs a migration while `update apply`
// has not reached its commit point (a rollback must never meet a migrated
// file).
var ErrUpdateInProgress = errors.New("an update is being applied")

// UpdateGate refuses a migration while an update is uncommitted, or when
// its marker cannot be read (the safe side).
func UpdateGate(d paths.Dirs) error {
	m, err := state.ReadUpdateMarker(d)
	if err != nil || (m != nil && !m.Committed) {
		return ErrUpdateInProgress
	}
	return nil
}
