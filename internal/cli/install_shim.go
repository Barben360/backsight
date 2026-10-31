package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/plugin"
)

// installedShim is what __install-shim did.
type installedShim struct {
	Version string `json:"version"`
	Target  string `json:"target"`
	Shim    string `json:"shim"`
	// Command is empty when the user's own file is at <bin>/backsight.
	Command string `json:"command"`
	Bin     string `json:"bin"`
	// UserPathAdded: <bin> was added to the user PATH (Windows).
	UserPathAdded bool `json:"user_path_added"`
}

// releaseVersion is a version install.ps1 and install.sh accept: it names
// the directory <data>/bin/<version>.
var releaseVersion = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]*$`)

// newInstallShimCmd is install.ps1's last step (decision 0080): this
// binary, verified by the script and run from where it extracted it,
// installs itself as <data>/bin/<version>/ with the signed checksums.txt
// beside it, points the shim and the command on PATH at it, and puts
// <bin> on the user PATH. The directories are this binary's own (paths,
// BACKSIGHT_HOME included), and the swap is client install's, so the
// script re-implements neither.
func newInstallShimCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:    "__install-shim <version>",
		Short:  "Install this binary as <data>/bin/<version> and point the shim and the PATH command at it (install.ps1)",
		Args:   usageArgs(cobra.ExactArgs(1)),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if !releaseVersion.MatchString(args[0]) {
				return &output.Error{Code: output.Usage, Message: fmt.Sprintf("unexpected release version %q", args[0])}
			}
			// The directory names the version this binary is: a release
			// installs only as itself (a dev build has no version to check).
			if v := buildinfo.Version; v != "" && v != "dev" && v != args[0] {
				return &output.Error{Code: output.Usage, Message: fmt.Sprintf("this binary is backsight %s, not %s", v, args[0])}
			}
			d, _, err := machine()
			if err != nil {
				return err
			}
			name := "backsight"
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			r := installedShim{
				Version: args[0],
				Target:  filepath.Join(d.Data, "bin", args[0], name),
				Shim:    filepath.Join(d.Data, "bin", plugin.ShimName(runtime.GOOS)),
				Bin:     d.Bin,
			}
			if err := installShim(r.Target, r.Shim); err != nil {
				return blocked(err)
			}
			// The command on PATH runs the shim, as client install makes it.
			// Anything else there is the user's and stays (on Windows,
			// where any file there is backsight's copy, it is refused):
			// no command is reported then.
			link, older := olderPathLink(d, r.Shim)
			switch {
			case older || absentPathLink(d):
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					return blocked(err)
				}
				if err := pointShim(r.Shim, link); err != nil {
					return blocked(err)
				}
				r.Command = link
			case runtime.GOOS == "windows":
				return blocked(fmt.Errorf("%s is not a file backsight made: remove it, then install again", link))
			default:
				if dest, err := os.Readlink(link); err == nil && filepath.Clean(dest) == filepath.Clean(r.Shim) {
					r.Command = link // already the shim's link
				}
			}
			if r.UserPathAdded, err = ensureUserPath(d.Bin); err != nil {
				return blocked(fmt.Errorf("adding %s to the user PATH: %w", d.Bin, err))
			}
			return e.emit("__install-shim", r, func() string {
				return fmt.Sprintf("backsight %s installed: %s\n", r.Version, r.Target)
			})
		},
	}
}
