// Package state holds backsight's runtime state: state.json and locks
// (SPEC Appendix A.3, §11.4).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
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
	// RetainSince: live retain only takes sessions started after it; older
	// ones belong to backfill (decision 0050).
	RetainSince *time.Time `json:"retain_since,omitempty"`

	// extra holds fields this binary does not know (written by a newer
	// minor version), saved back unchanged.
	extra map[string]json.RawMessage
}

type plain State // State without its JSON methods

func (s *State) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, (*plain)(s)); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for _, k := range knownKeys() {
		delete(all, k)
	}
	if len(all) > 0 {
		s.extra = all
	}
	return nil
}

func (s State) MarshalJSON() ([]byte, error) {
	if s.AppliedMigrations == nil {
		s.AppliedMigrations = []string{}
	}
	known, err := json.Marshal(plain(s))
	if err != nil || len(s.extra) == 0 {
		return known, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(known, &all); err != nil {
		return nil, err
	}
	for k, v := range s.extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

func knownKeys() []string {
	t := reflect.TypeOf(plain{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

// Backfill tracks the replay of past transcripts (decision 0037).
type Backfill struct {
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
	// Budget (decision 0051): a cap in currency or in tokens, the spend
	// measured so far, and the last token snapshot it was measured from.
	MaxCost     float64  `json:"max_cost,omitempty"`
	MaxTokens   int64    `json:"max_tokens,omitempty"`
	Currency    string   `json:"currency,omitempty"`
	SpentCost   float64  `json:"spent_cost"`
	SpentTokens int64    `json:"spent_tokens"`
	Last        [4]int64 `json:"last_tokens"`        // input, cached, output, thoughts
	Paused      string   `json:"paused,omitempty"`   // budget | server | ""
	Sessions    []string `json:"sessions,omitempty"` // still to send, oldest first
}

// Load reads path; a missing file is an empty state of the current schema.
// Load then Save is not safe against concurrent writers: use Update.
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

// Update loads state.json, applies fn and saves it, under the state lock.
// It never waits: a held lock returns the *HeldError. fn's error aborts
// without writing.
func Update(d paths.Dirs, command string, fn func(*State) error) error {
	l, err := TryAcquire(filepath.Join(d.LocksDir(), "state.lock"), command)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()
	s, err := Load(d.StateFile())
	if err != nil {
		return err
	}
	if err := fn(&s); err != nil {
		return err
	}
	return Save(d.StateFile(), s)
}
