package harness

import (
	"context"
	"os"
	"testing"

	"github.com/Barben360/backsight/internal/state"
)

// codex_install.go's ledger: block 3, https://github.com/Barben360/backsight/issues/278.
func init() {
	codexOwns("codex_install.go", codexPart{
		done: []string{"MinVersion", "ProfileVar"},
		stubs: map[string]func() bool{
			// A real implementation: "" is ~/.codex, the default.
			"IsDefaultProfile": func() bool { return !(Codex{}).IsDefaultProfile("/home/u", "") },
			"Locate":           func() bool { return (Codex{}).Locate(Env{Home: "/home/u"}) == Location{} },
			"Detect":           func() bool { _, err := (Codex{}).Detect(context.Background(), Env{}); return notImplemented(err) },
			"Install": func() bool {
				r, err := (Codex{}).Install(context.Background(), Env{}, Integration{}, true)
				return notImplemented(err) && len(r.Steps) == 0
			},
			"Uninstall": func() bool { return notImplemented((Codex{}).Uninstall(context.Background(), Env{})) },
			"Installed": func() bool { _, err := (Codex{}).Installed(context.Background(), Env{}); return notImplemented(err) },
			// A real implementation reports the missing shim of an install
			// that recorded one.
			"Drift": func() bool {
				data, _ := os.MkdirTemp("", "codex-drift-")
				defer func() { _ = os.RemoveAll(data) }()
				rec := Recorded{Version: "1.0.0+abc", ShimTarget: "/nonexistent/backsight", Data: data, BinaryVersion: "1.0.0"}
				return (Codex{}).Drift(Env{Home: data}, rec, nil) == nil
			},
			"HookCommand": func() bool {
				_, err := (Codex{}).HookCommand("/shim", EventStop, false)
				return notImplemented(err)
			},
			"ShellHome": func() bool {
				_, err := (Codex{}).ShellHome("/home/u", "", func(string) string { return "" })
				return notImplemented(err)
			},
			"RecordHome":   func() bool { return notImplemented((Codex{}).RecordHome(&state.State{}, "/x")) },
			"RecordedHome": func() bool { _, err := (Codex{}).RecordedHome(state.State{}); return notImplemented(err) },
		},
	})
}

func TestCodexMinVersionAndProfileVar(t *testing.T) {
	c := Codex{}
	if c.MinVersion() != "0.167.0" {
		t.Error(c.MinVersion())
	}
	// The home is recorded, never read from a shell: no variable for the
	// shared lookup to read.
	if c.ProfileVar() != "" {
		t.Errorf("ProfileVar %q", c.ProfileVar())
	}
}
