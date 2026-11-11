package harness

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/harnessid"
)

// The ledger of Codex's methods. Each file of the adapter owns some of
// them, and its test file declares them in init() (codexOwns): the ones it
// implements (done, with their own tests) and its stubs, each with a probe
// that says whether it still answers "not implemented" (an error wrapping
// ErrNotImplemented, or, without an error, the answer of doing nothing for
// an input a real implementation answers). Two blocks never edit the same
// line: each keeps to its own test file.
//
// The rule for a block: implementing a method moves it from the file's
// stubs to its done, with tests; once the block is over its stubs are
// empty and it sets its part's over to true, which
// TestCodexPartsOverHaveNoStubs then holds.
type codexPart struct {
	done  []string
	stubs map[string]func() bool
	over  bool
}

var codexParts = map[string]codexPart{}

func codexOwns(file string, p codexPart) {
	if _, dup := codexParts[file]; dup {
		panic(file + " declared twice")
	}
	codexParts[file] = p
}

func init() {
	codexOwns("codex.go", codexPart{over: true,
		done: []string{"ID", "Name", "Tag", "ToolPrefix", "ToolNames", "HookArgs", "MCPArgs", "MCPEnvVars"}})
}

func notImplemented(err error) bool { return errors.Is(err, ErrNotImplemented) }

// Every stub still answers "not implemented", and every method of Codex is
// owned by exactly one file, either done or a stub: a method added and
// listed nowhere, listed twice, or a stub its block implemented without
// moving it, fails here.
func TestCodexStubsReportNotImplemented(t *testing.T) {
	owner := map[string]string{}
	for file, p := range codexParts {
		for name, stub := range p.stubs {
			if !stub() {
				t.Errorf("%s: %s no longer answers not implemented: move it to the file's done, with its own tests", file, name)
			}
		}
		names := slices.Clone(p.done)
		for name := range p.stubs {
			names = append(names, name)
		}
		for _, name := range names {
			if other, dup := owner[name]; dup {
				t.Errorf("%s is listed by %s and %s", name, other, file)
			}
			owner[name] = file
		}
	}
	typ := reflect.TypeOf(Codex{})
	var missing []string
	for i := range typ.NumMethod() {
		if _, ok := owner[typ.Method(i).Name]; !ok {
			missing = append(missing, typ.Method(i).Name)
		}
		delete(owner, typ.Method(i).Name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("Codex methods listed by no file: %v", missing)
	}
	for name, file := range owner {
		t.Errorf("%s lists %s, which Codex does not have", file, name)
	}
}

func TestCodexPartsOverHaveNoStubs(t *testing.T) {
	for file, p := range codexParts {
		if p.over && len(p.stubs) > 0 {
			t.Errorf("%s is over but keeps stubs", file)
		}
	}
}

// The error a stub returns is one line, saying what is not there yet.
func TestCodexStubErrorText(t *testing.T) {
	err := notYet("detection")
	if err.Error() != "codex detection: not available for Codex yet" || !notImplemented(err) {
		t.Errorf("%q", err)
	}
}

// Codex is on: after Claude Code in All (the flush loop and every
// per-harness pass run it), found by its id, never the default, and its id
// cannot be registered again.
func TestCodexIsOn(t *testing.T) {
	a, ok := ByID(harnessid.Codex)
	if !ok || reflect.TypeOf(a) != reflect.TypeOf(Codex{}) || Of(harnessid.Codex).ID() != harnessid.Codex || TagOf(harnessid.Codex) != "harness:codex" {
		t.Fatalf("ByID(codex) = %v %v", a, ok)
	}
	all := All()
	if len(all) < 2 || all[0].ID() != ClaudeCode || all[1].ID() != harnessid.Codex {
		t.Errorf("All = %v, want Claude Code then Codex", all)
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
	if c.ID() != "codex" || c.Tag() != "harness:codex" || c.Name() != "Codex" || c.ToolPrefix() != "mcp__backsight__" {
		t.Errorf("%q %q %q %q", c.ID(), c.Tag(), c.Name(), c.ToolPrefix())
	}
	want := []string{"mcp__backsight__memory_search", "mcp__backsight__memory_read", "mcp__backsight__memory_store", "mcp__backsight__memory_credit"}
	if got := c.ToolNames(); !slices.Equal(got, want) {
		t.Errorf("tools %v", got)
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
