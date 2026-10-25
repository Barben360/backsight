// Package paths resolves backsight's directories per OS (SPEC §3).
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const app = "backsight"

// Dirs are backsight's directories: versioned binaries and generated files
// (Data), config.toml and secrets.toml (Config), runtime state (State) and
// the directory holding the user-facing shim (Bin).
type Dirs struct {
	Data   string `json:"data"`
	Config string `json:"config"`
	State  string `json:"state"`
	Bin    string `json:"bin"`
}

func (d Dirs) ConfigFile() string  { return filepath.Join(d.Config, "config.toml") }
func (d Dirs) SecretsFile() string { return filepath.Join(d.Config, "secrets.toml") }

// State layout, SPEC Appendix A.3.
func (d Dirs) StateFile() string       { return filepath.Join(d.State, "state.json") }
func (d Dirs) LocksDir() string        { return filepath.Join(d.State, "locks") }
func (d Dirs) MaintenanceLock() string { return filepath.Join(d.State, "maintenance.lock") }
func (d Dirs) CursorsDir() string      { return filepath.Join(d.State, "cursors") }
func (d Dirs) SessionsDir() string     { return filepath.Join(d.State, "sessions") }
func (d Dirs) SentIndexDir() string    { return filepath.Join(d.State, "sent-index") }
func (d Dirs) OutboxDir() string       { return filepath.Join(d.State, "outbox") }
func (d Dirs) QuarantineDir() string   { return filepath.Join(d.State, "quarantine") }
func (d Dirs) JournalFile() string     { return filepath.Join(d.State, "journal.jsonl") }
func (d Dirs) PreImagesDir() string    { return filepath.Join(d.State, "pre-images") }
func (d Dirs) TelemetryDir() string    { return filepath.Join(d.State, "telemetry") }
func (d Dirs) EntitiesFile() string    { return filepath.Join(d.State, "entities.json") }
func (d Dirs) SetupFile() string       { return filepath.Join(d.State, "setup.json") }

// Default resolves the directories of the running process.
func Default() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	return Resolve(runtime.GOOS, home, os.Getenv)
}

// Resolve computes the directories for goos. BACKSIGHT_HOME, when set,
// puts everything under one absolute directory (tests, portable installs).
func Resolve(goos, home string, getenv func(string) string) (Dirs, error) {
	if h := getenv("BACKSIGHT_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return Dirs{}, fmt.Errorf("BACKSIGHT_HOME must be absolute, got %q", h)
		}
		return Dirs{filepath.Join(h, "data"), filepath.Join(h, "config"), filepath.Join(h, "state"), filepath.Join(h, "bin")}, nil
	}
	if goos == "windows" {
		base := getenv("LOCALAPPDATA")
		if base == "" {
			return Dirs{}, errors.New("LOCALAPPDATA is not set")
		}
		root := filepath.Join(base, app)
		return Dirs{root, filepath.Join(root, "config"), filepath.Join(root, "state"), filepath.Join(base, "Programs", app)}, nil
	}
	if home == "" || !filepath.IsAbs(home) {
		return Dirs{}, fmt.Errorf("home directory must be absolute, got %q", home)
	}
	bin := filepath.Join(home, ".local", "bin")
	if goos == "darwin" {
		root := filepath.Join(home, "Library", "Application Support", app)
		return Dirs{root, filepath.Join(root, "config"), filepath.Join(root, "state"), bin}, nil
	}
	xdg := func(key string, fallback ...string) string {
		if v := getenv(key); filepath.IsAbs(v) {
			return filepath.Join(v, app)
		}
		return filepath.Join(append([]string{home}, append(fallback, app)...)...)
	}
	return Dirs{
		Data:   xdg("XDG_DATA_HOME", ".local", "share"),
		Config: xdg("XDG_CONFIG_HOME", ".config"),
		State:  xdg("XDG_STATE_HOME", ".local", "state"),
		Bin:    bin,
	}, nil
}
