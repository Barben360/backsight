// Package state holds backsight's runtime state: state.json and locks
// (SPEC Appendix A.3, §11.4).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// SchemaVersion of state.json this binary reads and writes.
const SchemaVersion = 1

type State struct {
	SchemaVersion           int       `json:"schema_version"`
	AppliedMigrations       []string  `json:"applied_migrations"`
	BankConfigHash          string    `json:"bank_config_hash"`
	PluginVersion           string    `json:"plugin_version"`
	ShimTarget              string    `json:"shim_target"`
	Backfill                *Backfill `json:"backfill"`
	ReplacedFrom            string    `json:"replaced_from"`
	FirstSessionNoticeShown bool      `json:"first_session_notice_shown"`
}

// Backfill tracks the replay of past transcripts (decision 0037).
type Backfill struct {
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
}

// Load reads path; a missing file is an empty state of the current schema.
func Load(path string) (State, error) {
	s := State{SchemaVersion: SchemaVersion, AppliedMigrations: []string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.SchemaVersion > SchemaVersion {
		return s, fmt.Errorf("%s: schema_version %d was written by a newer backsight (this one reads %d)", path, s.SchemaVersion, SchemaVersion)
	}
	return s, nil
}

// Save writes s atomically with mode 0600.
func Save(path string, s State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(raw, '\n'), 0o600)
}
