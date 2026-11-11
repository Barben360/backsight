package cli

import (
	"os"
	"path/filepath"

	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/project"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// importCodexHome is the Codex home `import --from codex` reads (decision
// 0085 D1, D10): --codex-home when given; else the home client install
// recorded for Codex on this PC (the adapter's RecordedHome); else, Codex
// not set up here, the one Codex itself would use (ShellHome: $CODEX_HOME
// when set and non-empty, else ~/.codex). It must be a folder.
func importCodexHome(home, flag string, st state.State, getenv func(string) string) (string, error) {
	var p string
	switch {
	case flag != "":
		p = expandHome(flag)
	case st.ClientOf(harnessid.Codex) != nil:
		r, err := codex.RecordedHome(st)
		if err != nil {
			return "", blocked(err)
		}
		if p = r; p == "" {
			p = filepath.Join(home, ".codex")
		}
	default:
		r, err := codex.ShellHome(home, "", getenv)
		if err != nil {
			return "", &output.Error{Code: output.Usage, Message: err.Error() + ": give the Codex home with --codex-home"}
		}
		p = r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", &output.Error{Code: output.Usage, Message: err.Error()}
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", &output.Error{Code: output.Usage, Message: "no Codex home at " + abs + ": give it with --codex-home"}
	}
	return abs, nil
}

// scanCodexImport reads Codex's memories in codexHome for an import: each
// summary's project is its working folder's, as for a session, and a
// summary of a folder kept out of memory is not sent.
func scanCodexImport(home, codexHome string, transverse, excludePaths, exclude []string) (fileimport.Scan, error) {
	res := project.New(home, transverse)
	return fileimport.ScanCodex(codexHome, fileimport.CodexOptions{
		Exclude: exclude,
		Now:     now(),
		Project: func(cwd string) string { return retain.CanonicalProject(transverse, res.Project(cwd)) },
		OptedOut: func(cwd string) bool {
			out, _ := res.OptedOut(cwd, excludePaths)
			return out
		},
	})
}
