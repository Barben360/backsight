package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// sharedNode draws memory m's page with the other PCs' counts sh and the
// server's answer read.
func sharedNode(t *testing.T, sh *metrics.SharedCounts, read *metrics.ServerState, edit func(m *metrics.MemoryPage)) *node {
	t.Helper()
	return detailNode(t, "memory", "/memories/m", views.Params{Limit: 50}, func(d *PageData) {
		d.V["memories"].GeneratedAt = detailBase
		d.Frame.Generated, d.Frame.ServerRead = detailBase, read
		m := d.V["memories"].Tables.Memory
		m.Row.ID, m.Shared = "7f3a9c21e04b4d0e9a1f6c55b2d18e07", sh
		if edit != nil {
			edit(m)
		}
	})
}

// countedShared is the design's counts: 8.6 used and 5.1 ignored here, 4.2 and
// 1.8 on 3 other PCs.
func countedShared() *metrics.SharedCounts {
	at := detailBase.Add(-2 * time.Hour)
	return &metrics.SharedCounts{State: "counted", Machines: 3, CombinedAt: &at,
		This:   metrics.Counts{Used: 8.64, Ignored: 5.06, Rate: fp(0.63)},
		Others: metrics.Counts{Used: 4.21, Ignored: 1.8, Rate: fp(0.7)},
		All:    metrics.Counts{Used: 12.84, Ignored: 6.86, Rate: fp(0.65)}}
}

func sharedCardOf(t *testing.T, n *node) *node { return cardOf(t, n, "w-memory-shared") }

func TestMemorySharedCardStates(t *testing.T) {
	answering := &metrics.ServerState{OK: true}
	pillOf := func(c *node) string {
		if p := c.first("div.card-head span.pill"); p != nil {
			return p.text()
		}
		return ""
	}
	t.Run("counted", func(t *testing.T) {
		n := sharedNode(t, countedShared(), answering, nil)
		c := sharedCardOf(t, n)
		ids := cardIDs(n)
		if at := slices.Index(ids, "w-memory-shared"); at < 0 || at+1 >= len(ids) || ids[at+1] != "w-memory-strip" {
			t.Errorf("cards %v: the card is not right before the timeline", ids)
		}
		if h := c.first("h2"); h.text() != "Has it helped on your other PCs?" {
			t.Errorf("title %q", h.text())
		}
		if chip := c.first("div.card-head span.chip"); chip == nil || chip.text() != "all PCs (4)" || chip.attr("title") != "This PC’s counts and the counts your other PCs share" {
			t.Errorf("chip %v", chip)
		}
		if pillOf(c) != "" {
			t.Errorf("pill %q", pillOf(c))
		}
		if got, want := texts(c.find("thead th")), []string{"Recent-weighted counts", "This PC", "3 other PCs", "All PCs"}; !slices.Equal(got, want) {
			t.Errorf("headings %q, want %q", got, want)
		}
		want := [][]string{
			{"Used by Claude", "8.6", "4.2", "12.8"},
			{"Shown, then ignored", "5.1", "1.8", "6.9"},
			{"Used when judged", "63 %", "70 %", "65 %"},
			{"Hidden from recall", "no", "no", "no"},
		}
		got := cellsOf(c)
		for i := range got {
			got[i][0] = strings.TrimSpace(got[i][0])
		}
		if len(got) != len(want) {
			t.Fatalf("rows %q", got)
		}
		for i := range want {
			if !slices.Equal(got[i], want[i]) {
				t.Errorf("row %d: %q, want %q", i, got[i], want[i])
			}
		}
		rows := c.find("tbody tr")
		if rows[0].first("i.sw.f-used") == nil || rows[1].first("i.sw.f-ignored") == nil {
			t.Error("the used and ignored rows lack their swatches")
		}
		if b := rows[0].find("td")[3].first("b"); b == nil || b.text() != "12.8" {
			t.Errorf("the All PCs cell is not set in bold: %v", b)
		}
		info := c.first("details.info")
		if dts := texts(info.find("dt")); !slices.Equal(dts, []string{"What", "Where from", "Used for"}) {
			t.Errorf("popover rows %q", dts)
		}
		if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats memories 7f3a9c21e04b4d0e9a1f6c55b2d18e07 --machines all" {
			t.Errorf("CLI %v", cli)
		}
		foot := c.first("p.xs.muted")
		if foot == nil || foot.first("time") == nil || spaced(foot.text()) != "Combined 2 h ago from the copies the other PCs share: memory ids and numbers only. The page shows their sum, not which PC. Shows, the timeline and the ranks below are this PC’s." {
			t.Errorf("foot %v", foot)
		}
		if len(c.find("div.state")) != 0 || len(c.find("div.cmd")) != 1 { // the CLI popover only
			t.Errorf("a notice or a command on the plain state")
		}
	})
	t.Run("one rate is empty", func(t *testing.T) {
		sh := countedShared()
		sh.Others = metrics.Counts{}
		c := sharedCardOf(t, sharedNode(t, sh, answering, nil))
		if got := cellsOf(c)[2]; got[2] != "—" {
			t.Errorf("the rate of no count reads %q", got[2])
		}
		if got := c.find("thead th")[2].text(); got != "3 other PCs" {
			t.Errorf("heading %q", got)
		}
		sh.Machines = 1
		c = sharedCardOf(t, sharedNode(t, sh, answering, nil))
		if got := c.find("thead th")[2].text(); got != "1 other PC" {
			t.Errorf("heading %q", got)
		}
	})
	t.Run("hidden elsewhere", func(t *testing.T) {
		sh := countedShared()
		since := time.Date(2026, 10, 12, 15, 20, 0, 0, time.UTC)
		sh.HiddenElsewhere = &since
		c := sharedCardOf(t, sharedNode(t, sh, answering, nil))
		if pillOf(c) != "hidden elsewhere" || !strings.Contains(c.first("div.card-head span.pill").attr("class"), "st-watch") {
			t.Errorf("pill %q", pillOf(c))
		}
		s := c.first("div.state")
		if s == nil || spaced(s.text()) != "Hidden from recall on one of your other PCs since Oct 12. Recall skips it on this PC too. To use it here again:" || s.first("time") == nil {
			t.Errorf("state %v", c.find("div.state"))
		}
		var cmds []string
		for _, code := range c.find("div.cmd code") {
			cmds = append(cmds, code.Text)
		}
		if !slices.Contains(cmds, "backsight forget --restore 7f3a9c21e04b4d0e9a1f6c55b2d18e07") {
			t.Errorf("commands %q", cmds)
		}
		if got := cellsOf(c)[3]; !slices.Equal(got, []string{"Hidden from recall", "no", "yes", "yes"}) {
			t.Errorf("hidden row %q", got)
		}
	})
	t.Run("hidden here", func(t *testing.T) {
		c := sharedCardOf(t, sharedNode(t, countedShared(), answering, func(m *metrics.MemoryPage) { m.Row.State = "hidden" }))
		if got := cellsOf(c)[3]; !slices.Equal(got, []string{"Hidden from recall", "yes", "—", "yes"}) {
			t.Errorf("hidden row %q", got)
		}
		if len(c.find("div.state")) != 0 {
			t.Error("a hidden-elsewhere notice for a memory hidden here")
		}
	})
	t.Run("not counted", func(t *testing.T) {
		sh := &metrics.SharedCounts{State: "not_counted", Machines: 2, This: metrics.Counts{Used: 1, Ignored: 0, Rate: fp(1)}}
		c := sharedCardOf(t, sharedNode(t, sh, answering, nil))
		if p := c.first("p.small"); p == nil || p.text() != "Not counted on your other PCs yet: none of them has been shown this memory. The ranking uses this PC’s counts." {
			t.Errorf("sentence %v", p)
		}
		if len(c.find("table")) != 0 || len(c.find("details")) != 0 || pillOf(c) != "" {
			t.Error("a table, a popover or a pill on a memory not counted")
		}
	})
	t.Run("no other PC", func(t *testing.T) {
		sh := &metrics.SharedCounts{State: "not_counted", Machines: 0}
		if sharedNode(t, sh, answering, nil).first("#w-memory-shared") != nil {
			t.Error("a card about the other PCs without any")
		}
		if sharedNode(t, &metrics.SharedCounts{State: "unavailable"}, answering, nil).first("#w-memory-shared") != nil {
			t.Error("a card for counts that were never combined, the server answering")
		}
		if sharedNode(t, nil, answering, nil).first("#w-memory-shared") != nil {
			t.Error("a card without the view's counts")
		}
	})
	t.Run("the restore command is quoted", func(t *testing.T) {
		sh := countedShared()
		since := time.Date(2026, 10, 12, 15, 20, 0, 0, time.UTC)
		sh.HiddenElsewhere = &since
		c := sharedCardOf(t, sharedNode(t, sh, answering, func(m *metrics.MemoryPage) { m.Row.ID = "it's; rm -rf" }))
		var cmds []string
		for _, code := range c.find("div.cmd code") {
			cmds = append(cmds, code.Text)
		}
		if !slices.Contains(cmds, `backsight forget --restore 'it'\''s; rm -rf'`) {
			t.Errorf("commands %q", cmds)
		}
	})
	t.Run("sharing off names its setting", func(t *testing.T) {
		for off, want := range map[string]string{"": "feedback.share_ledger", "share_ledger": "feedback.share_ledger", "feedback": "feedback.enabled", "both": "feedback.enabled and feedback.share_ledger"} {
			c := sharedCardOf(t, sharedNode(t, &metrics.SharedCounts{State: "sharing_off", Off: off}, answering, nil))
			if got := allText(c.first("p.small")); !strings.HasPrefix(got, "Sharing is off on this PC ("+want+"): ") {
				t.Errorf("off %q: %q, want it to name %q", off, got, want)
			}
		}
	})
	t.Run("sharing off", func(t *testing.T) {
		c := sharedCardOf(t, sharedNode(t, &metrics.SharedCounts{State: "sharing_off"}, answering, nil))
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "sharing off" || !strings.Contains(p.attr("class"), "st-info") {
			t.Errorf("pill %v", p)
		}
		p := c.first("p.small")
		if p == nil || spaced(allText(p)) != "Sharing is off on this PC (feedback.share_ledger): your other PCs’ counts are not read, and the ranking uses this PC’s alone. Machines on System says how to turn it on." {
			t.Errorf("sentence %v", p)
		}
		if a := p.first("a"); a == nil || a.attr("href") != "/system#machines" || a.text() != "Machines on System" {
			t.Errorf("link %v", a)
		}
		if len(c.find("table")) != 0 {
			t.Error("a table while sharing is off")
		}
	})
	t.Run("not answering", func(t *testing.T) {
		since := detailBase.Add(-5 * time.Hour)
		sh := countedShared()
		c := sharedCardOf(t, sharedNode(t, sh, &metrics.ServerState{OK: false, LastOK: &since}, nil))
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "not answering" || !strings.Contains(p.attr("class"), "st-watch") {
			t.Errorf("pill %v", p)
		}
		p := c.first("p.small")
		if p == nil || p.first("time") == nil || !strings.HasPrefix(spaced(p.text()), "The counts are the ones combined 2 h ago: the memory server has not answered since ") || !strings.HasSuffix(p.text(), "Recall keeps using them.") {
			t.Errorf("sentence %v", p)
		}
		if len(c.find("table")) != 1 {
			t.Error("the counts combined are not shown")
		}
		// Counts never combined, the server silent.
		c = sharedCardOf(t, sharedNode(t, &metrics.SharedCounts{State: "unavailable"}, &metrics.ServerState{OK: false}, nil))
		if p := c.first("p.small"); p == nil || p.text() != "The other PCs’ counts are not available: the memory server is not answering. The ranking uses this PC’s counts." {
			t.Errorf("sentence %v", p)
		}
	})
}

// On a server-only host this PC is not one of the PCs counted: the card's
// chip says the other PCs' number, as the scope line's switch does; where this
// PC counts it adds itself.
func TestMemorySharedChipCountsThisPCOnlyWhenItCounts(t *testing.T) {
	others := []metrics.PCRow{{Machine: "a07be4d1"}, {Machine: "5d21c8e0"}, {Machine: "c84e1f39"}}
	for _, c := range []struct {
		name string
		pcs  *metrics.PCs
		want string
	}{
		{"this PC counts", &metrics.PCs{Count: 4, List: append([]metrics.PCRow{{Machine: "3f9c2a17", This: true}}, others...)}, "all PCs (4)"},
		{"server-only host", &metrics.PCs{Count: 3, List: others}, "all PCs (3)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := detailNode(t, "memory", "/memories/m", views.Params{Limit: 50}, func(d *PageData) {
				d.V["memories"].GeneratedAt = detailBase
				d.Frame.Generated, d.Frame.ServerRead = detailBase, &metrics.ServerState{OK: true}
				d.V["memories"].Scope.PCs = c.pcs
				m := d.V["memories"].Tables.Memory
				m.Row.ID, m.Shared = "7f3a9c21e04b4d0e9a1f6c55b2d18e07", countedShared()
			})
			if chip := sharedCardOf(t, n).first("div.card-head span.chip"); chip == nil || chip.text() != c.want {
				t.Errorf("chip %v, want %q", chip, c.want)
			}
		})
	}
}
