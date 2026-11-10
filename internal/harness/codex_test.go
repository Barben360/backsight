package harness

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/state"
)

// Codex is known by its id but off: nothing iterates it, nothing defaults
// to it, and its id cannot be registered again.
func TestCodexIsKnownButOff(t *testing.T) {
	a, ok := ByID(harnessid.Codex)
	if !ok || reflect.TypeOf(a) != reflect.TypeOf(Codex{}) || Of(harnessid.Codex).ID() != harnessid.Codex || TagOf(harnessid.Codex) != "harness:codex" {
		t.Fatalf("ByID(codex) = %v %v", a, ok)
	}
	for _, b := range All() {
		if b.ID() == harnessid.Codex {
			t.Error("Codex is in All: the flush loop and every per-harness pass would run it")
		}
	}
	if Default().ID() != ClaudeCode || FromContext(context.Background()).ID() != ClaudeCode {
		t.Error("the default is not Claude Code's")
	}
	defer func() {
		if recover() == nil {
			t.Error("Codex's id registered a second time")
		}
	}()
	Register(Codex{})
}

func TestCodexIdentityAndTools(t *testing.T) {
	c := Codex{}
	if c.ID() != "codex" || c.Tag() != "harness:codex" || c.Name() != "Codex" || c.ToolPrefix() != "mcp__memory__" {
		t.Errorf("%q %q %q %q", c.ID(), c.Tag(), c.Name(), c.ToolPrefix())
	}
	want := []string{"mcp__memory__memory_search", "mcp__memory__memory_read", "mcp__memory__memory_store", "mcp__memory__memory_credit"}
	if got := c.ToolNames(); !slices.Equal(got, want) {
		t.Errorf("tools %v", got)
	}
	if c.MinVersion() != "0.167.0" {
		t.Error(c.MinVersion())
	}
}

// The command lines are the contracts of codex.go: the event is the hook
// command's last word, after the harness; the server names its harness and
// gets BACKSIGHT_HOME.
func TestCodexCommandLines(t *testing.T) {
	c := Codex{}
	for _, e := range c.Events() {
		args := c.HookArgs(e.Event)
		if !strings.HasSuffix(strings.Join(args, " "), "hook --harness codex "+e.Event) || args[len(args)-1] != e.Event || args[0] != "hook" {
			t.Errorf("%s: %v", e.Event, args)
		}
	}
	if got := c.MCPArgs(); !slices.Equal(got, []string{"mcp", "--harness", "codex"}) {
		t.Errorf("mcp args %v", got)
	}
	if got := c.MCPEnvVars(); !slices.Equal(got, []string{"BACKSIGHT_HOME"}) {
		t.Errorf("env vars %v", got)
	}
}

// The event map is 0085 D3's: each Codex event is one Codex sends (a
// recorded fixture), each runs one of backsight's events, every handler has
// its own timeout, the memory tools' matcher is Codex's names only, and no
// failure hook is wired (Codex has none).
func TestCodexEventMap(t *testing.T) {
	ids := []string{EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse, EventPostToolUseFailure,
		EventSubagentStart, EventSubagentStop, EventStop, EventSessionEnd}
	events := Codex{}.Events()
	if len(events) != 9 {
		t.Fatalf("%d events", len(events))
	}
	var sources []string
	for _, e := range events {
		if !slices.Contains(ids, e.Event) || e.Event == EventPostToolUseFailure {
			t.Errorf("%s runs %q", e.Name, e.Event)
		}
		if e.Timeout <= 0 {
			t.Errorf("%s has no timeout", e.Name)
		}
		if got, _ := filepath.Glob(filepath.Join("..", "testdata", "codex", "0.167.0", "hooks", e.Name+"*.json")); len(got) == 0 {
			t.Errorf("Codex sent no %s in the recorded run", e.Name)
		}
		switch e.Name {
		case "SessionStart":
			sources = append(sources, strings.Split(e.Matcher, "|")...)
		case "PostToolUse":
			m := regexp.MustCompile("^(?:" + e.Matcher + ")$")
			if !m.MatchString("mcp__memory__memory_search") || m.MatchString("mcp__plugin_backsight_memory__memory_search") {
				t.Errorf("PostToolUse matcher %q", e.Matcher)
			}
		}
	}
	slices.Sort(sources)
	if !slices.Equal(sources, []string{"clear", "compact", "fork", "resume", "startup"}) {
		t.Errorf("SessionStart sources %v", sources)
	}
	events[0].Name = "changed"
	if (Codex{}).Events()[0].Name == "changed" {
		t.Error("Events hands out the table itself")
	}
}

// codexDone are the Codex methods this skeleton implements: identity,
// tool names and the contracts of codex.go. Every other method is a stub
// its block replaces; the block moves it here and gives it real tests.
var codexDone = []string{"ID", "Name", "Tag", "ToolPrefix", "ToolNames", "HookArgs", "MCPArgs", "MCPEnvVars", "MinVersion", "ProfileVar", "Events"}

// codexStubs call each stub and say whether it answered "not implemented":
// an error wrapping ErrNotImplemented, or the zero value where the method
// has no error (an answer that does nothing).
var codexStubs = map[string]func() bool{
	// block 3, https://github.com/Barben360/backsight/issues/278
	"IsDefaultProfile": func() bool { return !(Codex{}).IsDefaultProfile("/home/u", "") },
	"Locate":           func() bool { return (Codex{}).Locate(Env{Home: "/home/u"}) == Location{} },
	"Detect":           func() bool { _, err := (Codex{}).Detect(context.Background(), Env{}); return notImplemented(err) },
	"Install": func() bool {
		r, err := (Codex{}).Install(context.Background(), Env{}, Integration{}, true)
		return notImplemented(err) && len(r.Steps) == 0
	},
	"Uninstall": func() bool { return notImplemented((Codex{}).Uninstall(context.Background(), Env{})) },
	"Installed": func() bool { _, err := (Codex{}).Installed(context.Background(), Env{}); return notImplemented(err) },
	"Drift":     func() bool { return (Codex{}).Drift(Env{}, Recorded{}, nil) == nil },
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
	// block 4, https://github.com/Barben360/backsight/issues/279
	"DecodeHook": func() bool {
		_, err := (Codex{}).DecodeHook(EventStop, []byte(`{"session_id":"s"}`))
		return notImplemented(err)
	},
	"EncodeHook":     func() bool { return (Codex{}).EncodeHook(HookOutput{Event: EventStop, Block: "b"}) == nil },
	"PairsBySession": func() bool { return !(Codex{}).PairsBySession("linux") },
	// block 5, https://github.com/Barben360/backsight/issues/280
	"MCPCallSession": func() bool {
		return (Codex{}).MCPCallSession(map[string]any{"sessionId": "s"}, func(string) string { return "s" }) == ""
	},
	// block 6, https://github.com/Barben360/backsight/issues/281
	"Conversations": func() bool { return (Codex{}).Conversations(Env{Home: "/home/u"}) == nil },
	"SessionOf":     func() bool { return (Codex{}).SessionOf("/x/rollout-2026-01-01T00-00-00-abc.jsonl") == "" },
	"Size":          func() bool { _, err := (Codex{}).Size("/x"); return notImplemented(err) },
	"Open":          func() bool { _, err := (Codex{}).Open("/x", 0); return notImplemented(err) },
	"ReadConversation": func() bool {
		_, _, err := (Codex{}).ReadConversation(strings.NewReader(""), 0)
		return notImplemented(err)
	},
	// block 7, https://github.com/Barben360/backsight/issues/282
	"Texts":  func() bool { return (Codex{}).Texts() == Texts{} },
	"Skills": func() bool { return (Codex{}).Skills("/shim") == nil },
}

func notImplemented(err error) bool { return errors.Is(err, ErrNotImplemented) }

// Every stub still answers "not implemented", and every method of Codex
// is either done or a stub: a method added without being listed, or a
// block's method that is neither implemented and listed done nor still a
// stub, fails here.
func TestCodexStubsReportNotImplemented(t *testing.T) {
	for name, stub := range codexStubs {
		if !stub() {
			t.Errorf("%s no longer answers not implemented: its block implemented it, move it to codexDone with its own tests", name)
		}
	}
	typ := reflect.TypeOf(Codex{})
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		_, stub := codexStubs[name]
		if done := slices.Contains(codexDone, name); done == stub {
			t.Errorf("%s: listed done %v and stub %v, want exactly one", name, done, stub)
		}
	}
	if n := len(codexDone) + len(codexStubs); n != typ.NumMethod() {
		t.Errorf("%d methods listed, Codex has %d", n, typ.NumMethod())
	}
	if err := notYet("x"); !strings.Contains(err.Error(), "not available for Codex yet") {
		t.Errorf("stub error %q", err)
	}
}
