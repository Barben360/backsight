package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Record is <state>/setup.json: the last setup run, what --resume reuses
// and what --undo reverses (decision 0064). It never holds a secret.
type Record struct {
	SchemaVersion int               `json:"schema_version"`
	Run           string            `json:"run"`
	StartedAt     time.Time         `json:"started_at"`
	Profile       string            `json:"profile,omitempty"` // CLAUDE_CONFIG_DIR given to setup
	Answers       map[string]string `json:"answers"`
	Steps         []Step            `json:"steps"`
	Undo          Undo              `json:"undo"`
	Finished      bool              `json:"finished"`
}

// Step is one step's outcome.
type Step struct {
	Name   string    `json:"name"`
	Status string    `json:"status"` // done | skipped | failed | planned
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// Undo is what setup changed that it may change back.
type Undo struct {
	// ConfigRuns are the config-editor runs setup made (pre-images), in order.
	ConfigRuns []string `json:"config_runs,omitempty"`
	// CreatedConfig: setup wrote config.toml (undo keeps it, decision 0064).
	CreatedConfig bool `json:"created_config,omitempty"`
	// Client: the plugin was not installed before setup.
	Client bool `json:"client,omitempty"`
	// Scheduler: the scheduler entries were not installed before setup.
	Scheduler bool `json:"scheduler,omitempty"`
	// ServerStarted: the server was not answering before setup.
	ServerStarted bool `json:"server_started,omitempty"`
	// Timers: reference systemd user timers setup stopped (§11.5).
	Timers []string `json:"timers,omitempty"`
}

// Empty reports whether there is nothing to undo.
func (u Undo) Empty() bool {
	return len(u.ConfigRuns) == 0 && !u.Client && !u.Scheduler && !u.ServerStarted && len(u.Timers) == 0
}

// RecordVersion of setup.json.
const RecordVersion = 1

// Load reads path; a missing file is (nil, nil).
func Load(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.SchemaVersion > RecordVersion {
		return nil, fmt.Errorf("%s was written by a newer backsight", path)
	}
	return &r, nil
}

// Save writes r atomically.
func (r *Record) Save(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'), 0o600)
}

// Done reports whether step name finished in this run.
func (r *Record) Done(name string) bool {
	for _, s := range r.Steps {
		if s.Name == name && (s.Status == "done" || s.Status == "skipped") {
			return true
		}
	}
	return false
}

// Set records step s, replacing an earlier outcome of the same step.
func (r *Record) Set(s Step) {
	for i := range r.Steps {
		if r.Steps[i].Name == s.Name {
			r.Steps[i] = s
			return
		}
	}
	r.Steps = append(r.Steps, s)
}

// AddRuns appends config-editor runs not recorded yet.
func (u *Undo) AddRuns(runs ...string) {
	for _, r := range runs {
		seen := false
		for _, have := range u.ConfigRuns {
			seen = seen || have == r
		}
		if !seen && r != "" {
			u.ConfigRuns = append(u.ConfigRuns, r)
		}
	}
}
