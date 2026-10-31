package cli

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

// TestStaleAgreesWithTheView: `memory stale` and the bank view's review
// queue name the same memories, and the view's counts are the lists'
// lengths. It guards the index's mirror of the ledger: the classification
// is one function, feedback.Stale.
func TestStaleAgreesWithTheView(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	writeStaleLedger(t, home)

	code, out, errOut := run(t, "memory", "stale", "--json")
	if code != output.OK {
		t.Fatalf("memory stale: exit %d: %s %s", code, out, errOut)
	}
	var stale staleOut
	if err := json.Unmarshal([]byte(out), &stale); err != nil {
		t.Fatal(err)
	}

	v := runView(t, "stats", "bank")
	if v.code != output.OK {
		t.Fatalf("bank: exit %d: %s", v.code, v.raw)
	}
	idsOf := func(list string) []string {
		var ids []string
		rows, _ := at(v.env.Data, "tables", "review", list).([]any)
		for _, r := range rows {
			ids = append(ids, at(r, "id").(string))
		}
		return ids
	}
	for _, c := range []struct {
		list   string
		metric string
		want   []staleMemory
	}{{"never_used", "stale.never_used", stale.Data.NeverUsed}, {"hidden", "stale.hidden", stale.Data.Hidden}, {"reported", "stale.reported", stale.Data.Reported}} {
		var want []string
		for _, m := range c.want {
			want = append(want, m.ID)
		}
		if len(want) == 0 {
			t.Fatalf("%s: the fixture lists none", c.list)
		}
		if got := idsOf(c.list); !slices.Equal(got, want) {
			t.Errorf("review.%s = %v, memory stale lists %v", c.list, got, want)
		}
		if got := at(v.env.Data, "metrics", c.metric, "value"); got != float64(len(want)) {
			t.Errorf("%s = %v, want %d", c.metric, got, len(want))
		}
	}
}
