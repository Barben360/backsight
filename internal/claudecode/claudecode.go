// Package claudecode locates Claude Code's own files and reads the few
// settings backsight depends on. It never writes (decision 0042 puts every
// edit in the config editor).
package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultCleanupDays is Claude Code's transcript retention when nothing
// sets cleanupPeriodDays.
const DefaultCleanupDays = 30

// Location of Claude Code's user-level files.
type Location struct {
	Dir      string `json:"dir"`
	Settings string `json:"settings"`
	State    string `json:"state"` // .claude.json
	ClaudeMD string `json:"claude_md"`
	Projects string `json:"projects"`
}

// Locate follows Claude Code: $CLAUDE_CONFIG_DIR replaces ~/.claude and
// then also holds .claude.json (verified on 2.1.310).
func Locate(home string, getenv func(string) string) Location {
	dir, state := filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		dir, state = d, filepath.Join(d, ".claude.json")
	}
	return Location{Dir: dir, Settings: filepath.Join(dir, "settings.json"), State: state,
		ClaudeMD: filepath.Join(dir, "CLAUDE.md"), Projects: filepath.Join(dir, "projects")}
}

// ManagedDir is where administrators put managed-settings.json.
func ManagedDir(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	}
	return "/etc/claude-code"
}

// Retention is the effective cleanupPeriodDays and where it comes from.
type Retention struct {
	EffectiveDays int    `json:"effective_days"`
	Source        string `json:"source"` // default | user | managed
	UserDays      *int   `json:"user_days"`
	ManagedDays   *int   `json:"managed_days"`
}

// ReadRetention reads user settings and managed settings (the file, then
// managed-settings.d/*.json in name order, the last value winning).
func ReadRetention(loc Location, managedDir string) (Retention, error) {
	r := Retention{EffectiveDays: DefaultCleanupDays, Source: "default"}
	var err error
	if r.UserDays, err = cleanupDays(loc.Settings); err != nil {
		return r, err
	}
	managed := []string{filepath.Join(managedDir, "managed-settings.json")}
	dropIns, _ := filepath.Glob(filepath.Join(managedDir, "managed-settings.d", "*.json"))
	sort.Strings(dropIns)
	for _, p := range append(managed, dropIns...) {
		d, err := cleanupDays(p)
		if err != nil {
			return r, err
		}
		if d != nil {
			r.ManagedDays = d
		}
	}
	switch {
	case r.ManagedDays != nil:
		r.EffectiveDays, r.Source = *r.ManagedDays, "managed"
	case r.UserDays != nil:
		r.EffectiveDays, r.Source = *r.UserDays, "user"
	}
	return r, nil
}

func cleanupDays(path string) (*int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s struct {
		CleanupPeriodDays *int `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s.CleanupPeriodDays, nil
}

// Transcripts summarises the session transcripts Claude Code keeps.
type Transcripts struct {
	Count  int       `json:"count"`
	Bytes  int64     `json:"bytes"`
	Oldest time.Time `json:"oldest"` // mtime, what cleanup goes by; zero when none
}

// ScanTranscripts counts projects/<project>/<session>.jsonl; subagent
// transcripts nested deeper belong to their session and are not counted.
func ScanTranscripts(projectsDir string) (Transcripts, error) {
	var t Transcripts
	files, err := filepath.Glob(filepath.Join(projectsDir, "*", "*.jsonl"))
	if err != nil {
		return t, err
	}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		t.Count++
		t.Bytes += fi.Size()
		if t.Oldest.IsZero() || fi.ModTime().Before(t.Oldest) {
			t.Oldest = fi.ModTime()
		}
	}
	return t, nil
}
