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
	Client      *Client    `json:"client,omitempty"`
	// Scheduler: the OS entries `jobs install` wrote (decision 0025).
	Scheduler *Scheduler `json:"scheduler,omitempty"`
	// RestoreEpoch: the bank's restore epoch the cursors were last
	// reconciled with (decision 0044).
	RestoreEpoch *RestoreEpoch `json:"restore_epoch,omitempty"`

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

// Scheduler is what `jobs install` set up: the scheduler kind and the files
// it wrote, so that `jobs uninstall` and doctor know them.
type Scheduler struct {
	Kind        string    `json:"kind"` // systemd | launchd | schtasks
	Files       []string  `json:"files,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

// RestoreEpoch is the bank restore epoch this client reconciled with.
type RestoreEpoch struct {
	Bank         string    `json:"bank"`
	Epoch        int       `json:"epoch"`
	ReconciledAt time.Time `json:"reconciled_at"`
}

// EpochOf is the restore epoch recorded for bank: 0 when none was (every
// install before M5, or another bank).
func (s State) EpochOf(bank string) int {
	if s.RestoreEpoch == nil || s.RestoreEpoch.Bank != bank {
		return 0
	}
	return s.RestoreEpoch.Epoch
}

// Client remembers Claude Code settings as they were before backsight first
// changed them, so uninstall can offer them back (decision 0052).
type Client struct {
	// RetentionBefore: absent = not recorded yet; null = the key was absent.
	RetentionBefore json.RawMessage `json:"retention_before,omitempty"`
	// Profile is the Claude Code directory client install wired ("" = the
	// default ~/.claude); ShimPrevious the binary the shim ran before, for
	// a rollback.
	Profile      string `json:"profile,omitempty"`
	ShimPrevious string `json:"shim_previous,omitempty"`
}

// Backfill tracks the replay of past transcripts (decision 0037).
type Backfill struct {
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
	// Budget (decision 0051): a cap in currency or in tokens and the spend
	// measured so far.
	MaxCost     float64 `json:"max_cost,omitempty"`
	MaxTokens   int64   `json:"max_tokens,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	SpentCost   float64 `json:"spent_cost"`
	SpentTokens int64   `json:"spent_tokens"`
	// StartDay (UTC) and its token sums when the backfill started: spend is
	// the start day's increase plus every later day.
	StartDay  string   `json:"start_day"`
	StartBase [4]int64 `json:"start_base"` // input, cached, output, thoughts
	// Paused: budget | server | measure | interrupted | "" (running).
	Paused            string   `json:"paused,omitempty"`
	AllowSubscription bool     `json:"allow_subscription,omitempty"`
	Filters           string   `json:"filters,omitempty"`  // since|project|projects-dir of the queue
	Sessions          []string `json:"sessions,omitempty"` // still to send, oldest first
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
// It never waits: a held lock returns the *HeldError (a stale one is
// taken over). fn's error aborts
// without writing.
func Update(d paths.Dirs, command string, fn func(*State) error) error {
	l, _, err := Acquire(filepath.Join(d.LocksDir(), "state.lock"), command)
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
