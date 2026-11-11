package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harness"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
)

// A shell holding both Claude Code's and Codex's variables runs in the
// tool started last (block 7, 0085 D4): a Codex started from Claude Code's
// shell records the Claude Code session its hooks inherited, and its shell
// then takes the Codex session; anything else is shellSession's choice.
func TestShellSessionInANestedShell(t *testing.T) {
	dir := t.TempDir()
	store := session.Store{Dir: dir, Locks: t.TempDir()}
	if err := store.Update(cxRoot, func(st *session.State) error { st.StartedIn = "s-claude"; return nil }); err != nil {
		t.Fatal(err)
	}
	const other = "01a1c72c-0000-7000-8000-0000000000ff" // a Codex session started from no Claude Code
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"Codex started from Claude Code", map[string]string{"BACKSIGHT_SESSION": "s-claude", "CLAUDE_CODE_SESSION_ID": "s-claude", "CODEX_SESSION_ID": cxRoot}, cxRoot},
		{"the same, Claude Code's env file not sourced", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-claude", "CODEX_SESSION_ID": cxRoot}, cxRoot},
		{"Claude Code started from that Codex", map[string]string{"BACKSIGHT_SESSION": "s-inner", "CLAUDE_CODE_SESSION_ID": "s-inner", "CODEX_SESSION_ID": cxRoot}, "s-inner"},
		{"Claude Code started from a plain Codex", map[string]string{"BACKSIGHT_SESSION": "s-inner", "CLAUDE_CODE_SESSION_ID": "s-inner", "CODEX_SESSION_ID": other}, "s-inner"},
		{"the same, Claude Code not wired", map[string]string{"CLAUDE_CODE_SESSION_ID": "s-inner", "CODEX_SESSION_ID": other}, ""},
		{"Codex alone", map[string]string{"CODEX_SESSION_ID": other}, other},
		{"Claude Code alone", map[string]string{"BACKSIGHT_SESSION": "s-claude"}, "s-claude"},
	}
	for _, c := range cases {
		if got := shellSessionIn(dir, env(c.env)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// No sessions directory: nothing is read (never a file of the working
	// directory).
	if got := shellSessionIn("", env(cases[0].env)); got != "s-claude" {
		t.Errorf("no sessions directory: %q", got)
	}
}

// End to end: Codex's SessionStart, its hooks' environment Claude Code's,
// records where it was started; pause and store run from its shell then
// take the Codex session, never Claude Code's; Claude Code's own
// SessionStart records nothing; the Codex session resumed from a plain
// terminal forgets where it was first started.
func TestCodexStartedFromClaudeCodePausesItsOwnSession(t *testing.T) {
	home := hookHome(t)
	t.Setenv("BACKSIGHT_SESSION", "s-claude")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-claude")
	t.Setenv("CODEX_SESSION_ID", "")
	resume := cxPayload(t, home, "SessionStart.resume", nil) // read before the test leaves the package's directory
	cxHook(t, harness.EventSessionStart, cxPayload(t, home, "SessionStart.startup", nil))
	if st, _ := sessions(home).Load(cxRoot); st.StartedIn != "s-claude" {
		t.Fatalf("Codex's session started in %q", st.StartedIn)
	}
	t.Setenv("CODEX_SESSION_ID", cxRoot) // the shell Codex runs
	t.Chdir(filepath.Join(home, "billing"))
	if code, _, out := storeJSON(t, "--kind", "decision", "Billing keeps PostgreSQL 16."); code != 0 {
		t.Fatal(out)
	}
	if got := lastNoteSession(t); got != cxRoot {
		t.Errorf("note names session %q", got)
	}
	if code, out, errOut := run(t, "pause"); code != output.OK {
		t.Fatalf("pause %d %s %s", code, out, errOut)
	}
	cx, _ := session.Paused(pathsOf(home).SessionsDir(), cxRoot)
	cl, _ := session.Paused(pathsOf(home).SessionsDir(), "s-claude")
	if !cx || cl {
		t.Errorf("paused: Codex's %v, Claude Code's %v", cx, cl)
	}

	t.Setenv("BACKSIGHT_SESSION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	fire(t, "session-start", map[string]any{"session_id": "s-claude2", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if st, _ := sessions(home).Load("s-claude2"); st.StartedIn != "" {
		t.Errorf("Claude Code's session recorded %q", st.StartedIn)
	}
	// The Codex session resumed from a plain terminal forgets where it
	// was first started.
	cxHook(t, harness.EventSessionStart, resume)
	if st, _ := sessions(home).Load(cxRoot); st.StartedIn != "" {
		t.Errorf("resumed elsewhere, still started in %q", st.StartedIn)
	}
}

// Claude Code started from a Codex shell, backsight not wired in Claude
// Code: pause names the Codex session it must be told to take.
func TestPauseSaysWhatToDoInClaudeCodeStartedFromCodex(t *testing.T) {
	home := hookHome(t)
	t.Chdir(filepath.Join(home, "billing"))
	t.Setenv("BACKSIGHT_SESSION", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-claude")
	t.Setenv("CODEX_SESSION_ID", cxRoot)
	code, out, errOut := run(t, "pause")
	if code != output.Usage || !strings.Contains(out+errOut, "give --session "+cxRoot) {
		t.Errorf("exit %d %s %s", code, out, errOut)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	if code, out, errOut := run(t, "pause"); code != output.Usage || !strings.Contains(out+errOut, "Claude Code or Codex session") {
		t.Errorf("no session at all: exit %d %s %s", code, out, errOut)
	}
}
