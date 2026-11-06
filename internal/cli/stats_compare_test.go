package cli

import (
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// TestCompareFlags: the refusals of --compare and --measure on
// `stats releases` are exit 2 with the sentence the dashboard's 400 gives;
// a valid pair answers with the comparison, too few when no unit matches.
func TestCompareFlags(t *testing.T) {
	viewsFixture(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--compare", "v1"}, "--compare: two versions, as A,B"},
		{[]string{"--compare", "v1,v2,v3"}, "--compare: two versions, as A,B"},
		{[]string{"--compare", "v1,v1"}, "--compare: A and B are the same version"},
		{[]string{"--compare", "v1,v2", "--measure", "x"}, "--measure: one of used.turn_rate, recall.hit_rate, verdict.wrong_rate"},
		{[]string{"--measure", "recall.hit_rate"}, "--measure needs --compare"},
	} {
		v := runView(t, append([]string{"stats", "releases"}, c.args...)...)
		if v.code != output.Usage || v.env.OK || v.env.Error.Message != c.want {
			t.Errorf("%v: exit %d %q, want exit 2 %q", c.args, v.code, v.env.Error.Message, c.want)
		}
	}
	v := runView(t, "stats", "releases", "--compare", "nope-1,nope-2", "--measure", "recall.hit_rate")
	if v.code != output.OK {
		t.Fatalf("exit %d: %s", v.code, v.raw)
	}
	cmp := at(v.env.Data, "tables", "compare")
	if cmp == nil || at(cmp, "reading") != "too_few" || at(cmp, "measure") != "recall.hit_rate" || at(cmp, "too_few", "why") != "days" {
		t.Errorf("compare = %v, want too_few on recall.hit_rate", cmp)
	}
	// without --compare the view has no comparison
	v = runView(t, "stats", "releases")
	if at(v.env.Data, "tables", "compare") != nil {
		t.Errorf("a comparison nobody asked for: %v", at(v.env.Data, "tables", "compare"))
	}
}
