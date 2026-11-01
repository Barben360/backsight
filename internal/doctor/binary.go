package doctor

import (
	"fmt"
	"strings"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/state"
)

// reinstall brings a release binary back with its signed checksums.txt.
const reinstall = "reinstall this release with install.sh (it verifies the download and keeps checksums.txt next to the binary), then run backsight client install"

// checkBinary reports the binary's check of itself (SPEC §11.1, decision
// 0067). A mismatch is an error, yet hooks keep running: they never read it.
func checkBinary(rec state.SelfCheck) Check {
	c := Check{ID: "binary", Detail: rec.Detail}
	switch rec.Status {
	case "verified":
		c.Status, c.Summary, c.Detail = OK, "backsight "+rec.Version+" matches its signed release (key "+rec.KeyID+")", ""
	case "unsigned":
		c.Status = Info
		if !buildinfo.Release() {
			c.Summary = "unsigned dev build"
			return c
		}
		c.Summary, c.Fix = "backsight "+rec.Version+" is not self-verified: no signed checksums.txt next to it", reinstall
	default:
		c.Status, c.Summary, c.Fix = Error, "backsight "+rec.Version+" does not match its signed release", reinstall
	}
	return c
}

// checkShimCopies counts the shim's earlier copies (Windows, decision
// 0080): a swap renames a running current.exe to current.exe.old-<n>,
// and the next swap or install deletes those nobody runs any more. Those
// left are held by programs still running them (Claude Code sessions, a
// supervisor not restarted): information, never a fault.
func checkShimCopies(copies []string) Check {
	c := Check{ID: "shim-copies", Status: OK, Summary: "no earlier copy of the shim is left"}
	if len(copies) > 0 {
		c.Status = Info
		what := fmt.Sprintf("%d earlier copies of the shim still run", len(copies))
		if len(copies) == 1 {
			what = "1 earlier copy of the shim still runs"
		}
		c.Summary = what + " (Claude Code sessions or the supervisor started before an update): the next update or client install deletes those no longer running"
		c.Detail = strings.Join(copies, ", ")
	}
	return c
}
