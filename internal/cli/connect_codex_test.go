package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
	"github.com/Barben360/backsight/internal/state"
)

// A PC may run Claude Code, Codex or both (issue 284): the restart a
// session open across connect needs names the tools wired here, and
// leaves out one setup already asked to restart.
func TestOpenSessionStepsNameTheToolsWired(t *testing.T) {
	home := isolate(t)
	d := pathsOf(home)
	codexDir := filepath.Join(home, ".codex")
	wire := func(claude, codexOn bool) {
		t.Helper()
		if err := state.Update(d, "test", func(s *state.State) error {
			s.PluginVersion = ""
			if claude {
				s.PluginVersion = "1.0.0"
			}
			c := s.ClientFor(harnessid.Codex)
			c.PluginVersion, c.Profile = "", codexDir
			if codexOn {
				c.PluginVersion = "1.0.0+abc"
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name          string
		claude, codex bool
		setupSaid     []string
		want, notWant []string // in the one step; nil want: no step
	}{
		{"Claude Code alone", true, false, nil, []string{"restart Claude Code (or run /reload-plugins):"}, []string{"Codex"}},
		{"Codex alone", false, true, nil, []string{"restart Codex:"}, []string{"Claude", "/reload-plugins"}},
		{"both", true, true, nil, []string{"restart Claude Code (or run /reload-plugins) and Codex:"}, nil},
		{"none recorded: Claude Code, as before", false, false, nil, []string{"restart Claude Code"}, []string{"Codex"}},
		{"both, setup restarts Claude Code", true, true, []string{claudeRestartStep}, []string{"restart Codex:"}, []string{"Claude"}},
		{"both, setup restarts Codex", true, true, []string{codexRestartStep}, []string{"restart Claude Code (or run /reload-plugins):"}, []string{"Codex"}},
		{"Codex alone, setup restarts it", false, true, []string{codexRestartStep}, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire(c.claude, c.codex)
			got := openSessionSteps(d, home, c.setupSaid)
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("steps %q, want none", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], "memory tools keep the old server") || !strings.Contains(got[0], "sent whole") {
				t.Fatalf("steps %q, want one open-session step", got)
			}
			for _, w := range c.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("step %q lacks %q", got[0], w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got[0], w) {
					t.Errorf("step %q names %q", got[0], w)
				}
			}
		})
	}
}

// connect --undo on a PC that runs Codex alone asks to restart Codex,
// not Claude Code.
func TestConnectUndoOnACodexPCNamesCodex(t *testing.T) {
	home, _ := setupMachine(t)
	connectedFromOwnServer(t, home)
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.PluginVersion = "" // Claude Code never wired here
		c := s.ClientFor(harnessid.Codex)
		c.PluginVersion, c.Profile = "1.0.0+abc", filepath.Join(home, ".codex")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "connect", "--undo", "--json")
	env := parseConnect(t, out)
	if code != output.OK || !env.Data.RestartRequired {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	next := strings.Join(env.Data.NextSteps, "\n")
	if !strings.Contains(next, "restart Codex:") || strings.Contains(next, "Claude Code") {
		t.Errorf("next steps %q, want Codex's restart alone", env.Data.NextSteps)
	}
}

// A PC with Codex alone joins: connect hands tools=codex and --codex-home
// to setup, which wires Codex in that home and not Claude Code; nothing
// asks to restart Claude Code.
func TestConnectFromACodexPC(t *testing.T) {
	home, steps := setupMachine(t)
	steps.real["client install"] = true
	f := withCodex(t, home)
	dir := filepath.Join(home, "elsewhere", "codex")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("model = \"gpt-6.1-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := joinableHost(t)
	code, out, errOut := run(t, "connect", "--token-file", tokenFile(t, tok), "--answer", "consent=yes", "--answer", "tools=codex", "--answer", "backfill=no",
		"--codex-home", dir, "--non-interactive", "--json")
	env := parseConnect(t, out)
	if code != output.OK || env.Data.Setup == nil {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	by := stepsByName(env.Data.Setup.Steps)
	if by["codex"].Status != "done" || by["client"].Status != "skipped" {
		t.Errorf("setup steps: codex %+v, client %+v", by["codex"], by["client"])
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || !slices.Equal(rec.Undo.CodexHomes, []string{dir}) {
		t.Errorf("setup record %+v, want Codex wired in %s", rec, dir)
	}
	if !slices.ContainsFunc(f.calls, func(c codexCall) bool { return strings.HasPrefix(c.line, "plugin add") && c.home == dir }) {
		t.Errorf("codex calls %+v, want a plugin added in %s", f.calls, dir)
	}
	if next := strings.Join(append(env.Data.NextSteps, env.Data.Setup.NextSteps...), "\n"); strings.Contains(next, "Claude Code") || !strings.Contains(next, "restart Codex") {
		t.Errorf("next steps %q", next)
	}
}

// server install --ssh from a PC with Codex alone: this PC's tools and
// Codex answers and --codex-home reach connect's setup.
func TestSSHInstallFromACodexPC(t *testing.T) {
	home, src, _ := sshMachine(t, "100.101.1.2")
	old := subcommand
	subcommand = func(ctx context.Context, args ...string) subResult {
		if len(args) > 1 && args[0] == "client" && args[1] == "install" {
			return runSubcommand(ctx, args...)
		}
		return old(ctx, args...)
	}
	t.Cleanup(func() { subcommand = old })
	f := withCodex(t, home)
	f.memories = true
	dir := filepath.Join(home, "elsewhere", "codex")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[features]\nmemories = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, sshInstallArgs(src, "--answer", "tools=codex", "--answer", "codex_memories=keep", "--answer", "codex_import=no",
		"--answer", "backfill=no", "--codex-home", dir, "--json")...)
	env := parseSSHInstall(t, out)
	if code != output.OK || env.Data.Connect == nil || env.Data.Connect.Setup == nil {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	by := stepsByName(env.Data.Connect.Setup.Steps)
	if by["codex"].Status != "done" || by["client"].Status != "skipped" || by["codex_memories"].Detail != "kept on, as answered" {
		t.Errorf("setup steps: codex %+v, client %+v, codex_memories %+v", by["codex"], by["client"], by["codex_memories"])
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec == nil || !slices.Equal(rec.Undo.CodexHomes, []string{dir}) {
		t.Errorf("setup record %+v, want Codex wired in %s", rec, dir)
	}
}

// A host already set up to use the memory is refused whatever tool it
// runs: the message no longer assumes Claude Code.
func TestSSHInstallRefusesAPCWhateverItsTool(t *testing.T) {
	_, src, h := sshMachine(t, "100.101.1.2")
	h.version, h.role, h.mode, h.config = "9.9.9", "both", "local", true
	code, out, errOut := run(t, sshInstallArgs(src, "--json")...)
	if env := parseSSHInstall(t, out); code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "Claude Code, Codex or both") {
		t.Errorf("exit %d, want 20 naming either tool\n%s%s", code, out, errOut)
	}
}

// --codex-home goes with --ssh: alone, or with --print-commands (which
// runs nothing here), it is refused; this PC's Codex answers go on the
// printed connect line.
func TestCodexHomeGoesWithSSH(t *testing.T) {
	isolate(t)
	if code, _, errOut := run(t, "server", "install", "--codex-home", "/c"); code != output.Usage || !strings.Contains(errOut, "--ssh") {
		t.Errorf("--codex-home alone: exit %d: %s", code, errOut)
	}
	setBuild(t, "9.9.9", "abc1234")
	if code, out, errOut := run(t, "server", "install", "--print-commands", "--codex-home", "/c"); code != output.Usage || !strings.Contains(errOut, "go with --ssh") || strings.Contains(out, "backsight setup") {
		t.Errorf("--print-commands --codex-home: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut := run(t, "server", "install", "--print-commands", "--private", "100.101.1.2", "--answer", "tools=codex", "--answer", "codex_memories=off")
	if code != output.OK || !strings.Contains(out, "\nbacksight connect --token-file backsight-token --answer tools=codex --answer codex_memories=off\n") {
		t.Errorf("exit %d, want the Codex answers on the connect line\n%s%s", code, out, errOut)
	}
}
