package claudecfg

import (
	"strings"
	"testing"
)

const settings = `{
  "model": "opus",
  "permissions": {
    "allow": [
      "Bash(git *)",
      "Read"
    ],
    "deny": []
  },
  "unknownKey": {"keep": [1, 2.50, "x"]}
}
`

func mustSet(t *testing.T, doc string, path []string, v any) (string, Change) {
	t.Helper()
	out, ch, err := Set([]byte(doc), path, v)
	if err != nil {
		t.Fatalf("Set %v: %v", path, err)
	}
	return string(out), ch
}

func TestSetReplacesAScalarInPlace(t *testing.T) {
	out, ch := mustSet(t, settings, []string{"model"}, "sonnet")
	if want := strings.Replace(settings, `"opus"`, `"sonnet"`, 1); out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	if ch.Path != "model" || ch.Old == nil || *ch.Old != `"opus"` || ch.New == nil || *ch.New != `"sonnet"` {
		t.Errorf("change %+v", ch)
	}
}

func TestSetAddsAMemberWithTheSiblingsIndentation(t *testing.T) {
	out, ch := mustSet(t, settings, []string{"cleanupPeriodDays"}, 3650)
	want := strings.Replace(settings, `"unknownKey": {"keep": [1, 2.50, "x"]}`, `"unknownKey": {"keep": [1, 2.50, "x"]},
  "cleanupPeriodDays": 3650`, 1)
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	if ch.Old != nil || *ch.New != "3650" {
		t.Errorf("change %+v", ch)
	}
}

func TestSetCreatesMissingParentsAndFillsAnEmptyDoc(t *testing.T) {
	for _, doc := range []string{"", "  \n", "{}", "{}\n"} {
		out, _ := mustSet(t, doc, []string{"permissions", "defaultMode"}, "plan")
		if !strings.Contains(out, "{\n  \"permissions\": {\n    \"defaultMode\": \"plan\"\n  }\n}") {
			t.Errorf("doc %q gave\n%s", doc, out)
		}
	}
	out, _ := mustSet(t, settings, []string{"permissions", "ask"}, []string{"Edit"})
	if !strings.Contains(out, `"deny": [],
    "ask": [
      "Edit"
    ]
  },`) {
		t.Errorf("nested insert:\n%s", out)
	}
}

func TestSetIsIdempotent(t *testing.T) {
	once, _ := mustSet(t, settings, []string{"cleanupPeriodDays"}, 3650)
	twice, ch := mustSet(t, once, []string{"cleanupPeriodDays"}, 3650)
	if twice != once || ch.Changed() {
		t.Errorf("second Set changed the doc or reported a change: %+v", ch)
	}
}

func TestSetRefusesAScalarParent(t *testing.T) {
	if _, _, err := Set([]byte(settings), []string{"model", "x"}, 1); err == nil {
		t.Error("a key under a string must be refused")
	}
}

func TestInvalidJSONIsRefused(t *testing.T) {
	for _, doc := range []string{`{"a": 1,}`, `{"a": `, `// comment
{}`, `[1]`} {
		if _, _, err := Set([]byte(doc), []string{"a"}, 2); err == nil {
			t.Errorf("%q must be refused", doc)
		}
	}
}

func TestEnsureInArrayAppendsOnceInTheSameStyle(t *testing.T) {
	out, ch, err := EnsureInArray([]byte(settings), []string{"permissions", "allow"}, "mcp__backsight__memory_search")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `      "Read",
      "mcp__backsight__memory_search"
    ],`) || !ch.Changed() {
		t.Errorf("got\n%s", out)
	}
	again, ch2, _ := EnsureInArray(out, []string{"permissions", "allow"}, "mcp__backsight__memory_search")
	if string(again) != string(out) || ch2.Changed() {
		t.Error("second ensure changed the doc")
	}
	empty, _, _ := EnsureInArray([]byte(settings), []string{"permissions", "deny"}, "Bash(rm *)")
	if !strings.Contains(string(empty), `"deny": [
      "Bash(rm *)"
    ]`) {
		t.Errorf("empty array:\n%s", empty)
	}
	missing, _, _ := EnsureInArray([]byte(`{}`), []string{"permissions", "allow"}, "Read")
	if string(missing) != "{\n  \"permissions\": {\n    \"allow\": [\n      \"Read\"\n    ]\n  }\n}" {
		t.Errorf("missing array:\n%s", missing)
	}
}

func TestEnsureInArrayRefusesANonArray(t *testing.T) {
	if _, _, err := EnsureInArray([]byte(settings), []string{"model"}, "x"); err == nil {
		t.Error("ensure on a string must be refused")
	}
}

func TestRemoveWhereDropsMembersAndElementsCleanly(t *testing.T) {
	doc := `{
  "mcpServers": {
    "hindsight": {"command": "npx", "env": {"HINDSIGHT_API_TOKEN": "hs_live_0123456789abcdefABCDEF0123456789"}},
    "github": {"command": "gh"},
    "memory": {"command": "py"}
  },
  "list": ["a", "b", "c"]
}`
	out, chs, err := RemoveWhere([]byte(doc), []string{"mcpServers"}, func(key string, _ any) bool { return key != "github" })
	if err != nil {
		t.Fatal(err)
	}
	if want := `{
  "mcpServers": {
    "github": {"command": "gh"}
  },
  "list": ["a", "b", "c"]
}`; string(out) != want {
		t.Errorf("got\n%s", out)
	}
	if len(chs) != 2 || chs[0].Path != "mcpServers.hindsight" || chs[0].New != nil {
		t.Fatalf("changes %+v", chs)
	}
	if strings.Contains(*chs[0].Old, "0123456789abcdef") {
		t.Errorf("removed value printed a token: %s", *chs[0].Old)
	}
	for _, c := range []struct {
		drop string
		want string
	}{{"a", `["b", "c"]`}, {"b", `["a", "c"]`}, {"c", `["a", "b"]`}} {
		out, _, _ := RemoveWhere([]byte(doc), []string{"list"}, func(_ string, v any) bool { return v == c.drop })
		if !strings.Contains(string(out), `"list": `+c.want+"\n}") {
			t.Errorf("drop %s:\n%s", c.drop, out)
		}
	}
	all, _, _ := RemoveWhere([]byte(doc), []string{"list"}, func(string, any) bool { return true })
	if !strings.Contains(string(all), `"list": []`) {
		t.Errorf("drop all:\n%s", all)
	}
	same, chs, _ := RemoveWhere([]byte(doc), []string{"absent"}, func(string, any) bool { return true })
	if string(same) != doc || len(chs) != 0 {
		t.Error("removing under a missing path must change nothing")
	}
}

func TestDuplicateKeysEditTheLastOne(t *testing.T) {
	out, _ := mustSet(t, `{"a": 1, "a": 2}`, []string{"a"}, 3)
	if out != `{"a": 1, "a": 3}` {
		t.Errorf("got %s", out)
	}
}

func TestEscapedKeysAndUnicodeArePreserved(t *testing.T) {
	doc := "{\n  \"caf\\u00e9\": \"é\",\n  \"x\": 1\n}\n"
	out, _ := mustSet(t, doc, []string{"café"}, "ok")
	if out != "{\n  \"caf\\u00e9\": \"ok\",\n  \"x\": 1\n}\n" {
		t.Errorf("got %q", out)
	}
}

func TestByteOrderMarkIsKept(t *testing.T) {
	out, _ := mustSet(t, "\ufeff{\"a\": 1}", []string{"a"}, 2)
	if out != "\ufeff{\"a\": 2}" {
		t.Errorf("got %q", out)
	}
}

func TestTabIndentedFilesKeepTabs(t *testing.T) {
	out, _ := mustSet(t, "{\n\t\"a\": 1\n}", []string{"b", "c"}, true)
	if out != "{\n\t\"a\": 1,\n\t\"b\": {\n\t\t\"c\": true\n\t}\n}" {
		t.Errorf("got %q", out)
	}
}

func TestMarkedBlock(t *testing.T) {
	md := "# Me\n\nMy rules.\n"
	out, ch, _ := MarkedBlock([]byte(md), "memory", "Use memory_search first.")
	want := md + "\n<!-- backsight:memory:begin -->\nUse memory_search first.\n<!-- backsight:memory:end -->\n"
	if string(out) != want || !ch.Changed() {
		t.Fatalf("add:\n%q", out)
	}
	again, ch, _ := MarkedBlock(out, "memory", "Use memory_search first.")
	if string(again) != want || ch.Changed() {
		t.Error("same block twice changed the file")
	}
	edited := strings.Replace(want, "My rules.", "My rules, edited.", 1)
	upd, _, _ := MarkedBlock([]byte(edited), "memory", "New text.")
	if string(upd) != strings.Replace(edited, "Use memory_search first.", "New text.", 1) {
		t.Errorf("update:\n%q", upd)
	}
	gone, ch, _ := MarkedBlock(upd, "memory", "")
	if string(gone) != "# Me\n\nMy rules, edited.\n" || !ch.Changed() {
		t.Errorf("remove:\n%q", gone)
	}
	fresh, _, _ := MarkedBlock(nil, "memory", "x")
	if string(fresh) != "<!-- backsight:memory:begin -->\nx\n<!-- backsight:memory:end -->\n" {
		t.Errorf("empty file:\n%q", fresh)
	}
}

func TestRemoveWhereHandlesKeysWithDotsAndSpaces(t *testing.T) {
	doc := `{"enabledPlugins": {"hindsight@vectorize.io": true, "my server": true, "keep": true}}`
	out, chs, err := RemoveWhere([]byte(doc), []string{"enabledPlugins"}, func(k string, _ any) bool { return k != "keep" })
	if err != nil || string(out) != `{"enabledPlugins": {"keep": true}}` || len(chs) != 2 {
		t.Fatalf("out %s chs %+v err %v", out, chs, err)
	}
	if chs[1].Path != `enabledPlugins."my server"` {
		t.Errorf("second change %+v", chs[1])
	}
	if _, _, err := RemoveWhere([]byte(doc), []string{"enabledPlugins", "keep"}, func(string, any) bool { return true }); err == nil {
		t.Error("removing under a boolean must be refused")
	}
}

// Unbalanced markers would make the next edit swallow the user's text in
// between: refused (M1 review).
func TestMarkedBlockRefusesUnbalancedMarkers(t *testing.T) {
	for _, md := range []string{
		"<!-- backsight:memory:begin -->\nold\n\n## My notes\nkeep me\n",
		"## My notes\n<!-- backsight:memory:end -->\n",
		"<!-- backsight:memory:end -->\n<!-- backsight:memory:begin -->\n",
		"<!-- backsight:memory:begin -->\na\n<!-- backsight:memory:end -->\n<!-- backsight:memory:begin -->\nb\n<!-- backsight:memory:end -->\n",
	} {
		if _, _, err := MarkedBlock([]byte(md), "memory", "new"); err == nil {
			t.Errorf("accepted:\n%s", md)
		}
	}
}

func TestEnsureInArrayNamesTheAppendedElement(t *testing.T) {
	_, ch, _ := EnsureInArray([]byte(settings), []string{"permissions", "allow"}, "Edit")
	if ch.Path != "permissions.allow[+]" || ch.Old != nil || *ch.New != `"Edit"` {
		t.Errorf("change %+v", ch)
	}
}

func TestSetRedactsTheOldValueItReplaces(t *testing.T) {
	doc := `{"env": {"GITHUB_TOKEN": "ghp_0123456789abcdefghijklmnopqrstuvwxyz01"}}`
	_, ch, err := Set([]byte(doc), []string{"env", "GITHUB_TOKEN"}, "")
	if err != nil || ch.Old == nil || strings.Contains(*ch.Old, "0123456789abcdef") || !ch.Changed() {
		t.Errorf("change %+v err %v", ch, err)
	}
}

// The bytes-vs-tree check refuses a patch that does not do what the
// operation means, even when the bytes are valid JSON.
func TestPatchRefusesBytesThatDisagreeWithTheTree(t *testing.T) {
	lie := func(src []byte, _ *node) ([]byte, error) { return []byte(`{"a": 2}`), nil }
	if _, err := patch([]byte(`{"a": 1}`), lie, func(tree any) (any, error) { return treeSet(tree, []string{"a"}, 3) }); err == nil {
		t.Error("a lying patch was accepted")
	}
	broken := func(src []byte, _ *node) ([]byte, error) { return []byte(`{"a": `), nil }
	if _, err := patch([]byte(`{"a": 1}`), broken, nil); err == nil {
		t.Error("invalid patched JSON accepted")
	}
}
