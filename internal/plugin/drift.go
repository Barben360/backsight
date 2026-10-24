package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// Installed is what client install recorded (state plugin_version,
// shim_target).
type Installed struct {
	Version    string
	ShimTarget string
}

// Drift lists what no longer matches the installed wiring (SPEC §5): the
// plugin version for this binary, the permission rule, the shim target.
// Reads two small files and one link: cheap enough for SessionStart.
func Drift(data, settings string, in Installed, binaryVersion string) []string {
	if in.Version == "" {
		return nil // never installed: nothing to compare
	}
	var out []string
	shim := filepath.Join(data, "bin", ShimName(runtime.GOOS))
	if Version(binaryVersion, shim) != in.Version {
		out = append(out, "plugin "+in.Version+" is not this binary's ("+Version(binaryVersion, shim)+")")
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
