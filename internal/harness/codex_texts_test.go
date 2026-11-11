package harness

import (
	"reflect"
	"testing"
)

// codex_texts.go's ledger: block 7, https://github.com/Barben360/backsight/issues/282.
// Its methods are done as a copy of Claude Code's; block 7 rewrites them
// and replaces this file's test.
func init() {
	codexOwns("codex_texts.go", codexPart{done: []string{"Texts", "Skills"}})
}

// Until block 7, Codex's texts and skills are Claude Code's, as block 3
// ships them.
func TestCodexTextsAreClaudesUntilRewritten(t *testing.T) {
	c := Codex{}
	if c.Texts() != (Claude{}).Texts() {
		t.Errorf("texts %+v", c.Texts())
	}
	if got, want := c.Skills("/data/bin/current"), (Claude{}).Skills("/data/bin/current"); len(got) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("%d skills, Claude Code's %d", len(got), len(want))
	}
}
