package harness

import (
	"context"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/state"
)

// Codex's home and profile, detection and version, install, uninstall and
// drift: block 3 (https://github.com/Barben360/backsight/issues/278),
// 0085 D1 (home), D2 (the two plugins, their install commands), D5 (hook
// trust), D8 (the TOML editor), D9 (minimum version). Every method here
// is a stub until then.

// CodexMinVersion is the oldest Codex every fact of 0085 was observed on
// (D9); setup and doctor refuse an older one.
const CodexMinVersion = "0.167.0"

func (Codex) MinVersion() string { return CodexMinVersion }

// ProfileVar is "": the Codex home is resolved once (ShellHome) and
// recorded, never read from a shell afterwards, so the shared
// wiredProfile must find no variable to read (0085 D1).
func (Codex) ProfileVar() string { return "" }

// IsDefaultProfile reports that profile is ~/.codex. Block 3.
func (Codex) IsDefaultProfile(home, profile string) bool { return false }

// Locate is where Codex keeps its files for env's profile (config.toml,
// sessions/). Block 3; the zero Location until then.
func (Codex) Locate(env Env) Location { return Location{} }

// Detect is `codex --version` (codex-cli <x.y.z>). Block 3.
func (Codex) Detect(ctx context.Context, env Env) (string, error) {
	return "", notYet("detection")
}

// Install generates the two plugins in <data>/codex-marketplace/ and adds
// them with Codex's own commands (0085 D2). Block 3.
func (Codex) Install(ctx context.Context, env Env, in Integration, plan bool) (InstallResult, error) {
	return InstallResult{}, notYet("install")
}

// Uninstall removes both plugins, their [hooks.state] tables and the
// empty plugin cache (0085 D2, D8). Block 3.
func (Codex) Uninstall(ctx context.Context, env Env) error { return notYet("uninstall") }

// Installed reports whether either plugin or the marketplace is
// registered (`codex plugin list --json`). Block 3.
func (Codex) Installed(ctx context.Context, env Env) (bool, error) {
	return false, notYet("install check")
}

// Drift lists what no longer matches the install, hook trust included
// (0085 D5). Block 3; until then it reports nothing, and nothing asks it
// (Codex is off).
func (Codex) Drift(env Env, rec Recorded, cfg *config.Config) []string { return nil }

// HookCommand is the hook handler's command string for backsight's event:
// the shim quoted for sh ("command") or, windows, for cmd /C
// ("commandWindows"), then HookArgs(event) (codex.go's contract). Block 3.
func (Codex) HookCommand(shim, event string, windows bool) (string, error) {
	return "", notYet("hook command")
}

// ShellHome is the Codex home setup and client install resolve, once:
// flag when set (--codex-home), else $CODEX_HOME when set and non-empty,
// else ~/.codex (0085 D1). Block 3.
func (Codex) ShellHome(home, flag string, getenv func(string) string) (string, error) {
	return "", notYet("home")
}

// RecordHome records codexHome in s's Codex entry,
// s.ClientFor(harnessid.Codex).Profile, "" for ~/.codex; client install
// is its only caller. Block 3.
func (Codex) RecordHome(s *state.State, codexHome string) error { return notYet("home") }

// RecordedHome is the Codex home client install recorded,
// st.ProfileOf(harnessid.Codex) ("" for ~/.codex): what every command
// after install acts on. Block 3.
func (Codex) RecordedHome(st state.State) (string, error) { return "", notYet("home") }
