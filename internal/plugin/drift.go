package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/Barben360/backsight/internal/config"
)

// Installed is what client install recorded (state plugin_version,
// shim_target).
type Installed struct {
	Version    string
	ShimTarget string
}

// InstalledWiring is the wiring the installed plugin was generated with,
// as far as its version tells (off when it matches neither): for a caller
// that cannot read the config, so an unknown switch is no drift.
func InstalledWiring(data string, in Installed, binaryVersion string) Wiring {
	on := Wiring{FailurePitfalls: true}
	if Version(binaryVersion, filepath.Join(data, "bin", ShimName(runtime.GOOS)), on) == in.Version {
		return on
	}
	return Wiring{}
}

// Drift lists what no longer matches the installed wiring (SPEC §5): the
// plugin version for this binary and the config's wiring w, the permission
// rule, the shim target. Reads two small files and one link: cheap enough
// for SessionStart.
func Drift(data, settings string, in Installed, binaryVersion string, w Wiring) []string {
	if in.Version == "" {
		return nil // never installed: nothing to compare
	}
	var out []string
	shim := filepath.Join(data, "bin", ShimName(runtime.GOOS))
	flipped := w
	flipped.FailurePitfalls = !w.FailurePitfalls
	switch want := Version(binaryVersion, shim, w); {
	case want == in.Version:
	case Version(binaryVersion, shim, flipped) == in.Version && w.FailurePitfalls:
		out = append(out, config.FailurePitfallsKey+" is on but the plugin does not wire PostToolUseFailure")
	case Version(binaryVersion, shim, flipped) == in.Version:
		out = append(out, config.FailurePitfallsKey+" is off but the plugin still wires PostToolUseFailure")
	default:
		out = append(out, "plugin "+in.Version+" is not this binary's ("+want+")")
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	raw, _ := os.ReadFile(settings)
	_ = json.Unmarshal(raw, &s)
	for _, tool := range ToolNames {
		if !slices.Contains(s.Permissions.Allow, tool) {
			out = append(out, "permission rule for the memory tools missing in "+settings)
			break
		}
	}
	switch target, err := filepath.EvalSymlinks(shim); {
	case err != nil:
		out = append(out, "shim "+shim+" missing")
	case in.ShimTarget == "", runtime.GOOS == "windows":
		// Windows: the shim is a copy; hashing two binaries would blow the
		// SessionStart budget, presence is what is checked there.
	default:
		want, werr := filepath.EvalSymlinks(in.ShimTarget) // a data dir behind a symlink resolves the same
		if werr != nil || target != want {
			out = append(out, "shim points to "+target+", not "+in.ShimTarget)
		}
	}
	return out
}
