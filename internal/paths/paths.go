// Package paths resolves backsight's directories per OS (SPEC §3).
package paths

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
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
func (d Dirs) AttributionDir() string  { return filepath.Join(d.State, "attribution") }
func (d Dirs) FamiliesFile() string    { return filepath.Join(d.State, "families.json") }
func (d Dirs) EntitiesFile() string    { return filepath.Join(d.State, "entities.json") }
func (d Dirs) SetupFile() string       { return filepath.Join(d.State, "setup.json") }

// ConnectDir keeps, one directory per run, what connect moved aside when
// this PC joined another server (cursors, sent-index, the state it had).
func (d Dirs) ConnectDir() string { return filepath.Join(d.State, "connect") }

// FeedbackDir holds the feedback journal and the turn pointers.
func (d Dirs) FeedbackDir() string { return filepath.Join(d.State, "feedback") }

// IndexDir holds the local metrics index, derived from telemetry and the
// feedback journal. MetricsDir and DashboardDir are named here for the
// uninstall; their owners create them.
func (d Dirs) IndexDir() string     { return filepath.Join(d.State, "index") }
func (d Dirs) MetricsDir() string   { return filepath.Join(d.State, "metrics") }
func (d Dirs) DashboardDir() string { return filepath.Join(d.State, "dashboard") }

// BankTemplateFile records the bank template last applied, so doctor can
// tell an older backsight template from a change made on the server.
func (d Dirs) BankTemplateFile() string { return filepath.Join(d.State, "bank-template.json") }

// RestoreMarker exists while a restore has not finished (decision 0044).
func (d Dirs) RestoreMarker() string { return filepath.Join(d.State, "restore-incomplete.json") }

// UpdateMarker exists while an update apply has not finished or rolled
// back (SPEC §11.7).
func (d Dirs) UpdateMarker() string { return filepath.Join(d.State, "update-incomplete.json") }

// UpdateSnapshot holds state.json and config.toml as they were before the
// update the marker names, for its rollback or its resume.
func (d Dirs) UpdateSnapshot() string { return filepath.Join(d.State, "update-snapshot.json") }

// Default resolves the directories of the running process.
func Default() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	return Resolve(runtime.GOOS, home, os.Getenv)
}

// Resolve computes the directories for goos, with goos's path rules (what
// is absolute, which separator joins), not the host's.
// BACKSIGHT_HOME, when set, puts everything under one absolute directory
// (tests, portable installs).
func Resolve(goos, home string, getenv func(string) string) (Dirs, error) {
	r := rulesFor(goos)
	d, err := r.resolve(goos, home, getenv)
	// BACKSIGHT_STATE_DIR moves the runtime state alone: verify runs the
	// hooks and the MCP server against a scratch state (decision 0064).
	if s := getenv("BACKSIGHT_STATE_DIR"); err == nil && r.isAbs(s) {
		d.State = s
	}
	return d, err
}

// rules are one OS's path syntax. The host's are filepath's own, so the
// running OS resolves exactly as filepath does.
type rules struct {
	isAbs func(string) bool
	join  func(...string) string
}

func rulesFor(goos string) rules {
	if goos == runtime.GOOS {
		return rules{filepath.IsAbs, filepath.Join}
	}
	if goos == "windows" {
		return rules{windowsIsAbs, windowsJoin}
	}
	return rules{func(p string) bool { return strings.HasPrefix(p, "/") }, path.Join}
}

// windowsIsAbs: a drive letter and a separator (C:\x, C:/x) or a UNC path.
func windowsIsAbs(p string) bool {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		('a' <= p[0]|0x20 && p[0]|0x20 <= 'z')
}

func windowsJoin(elem ...string) string {
	unc := len(elem) > 0 && (strings.HasPrefix(elem[0], `\\`) || strings.HasPrefix(elem[0], "//"))
	for i, e := range elem {
		elem[i] = strings.ReplaceAll(e, `\`, "/")
	}
	j := strings.ReplaceAll(path.Join(elem...), "/", `\`)
	if unc {
		j = `\` + j
	}
	return j
}

func (r rules) resolve(goos, home string, getenv func(string) string) (Dirs, error) {
	join := r.join
	if h := getenv("BACKSIGHT_HOME"); h != "" {
		if !r.isAbs(h) {
			return Dirs{}, fmt.Errorf("BACKSIGHT_HOME must be absolute, got %q", h)
		}
		return Dirs{join(h, "data"), join(h, "config"), join(h, "state"), join(h, "bin")}, nil
	}
	if goos == "windows" {
		base := getenv("LOCALAPPDATA")
		if base == "" {
			return Dirs{}, errors.New("LOCALAPPDATA is not set")
		}
		root := join(base, app)
		return Dirs{root, join(root, "config"), join(root, "state"), join(base, "Programs", app)}, nil
	}
	if home == "" || !r.isAbs(home) {
		return Dirs{}, fmt.Errorf("home directory must be absolute, got %q", home)
	}
	bin := join(home, ".local", "bin")
	if goos == "darwin" {
		root := join(home, "Library", "Application Support", app)
		return Dirs{root, join(root, "config"), join(root, "state"), bin}, nil
	}
	xdg := func(key string, fallback ...string) string {
		if v := getenv(key); r.isAbs(v) {
			return join(v, app)
		}
		return join(append([]string{home}, append(fallback, app)...)...)
	}
	return Dirs{
		Data:   xdg("XDG_DATA_HOME", ".local", "share"),
		Config: xdg("XDG_CONFIG_HOME", ".config"),
		State:  xdg("XDG_STATE_HOME", ".local", "state"),
		Bin:    bin,
	}, nil
}
