package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/testtool"
)

const npmError = "Exit code 1\nError: Cannot find module 'express'\nRequire stack:\n- /home/u/app/server.js"

func failure(t *testing.T, home, sid, tool, errText string, extra map[string]any) hookOut {
	t.Helper()
	in := map[string]any{"session_id": sid, "cwd": filepath.Join(home, "billing"), "tool_name": tool,
		"tool_input": map[string]any{"command": "npm test"}, "error": errText, "is_interrupt": false}
	for k, v := range extra {
		in[k] = v
	}
	return fire(t, "post-tool-use-failure", in)
}

func failureOn(t *testing.T, home string) {
	t.Helper()
	appendConfig(t, home, "[recall]\nfailure_pitfalls = true")
}

func TestFailureRecallIsOffByDefault(t *testing.T) {
	home := hookHome(t)
	recallAnswer = hits(0.95)
	if h := failure(t, home, "s1", "Bash", npmError, nil); h.Specific.Context != "" || len(recallQueries) != 0 {
		t.Fatalf("off by default, yet it searched %v and injected %q", recallQueries, h.Specific.Context)
	}
}

func TestFailureRecallInjectsPitfallsAtTheFloorOnce(t *testing.T) {
	home := hookHome(t)
	failureOn(t, home)
	recallAnswer = hits(0.95, 0.85, 0.82, 0.79) // only 2 are kept; m4 is under the floor
	h := failure(t, home, "s1", "Bash", npmError, nil)
	c := h.Specific.Context
	if h.Specific.Event != "PostToolUseFailure" || !strings.Contains(c, "Memories about this failure "+instructions.HintsLine) ||
		!strings.Contains(c, "memory m1") || !strings.Contains(c, "memory m2") || strings.Contains(c, "memory m3") || strings.Contains(c, "memory m4") {
		t.Fatalf("context:\n%s", c)
	}
	if len(recallQueries) == 0 || !strings.Contains(recallQueries[0], "npm test") || !strings.Contains(recallQueries[0], "Cannot find module") {
		t.Errorf("query %v lacks the command or the error", recallQueries)
	}
	st, _ := sessions(home).Load("s1")
	if len(st.FailureSigs) != 1 || st.CharsInjected == 0 || len(st.Injected) != 2 || st.Injected[0].Rule != "failure" {
		t.Errorf("state %+v", st)
	}
	// The same failure with other numbers, paths and case: not looked up again.
	recallQueries = nil
	again := "Exit code 127\nerror: cannot find module 'express'\nRequire stack:\n- /tmp/other/42/server.js"
	if h := failure(t, home, "s1", "Bash", again, nil); h.Specific.Context != "" || len(recallQueries) != 0 {
		t.Errorf("a repeated failure was looked up again: %q %v", h.Specific.Context, recallQueries)
	}
	// A different error in the same session is looked up (m1, m2 were seen: m3 is next).
	recallAnswer = hits(0.95, 0.9, 0.85)
	h = failure(t, home, "s1", "Bash", "Exit code 2\nfatal: not a git repository (or any parent up to mount point /)", nil)
	if len(recallQueries) == 0 || !strings.Contains(h.Specific.Context, "memory m3") || strings.Contains(h.Specific.Context, "memory m1") {
		t.Errorf("a different failure: queries %v, context:\n%s", recallQueries, h.Specific.Context)
	}
	// Another session starts over.
	recallAnswer = hits(0.95, 0.9)
	if h := failure(t, home, "s2", "Bash", npmError, nil); !strings.Contains(h.Specific.Context, "memory m1") {
		t.Errorf("a new session got %q", h.Specific.Context)
	}
}

func TestFailureRecallNeverInjectsBelowTheFloor(t *testing.T) {
	home := hookHome(t)
	failureOn(t, home)
	recallAnswer = hits(0.79, 0.6)
	if h := failure(t, home, "s1", "Bash", npmError, nil); h.Specific.Context != "" {
		t.Fatalf("injected under 0.8: %q", h.Specific.Context)
	}
	// The lookup is recorded all the same: a repeat does not search again.
	recallQueries = nil
	failure(t, home, "s1", "Bash", npmError, nil)
	if len(recallQueries) != 0 {
		t.Error("an empty lookup was repeated")
	}
}

func TestFailureRecallSkips(t *testing.T) {
	cases := map[string]struct {
		tool, err string
		extra     map[string]any
		setup     func(t *testing.T, home string)
		reason    string
	}{
		"interrupt":   {tool: "Bash", err: npmError, extra: map[string]any{"is_interrupt": true}, reason: "interrupt"},
		"short error": {tool: "Bash", err: "Exit code 1\nverify", reason: "short_error"},
		"own tool":    {tool: "mcp__plugin_backsight_memory__memory_search", err: "connection refused to the server at localhost", reason: "own_tool"},
		"paused": {tool: "Bash", err: npmError, reason: "paused", setup: func(t *testing.T, home string) {
			_ = sessions(home).Update("s1", func(st *session.State) error { st.Paused = true; return nil })
		}},
		"excluded": {tool: "Bash", err: npmError, reason: "excluded", setup: func(t *testing.T, home string) {
			appendConfig(t, home, "[privacy]\nexclude_paths = ["+testtool.TOMLStr(filepath.Join(home, "billing"))+"]")
		}},
		"budget": {tool: "Bash", err: npmError, reason: "budget", setup: func(t *testing.T, home string) {
			_ = sessions(home).Update("s1", func(st *session.State) error { st.CharsInjected = 6000; return nil })
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t)
			failureOn(t, home)
			recallAnswer = hits(0.95)
			if c.setup != nil {
				c.setup(t, home)
			}
			if h := failure(t, home, "s1", c.tool, c.err, c.extra); h.Specific.Context != "" || len(recallQueries) != 0 {
				t.Fatalf("searched %v and injected %q", recallQueries, h.Specific.Context)
			}
			if tel := telemetryText(t, home); !strings.Contains(tel, `"reason":"`+c.reason+`"`) {
				t.Errorf("no %s skip in telemetry:\n%s", c.reason, tel)
			}
		})
	}
}

func TestFailureRecallRedactsTheQueryAndKeepsToTheBudget(t *testing.T) {
	home := hookHome(t)
	failureOn(t, home)
	recallAnswer = hits(0.95, 0.9)
	_ = sessions(home).Update("s1", func(st *session.State) error { st.CharsInjected = 5900; return nil })
	secret := "sk-ant-api03-" + strings.Repeat("A1b2C3d4", 6)
	h := failure(t, home, "s1", "Bash", "Exit code 1\nAuthentication failed with key "+secret, map[string]any{"tool_input": map[string]any{"command": "curl -H 'x-api-key: " + secret + "' https://api"}})
	if len(recallQueries) == 0 {
		t.Fatal("no search")
	}
	for _, q := range recallQueries {
		if strings.Contains(q, secret) || len([]rune(q)) > 300 {
			t.Errorf("query leaks a key or is too long: %q", q)
		}
	}
	if n := len([]rune(h.Specific.Context)); n > 100+len([]rune(instructions.FailureHeader))+1 {
		t.Errorf("injected %d chars with 100 left in the session budget:\n%s", n, h.Specific.Context)
	}
}

func TestFailureSignatureIgnoresNumbersPathsHexAndCase(t *testing.T) {
	a := failureSignature("Bash", "Exit code 1\nENOENT: no such file '/a/b/c.txt' at 0xdeadbeef line 12")
	b := failureSignature("Bash", "Exit code 9\nenoent: no such file '/x/y.txt' at 0xcafebabe1 line 400")
	if a != b {
		t.Error("the same failure got two signatures")
	}
	if a == failureSignature("Edit", "Exit code 1\nENOENT: no such file '/a/b/c.txt' at 0xdeadbeef line 12") {
		t.Error("two tools share a signature")
	}
	if a == failureSignature("Bash", "Exit code 1\npermission denied while opening '/a/b/c.txt'") {
		t.Error("two different errors share a signature")
	}
}

// What the failure recall injects is journaled as shown, path failure.
func TestFailureRecallJournalsItsShows(t *testing.T) {
	home := hookHome(t)
	failureOn(t, home)
	recallAnswer = hits(0.95, 0.85, 0.82)
	h := failure(t, home, "s1", "Bash", npmError, map[string]any{"prompt_id": "p-1"})
	if !strings.Contains(h.Specific.Context, "[billing · m1] memory m1") {
		t.Fatalf("context:\n%s", h.Specific.Context)
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 1 || shows[0].Path != "failure" || shows[0].Turn != "p-1" || itemsText(shows[0].Items) != "m1:1:0.95,m2:2:0.85" {
		t.Errorf("shows %+v", shows)
	}
}
