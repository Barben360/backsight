package codexcfg

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/claudecfg"
)

const fixtures = "../testdata/codex/0.167.0/"

func read(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, rel))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// What `codex plugin remove` leaves (checks/6-removal/config-after.toml):
// the edit removes exactly backsight's 18 [hooks.state] tables, each with
// the blank line before it, and keeps every other byte, Codex's own
// [hooks.state] and the user-level hooks' trust included.
func TestRemoveTrustOnTheRecordedRemoval(t *testing.T) {
	before := read(t, "checks/6-removal/config-after.toml")
	lines := strings.SplitAfter(string(before), "\n")
	// The tables of backsight's two plugins start at line 67 (blank line 66
	// before it) and run to the end of the file; lines 1-65 are the user's
	// and Codex's own.
	if !strings.HasPrefix(lines[66], `[hooks.state."backsight@backsight:`) || lines[65] != "\n" {
		t.Fatalf("the fixture changed shape: line 66 %q, line 67 %q", lines[65], lines[66])
	}
	want := strings.Join(lines[:65], "")
	out, changes, err := RemoveTrust(before)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("not the file without backsight's tables:\n%s", out)
	}
	if len(changes) != 18 {
		t.Errorf("%d changes, want 18", len(changes))
	}
	for _, ch := range changes {
		if !strings.Contains(ch.Path, "backsight") || !ch.Changed() {
			t.Errorf("change %+v", ch)
		}
	}
	if !strings.Contains(string(out), "[hooks.state]\n") || !strings.Contains(string(out), `[hooks.state."/home/tester/.codex/hooks.json:interrupt:0:0"]`) {
		t.Error("Codex's own tables went too")
	}
	// Run again: nothing left to remove.
	again, changes, err := RemoveTrust(out)
	if err != nil || len(changes) != 0 || !bytes.Equal(again, out) {
		t.Errorf("second run: %d changes, %v", len(changes), err)
	}
}

// With the user's tables among backsight's, a comment heading the next
// table and strings holding what looks like a header, only backsight's
// tables go.
func TestRemoveTrustKeepsWhatIsNotBacksights(t *testing.T) {
	in := `model = "x"
notes = """
[hooks.state."backsight@backsight:hooks/hooks.json:stop:0:0"]
not a table: a string
"""

[hooks.state."backsight-hooks@backsight:hooks/hooks.json:stop:0:0"]
trusted_hash = "sha256:aa" # a comment on its line
trusted_content_hash = "sha256:bb"

# the user's own hook, kept
[hooks.state."/home/u/.codex/hooks.json:stop:0:0"]
trusted_hash = "sha256:cc"

[hooks.state."backsight@backsight:hooks/hooks.json:session_start:1:0"]
trusted_hash = "sha256:dd"
list = [
  "a",
  "]",
]

[hooks.state."backsight-hooksX@backsight:x"]
trusted_hash = "sha256:ee"
[tui]
x = 1
`
	want := `model = "x"
notes = """
[hooks.state."backsight@backsight:hooks/hooks.json:stop:0:0"]
not a table: a string
"""

# the user's own hook, kept
[hooks.state."/home/u/.codex/hooks.json:stop:0:0"]
trusted_hash = "sha256:cc"

[hooks.state."backsight-hooksX@backsight:x"]
trusted_hash = "sha256:ee"
[tui]
x = 1
`
	out, changes, err := RemoveTrust([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Errorf("got:\n%s", out)
	}
	if len(changes) != 2 {
		t.Errorf("%d changes", len(changes))
	}
}

// The editor runs under the config editor's rules: undo puts every table
// back, byte for byte.
func TestRemoveTrustIsUndone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	before := read(t, "checks/6-removal/config-after.toml")
	if err := os.WriteFile(target, before, 0o600); err != nil {
		t.Fatal(err)
	}
	r := claudecfg.Runner{PreImages: filepath.Join(dir, "pre"), Journal: filepath.Join(dir, "journal.jsonl"), Command: "uninstall", Now: time.Now}
	run, fr, err := r.Apply([]claudecfg.Edit{{Target: target, Apply: RemoveTrust}})
	if err != nil || run == "" || !fr[0].Written {
		t.Fatalf("run %q %+v %v", run, fr, err)
	}
	if got, _ := os.ReadFile(target); bytes.Contains(got, []byte(`"backsight`)) {
		t.Fatal("tables left")
	}
	if _, _, err := r.Restore(run, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, before) {
		t.Error("undo did not put the file back as it was")
	}
}

// A trust table with a sub-table cannot be removed whole: refused, named
// by TrustKeys for the user to remove by hand.
func TestRemoveTrustRefusesASubTable(t *testing.T) {
	in := []byte("[hooks.state]\n\n[hooks.state.\"backsight@backsight:x\"]\ntrusted_hash = \"a\"\n\n[hooks.state.\"backsight@backsight:x\".sub]\nk = 1\n")
	if _, _, err := RemoveTrust(in); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("removed: %v", err)
	}
	keys, err := TrustKeys(in)
	if err != nil || !slices.Equal(keys, []string{"backsight@backsight:x"}) {
		t.Errorf("keys %v %v", keys, err)
	}
	if keys, err := TrustKeys([]byte("model = 1\n")); err != nil || len(keys) != 0 {
		t.Errorf("no trust: %v %v", keys, err)
	}
}

// Uninstall's way back sets memories = true only while the key is absent:
// a value the user set since stays.
func TestMemoriesBackOn(t *testing.T) {
	out, ch, err := MemoriesBackOn([]byte("[features]\n"))
	if err != nil || string(out) != "[features]\nmemories = true\n" || !ch[0].Changed() {
		t.Errorf("absent: %q %v %v", out, ch, err)
	}
	for _, in := range []string{"[features]\nmemories = false\n", "[features]\nmemories = true\n"} {
		out, ch, err := MemoriesBackOn([]byte(in))
		if err != nil || string(out) != in || len(ch) != 0 {
			t.Errorf("%q: %q %v %v", in, out, ch, err)
		}
	}
}

func TestIsTrustTable(t *testing.T) {
	for path, want := range map[string]bool{
		"hooks|state|backsight@backsight:hooks/hooks.json:stop:0:0":       true,
		"hooks|state|backsight-hooks@backsight:hooks/hooks.json:stop:0:0": true,
		"hooks|state|backsight@backsight":                                 false, // no ':'
		"hooks|state|other@backsight:hooks/hooks.json:stop:0:0":           false,
		"hooks|state|/home/u/.codex/hooks.json:stop:0:0":                  false,
		"hooks|backsight@backsight:x":                                     false,
		"hooks|state|backsight@backsight:x|deeper":                        false,
	} {
		if got := IsTrustTable(strings.Split(path, "|")); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

// Codex's own memories off, as `codex features disable memories` does,
// then back at the same place, byte for byte, though Codex wrote other
// tables meanwhile.
func TestMemoriesOffAndBack(t *testing.T) {
	for name, in := range map[string]string{
		"alone":  "model = \"m\"\n\n[features]\nmemories = true\n",
		"middle": "[features]\nhooks = true\nmemories   =   true # mine\nplugins = true\n\n[tui]\nx = 1\n",
		"first":  "[features]\nmemories = true\nhooks = true\n",
		"dotted": "features.memories = true\nmodel = \"m\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			on, err := MemoriesOn([]byte(in))
			if err != nil || !on {
				t.Fatalf("on %v %v", on, err)
			}
			off, changes, err := MemoriesOff([]byte(in))
			if err != nil {
				t.Fatal(err)
			}
			if on, _ := MemoriesOn(off); on || strings.Contains(string(off), "memories") {
				t.Fatalf("still on:\n%s", off)
			}
			if len(changes) != 1 || !changes[0].Changed() || changes[0].Path != "features.memories" {
				t.Errorf("changes %+v", changes)
			}
			if name == "alone" && string(off) != "model = \"m\"\n\n[features]\n" {
				t.Errorf("the [features] table should stay, empty, as Codex leaves it: %q", off)
			}
			// Codex adds its tables at the end while backsight is installed.
			later := string(off) + "\n[plugins.\"backsight@backsight\"]\nenabled = true\n"
			back, changes, err := MemoriesBack([]byte(in))([]byte(later))
			if err != nil {
				t.Fatal(err)
			}
			if string(back) != in+"\n[plugins.\"backsight@backsight\"]\nenabled = true\n" && name != "dotted" {
				t.Errorf("not back at its place:\n%s", back)
			}
			if on, _ := MemoriesOn(back); !on || !changes[0].Changed() {
				t.Errorf("not back on:\n%s", back)
			}
			// Set again since: nothing to do.
			if again, changes, err := MemoriesBack([]byte(in))(back); err != nil || !bytes.Equal(again, back) || changes[0].Changed() {
				t.Errorf("second restore changed the file: %v", err)
			}
		})
	}
	// Another value is the user's: left as it is.
	for _, in := range []string{"[features]\nmemories = false\n", "[memories]\nuse_memories = true\n", ""} {
		out, changes, err := MemoriesOff([]byte(in))
		if err != nil || string(out) != in || len(changes) != 0 {
			t.Errorf("%q: %q %v %v", in, out, changes, err)
		}
	}
}

// The approval fallback writes the key Codex's "Always allow" writes, and
// it decodes as that key; a second run changes nothing.
func TestApprovalFallback(t *testing.T) {
	in := []byte("model = \"m\"\n")
	out, changes, err := ApprovalFallback("memory_store", "memory_credit")(in)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := toml.Unmarshal(out, &tree); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"memory_store", "memory_credit"} {
		v := tree["plugins"].(map[string]any)["backsight@backsight"].(map[string]any)["mcp_servers"].(map[string]any)["backsight"].(map[string]any)["tools"].(map[string]any)[tool].(map[string]any)["approval_mode"]
		if v != "approve" {
			t.Errorf("%s: %v", tool, v)
		}
	}
	if !strings.Contains(string(out), "\n[plugins.\"backsight@backsight\".mcp_servers.backsight.tools.memory_store]\napproval_mode = \"approve\"\n") {
		t.Errorf("got:\n%s", out)
	}
	if len(changes) != 2 || !changes[0].Changed() {
		t.Errorf("changes %+v", changes)
	}
	again, changes, err := ApprovalFallback("memory_store", "memory_credit")(out)
	if err != nil || !bytes.Equal(again, out) || changes[0].Changed() || changes[1].Changed() {
		t.Errorf("second run changed it: %v", err)
	}
}

func TestSetReplacesAValueInPlace(t *testing.T) {
	in := "[a]\nk = 1 # keep\nj = 2\n"
	out, ch, err := Set([]byte(in), []string{"a", "k"}, "3")
	if err != nil || string(out) != "[a]\nk = 3 # keep\nj = 2\n" || !ch.Changed() {
		t.Errorf("%q %+v %v", out, ch, err)
	}
	out, _, err = Set([]byte(in), []string{"a", "new key"}, `"v"`)
	if err != nil || string(out) != "[a]\nk = 1 # keep\nj = 2\n\"new key\" = \"v\"\n" {
		t.Errorf("%q %v", out, err)
	}
	if _, _, err := Set([]byte(in), []string{"a", "k"}, "not toml ="); err == nil {
		t.Error("a value that is not TOML was written")
	}
}

// A file the editor cannot read is refused, never guessed at; a file that
// is not TOML is refused as such.
func TestUnsupportedIsRefused(t *testing.T) {
	for _, in := range []string{
		"k = \"unclosed\n",
		"[a\nk = 1\n",
		"k = [1, 2\n",
		"k = '''never closed\n",
		"= 1\n",
	} {
		if _, _, err := RemoveTrust([]byte(in)); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
	_, _, err := RemoveTrust([]byte("[a]\nk = 1\n[a]\nk = 2\n[hooks.state.\"backsight@backsight:x\"]\nt = 1\n"))
	if err == nil {
		t.Error("an invalid TOML file (a table twice) was edited")
	}
	if !errors.Is(fmt.Errorf("%w", ErrUnsupported), ErrUnsupported) {
		t.Fatal("sentinel")
	}
}

func TestCRLFIsKept(t *testing.T) {
	in := "[features]\r\nmemories = true\r\n\r\n[tui]\r\nx = 1\r\n"
	off, _, err := MemoriesOff([]byte(in))
	if err != nil || string(off) != "[features]\r\n\r\n[tui]\r\nx = 1\r\n" {
		t.Fatalf("%q %v", off, err)
	}
	back, _, err := MemoriesBack([]byte(in))(off)
	if err != nil || string(back) != in {
		t.Errorf("%q %v", back, err)
	}
}

func TestMCPServerTable(t *testing.T) {
	for in, want := range map[string]bool{
		"[mcp_servers.memory]\ncommand = \"x\"\n":          true,
		"[mcp_servers.memory.env]\nA = \"1\"\n":            true,
		"mcp_servers.memory.command = \"x\"\n":             true,
		"[mcp_servers]\nmemory = { command = \"x\" }\n":    true,
		"[mcp_servers.memoryx]\ncommand = \"x\"\n":         false,
		"[mcp_servers.probe]\nmemory = 1\n":                false,
		"[plugins.\"backsight@backsight\"]\nenabled = 1\n": false,
	} {
		got, err := MCPServerTable([]byte(in), "memory")
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	got, err := MCPServerTable(read(t, "config/config-end.toml"), "memory")
	if err != nil || got {
		t.Errorf("the recorded config: %v %v", got, err)
	}
}
