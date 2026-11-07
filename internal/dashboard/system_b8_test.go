package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// machinesNow is the moment the Machines boards are read at.
var machinesNow = time.Date(2026, 10, 30, 15, 42, 0, 0, time.UTC)

// fourMachines is the table of the design's board: this PC, three others (one
// gone quiet, one with no activity in the window) and one document set aside
// whose name reads.
func fourMachines() *metrics.Machines {
	ago := func(d time.Duration) *time.Time { t := machinesNow.Add(-d); return &t }
	fetched := machinesNow.Add(-2 * time.Hour)
	return &metrics.Machines{Sharing: true, FetchedAt: &fetched, Rows: []metrics.MachineRow{
		{Machine: "3f9c2a17", This: true, OS: "linux", FirstSeen: "2026-09-02", LastSent: ago(2 * time.Hour), BinaryVersion: "v0.1.17-m7", Active: true},
		{Machine: "a07be4d1", OS: "darwin", FirstSeen: "2026-10-10", LastSent: ago(5 * time.Hour), BinaryVersion: "v0.1.17-m7", Active: true},
		{Machine: "5d21c8e0", OS: "windows", FirstSeen: "2026-10-28", LastSent: ago(time.Hour), BinaryVersion: "v0.1.17-m7"},
		{Machine: "c84e1f39", OS: "linux", FirstSeen: "2026-07-18", LastSent: ago(9*24*time.Hour + time.Hour), BinaryVersion: "v0.1.16-m7", Until: ptrS("2026-10-21"), Stale: true, Active: true},
	}, Ignored: []metrics.IgnoredRow{{Machine: "e19b77a0", Reason: "not_valid"}}}
}

// machinesNode draws System with the table of PCs t and the server answering
// as read.
func machinesNode(t *testing.T, tab *metrics.Machines, read *metrics.ServerState) *node {
	t.Helper()
	return pageNode(t, "system", false, func(d *PageData) {
		r := d.V["system"]
		r.GeneratedAt = machinesNow
		r.Tables.Machines = tab
		d.Frame.Generated, d.Frame.ServerRead = machinesNow, read
	})
}

// machinesCardOf is the Machines card of n.
func machinesCardOf(t *testing.T, n *node) *node { return cardOf(t, n, "w-machines") }

// cellsOf are the texts of the cells of each row of the card's table.
func cellsOf(c *node) [][]string {
	var out [][]string
	for _, tr := range c.find("tbody tr") {
		out = append(out, texts(tr.find("td")))
	}
	return out
}

func TestMachinesCardStates(t *testing.T) {
	answering := &metrics.ServerState{OK: true}
	pill := func(c *node) string {
		if p := c.first("div.card-head span.pill"); p != nil {
			return p.text()
		}
		return ""
	}
	t.Run("the PCs", func(t *testing.T) {
		n := machinesNode(t, fourMachines(), answering)
		c := machinesCardOf(t, n)
		if c.attr("id") != "machines" {
			t.Errorf("the section is not the target of /system#machines: id %q", c.attr("id"))
		}
		if got := len(n.find("#machines")); got != 1 {
			t.Errorf("%d elements with id machines", got)
		}
		if got := cardIDs(n); !slices.Equal(got[:4], []string{"w-server-reach", "w-this-pc", "w-attention", "w-machines"}) {
			t.Errorf("cards %v: the Machines card is not right after the first row", got)
		}
		if h := c.first("h2"); h.text() != "Which PCs share their activity here?" {
			t.Errorf("title %q", h.text())
		}
		if chips := texts(c.find("div.card-head span.chip")); !slices.Equal(chips, []string{"server"}) {
			t.Errorf("chips %q", chips)
		}
		if aside := c.first("div.card-head span.muted.small"); aside == nil || aside.text() != "4 PCs · 1 document ignored" {
			t.Errorf("aside %v", aside)
		}
		if pill(c) != "" {
			t.Errorf("pill %q on the plain state", pill(c))
		}
		if got, want := texts(c.find("thead th")), []string{"PC", "System", "First seen", "Last sent", "backsight", "In “all PCs”"}; !slices.Equal(got, want) {
			t.Errorf("headings %q, want %q", got, want)
		}
		want := [][]string{
			{"3f9c2a17this PC", "Linux", "Sep 2", "2 h ago", "v0.1.17-m7", "counted its own figures, live"},
			{"a07be4d1", "macOS", "Oct 10", "5 h ago", "v0.1.17-m7", "counted"},
			{"5d21c8e0", "Windows", "Oct 28", "1 h ago", "v0.1.17-m7", "counted no activity in this window"},
			{"c84e1f39", "Linux", "Jul 18", "9 d ago", "v0.1.16-m7", "up to Oct 21"},
			{"e19b77a0", "—", "—", "—", "—", "ignored: not valid"},
		}
		got := cellsOf(c)
		for i := range got {
			got[i][0] = strings.ReplaceAll(got[i][0], " ", "")
		}
		for i := range want {
			want[i][0] = strings.ReplaceAll(want[i][0], " ", "")
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
		if cl := rows[4].attr("class"); cl != "ign" {
			t.Errorf("the ignored row's class %q", cl)
		}
		if q := rows[3].find("td")[3].first("span.st-watch"); q == nil || q.first("time") == nil {
			t.Error("the last send of a PC gone quiet is not drawn as a warning")
		}
		if q := rows[3].find("td")[5].first("span.st-watch"); q == nil {
			t.Error("the stale PC's state is not a warning")
		}
		if got, want := texts(c.find("span.idc")), []string{"3f9c2a17", "a07be4d1", "5d21c8e0", "c84e1f39", "e19b77a0"}; !slices.Equal(got, want) {
			t.Errorf("short ids %q, want %q, one per row in order", got, want)
		}
		if tm := rows[3].find("td")[3].first("time"); tm == nil || !strings.HasPrefix(tm.attr("title"), "2026-10-21") {
			t.Errorf("the date of a quiet PC's last send is not in the title: %v", tm)
		}
		info := c.first("details.info")
		if dts := texts(info.find("dt")); !slices.Equal(dts, []string{"What", "Not here", "Privacy"}) {
			t.Errorf("popover rows %q", dts)
		}
		if !strings.Contains(allText(info), "feedback.share_ledger") || !strings.Contains(allText(info), "backsight:metrics:<machine_id>") {
			t.Errorf("popover %q", allText(info))
		}
		if cmd := c.first("details.cli code"); cmd == nil || cmd.Text != "backsight stats system" {
			t.Errorf("CLI %v", cmd)
		}
		foot := c.find("p.xs.muted")
		if len(foot) != 1 || foot[0].first("time") == nil || !strings.HasPrefix(foot[0].text(), "Fetched from the memory server 2 h ago, every 6 h.") ||
			!strings.Contains(foot[0].text(), "A PC that has not sent for 30 days drops off this list.") {
			t.Errorf("foot %v", texts(foot))
		}
	})
	t.Run("ignored documents show only an id and words", func(t *testing.T) {
		tab := fourMachines()
		tab.Ignored = []metrics.IgnoredRow{
			{Machine: "e19b77a0", Reason: "too_large"},
			{Machine: "<script>alert(1)</script>", Reason: "<b>not a reason</b>"},
			{Machine: "", Reason: "unknown_version"},
		}
		c := machinesCardOf(t, machinesNode(t, tab, answering))
		var got [][2]string
		for _, tr := range c.find("tr.ign") {
			cells := tr.find("td")
			if len(cells) != 6 {
				t.Fatalf("an ignored row has %d cells", len(cells))
			}
			got = append(got, [2]string{cells[0].text(), cells[5].text()})
		}
		want := [][2]string{{"e19b77a0", "ignored: over the 1 MiB cap"}, {"unknown PC", "ignored: not valid"}, {"unknown PC", "ignored: written by a newer backsight: update this PC"}}
		if !slices.Equal(got, want) {
			t.Errorf("ignored rows %q, want %q", got, want)
		}
		if body := allText(c); strings.Contains(body, "script") || strings.Contains(body, "not a reason") {
			t.Errorf("a name or a reason from the document reached the page: %q", body)
		}
		if aside := c.first("div.card-head span.muted.small"); aside.text() != "4 PCs · 3 documents ignored" {
			t.Errorf("aside %q", aside.text())
		}
	})
	t.Run("this PC only", func(t *testing.T) {
		tab := fourMachines()
		tab.Rows, tab.Ignored = tab.Rows[:1], nil
		c := machinesCardOf(t, machinesNode(t, tab, answering))
		if aside := c.first("div.card-head span.muted.small"); aside == nil || aside.text() != "this PC only" {
			t.Errorf("aside %v", aside)
		}
		if got, want := texts(c.find("thead th")), []string{"PC", "System", "Last sent", "backsight"}; !slices.Equal(got, want) {
			t.Errorf("headings %q, want %q", got, want)
		}
		if got := cellsOf(c); len(got) != 1 || got[0][0] != "3f9c2a17 this PC" && got[0][0] != "3f9c2a17this PC" || got[0][1] != "Linux" || got[0][2] != "2 h ago" || got[0][3] != "v0.1.17-m7" {
			t.Errorf("rows %q", got)
		}
		want := "Only this PC shares its activity on this server. Your other PCs appear here after their first daily send, with sharing on (the default). The scope line shows no switch until then."
		if p := c.first("p.small.sub"); p == nil || p.text() != want {
			t.Errorf("sentence %v", p)
		}
		if len(c.find("p.xs.muted")) != 0 || pill(c) != "" {
			t.Error("a foot or a pill on this PC only")
		}
	})
	t.Run("sharing off", func(t *testing.T) {
		c := machinesCardOf(t, machinesNode(t, &metrics.Machines{Sharing: false, Rows: fourMachines().Rows[:1]}, answering))
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "sharing off" || !strings.Contains(p.attr("class"), "st-info") {
			t.Errorf("pill %v", p)
		}
		if len(c.find("table")) != 0 {
			t.Error("a table of PCs while sharing is off")
		}
		items := c.find("li.att-item")
		if len(items) != 1 {
			t.Fatalf("%d items", len(items))
		}
		it := items[0]
		if w := it.first("span.pill"); w.text() != "info" || !strings.Contains(w.attr("class"), "st-info") {
			t.Errorf("item pill %q", w.attr("class"))
		}
		body := allText(it.first("p"))
		for _, want := range []string{"This PC does not share its activity: feedback.share_ledger is off.", "set share_ledger = true under [feedback] in config.toml (backsight paths shows where it is), then send now:"} {
			if !strings.Contains(body, want) {
				t.Errorf("the item %q lacks %q", body, want)
			}
		}
		if cmds := texts(it.find("div.cmd code")); !slices.Equal(cmds, []string{"backsight jobs tick"}) {
			t.Errorf("commands %q", cmds)
		}
		if w := it.first("details.why summary"); w == nil || w.text() != "Why this matters" || !strings.Contains(allText(it.first("details.why")), "Off is a valid choice and nothing is broken.") {
			t.Errorf("why %v", w)
		}
		if note := c.first("p.xs.muted"); note == nil || !strings.Contains(note.text(), "it is a choice, not a fault") {
			t.Errorf("note %v", note)
		}
	})
	t.Run("not answering", func(t *testing.T) {
		since := machinesNow.Add(-6*time.Hour - 30*time.Minute) // 09:12 UTC, 10:12 in the page's zone (Paris, UTC+1)
		tab := fourMachines()
		tab.Rows, tab.Ignored = tab.Rows[:3], nil
		fetched := time.Date(2026, 10, 30, 7, 40, 0, 0, time.UTC)
		tab.FetchedAt = &fetched
		c := machinesCardOf(t, machinesNode(t, tab, &metrics.ServerState{OK: false, LastOK: &since}))
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "not answering" || !strings.Contains(p.attr("class"), "st-watch") {
			t.Errorf("pill %v", p)
		}
		if s := c.first("div.state"); s == nil || spaced(s.text()) != "Memory server not answering since 10:12. The list is the one fetched at 08:40." || !strings.Contains(s.attr("class"), "st-watch") {
			t.Errorf("state %v", texts(c.find("div.state")))
		}
		if got, want := texts(c.find("thead th")), []string{"PC", "System", "Last sent", "backsight"}; !slices.Equal(got, want) {
			t.Errorf("headings %q, want %q", got, want)
		}
		if len(cellsOf(c)) != 3 {
			t.Errorf("rows %q: the list is the one fetched", cellsOf(c))
		}
		if f := c.first("p.xs.muted"); f == nil || f.text() != "This PC’s figures wait and are sent once the server answers; nothing is lost." {
			t.Errorf("foot %v", f)
		}
	})
	t.Run("no table without the view's table", func(t *testing.T) {
		n := machinesNode(t, nil, answering)
		if n.first("#machines") != nil {
			t.Error("a Machines card without a table of PCs")
		}
	})
}

// The sharing-off item is on System, in "What needs a look here?" and in the
// card, and never in the Overview's "What should I do?": it is a choice.
func TestSharingOffItemOnSystemOnly(t *testing.T) {
	off := &metrics.Machines{Sharing: false, Rows: fourMachines().Rows[:1]}
	n := machinesNode(t, off, &metrics.ServerState{OK: true})
	look := cardOf(t, n, "w-attention")
	var items []*node
	for _, it := range look.find("li.att-item") {
		if strings.Contains(allText(it), "This PC does not share its activity") {
			items = append(items, it)
		}
	}
	if len(items) != 1 || items[0].first("span.pill").text() != "info" {
		t.Fatalf("%d sharing-off items in What needs a look here?", len(items))
	}
	if last := look.find("li.att-item"); last[len(last)-1] != items[0] {
		t.Error("the item is not after the others")
	}
	if cmds := texts(items[0].find("div.cmd code")); !slices.Equal(cmds, []string{"backsight jobs tick"}) {
		t.Errorf("commands %q", cmds)
	}
	// Sharing on: no item, and the count of warnings is the same: an info item is not one.
	on := machinesNode(t, fourMachines(), &metrics.ServerState{OK: true})
	lookOn := cardOf(t, on, "w-attention")
	if strings.Contains(allText(lookOn), "does not share") {
		t.Error("the item shows with sharing on")
	}
	asideOf := func(c *node) string {
		if a := c.first("div.card-head span.muted.small"); a != nil {
			return a.text()
		}
		return ""
	}
	if asideOf(look) != asideOf(lookOn) {
		t.Errorf("a choice is counted: aside %q with sharing off, %q with it on", asideOf(look), asideOf(lookOn))
	}
	// The Overview carries the item nowhere (a guard against a later addition:
	// no change of this code reddens it today), even with the table in hand.
	ov := pageNode(t, "overview", false, func(d *PageData) {
		for _, r := range d.V {
			r.Tables.Machines = off
		}
	})
	if strings.Contains(allText(ov), "does not share its activity") {
		t.Error("the sharing-off item is on the Overview")
	}
}

func TestThisPCCardSaysItShares(t *testing.T) {
	rowsOf := func(t *testing.T, tab *metrics.Machines) [][2]string {
		t.Helper()
		return dlOf(cardOf(t, machinesNode(t, tab, &metrics.ServerState{OK: true}), "w-this-pc"))
	}
	t.Run("shares", func(t *testing.T) {
		n := machinesNode(t, fourMachines(), &metrics.ServerState{OK: true})
		c := cardOf(t, n, "w-this-pc")
		got := dlOf(c)
		if got[1][0] != "Machine" || strings.ReplaceAll(got[1][1], " ", "") != "3f9c2a17Linux·sinceSep2" {
			t.Errorf("machine row %q", got[1])
		}
		if got[2] != [2]string{"Shares its activity", "yes, last sent 2 h ago"} {
			t.Errorf("sharing row %q", got[2])
		}
		if idc := c.first("dl.dl span.idc"); idc == nil || idc.text() != "3f9c2a17" || idc.attr("title") == "" {
			t.Errorf("id %v", idc)
		}
		if tm := c.find("dl.dl dd")[2].first("time"); tm == nil || tm.attr("datetime") == "" {
			t.Error("the last send is not a time element")
		}
	})
	t.Run("never sent", func(t *testing.T) {
		tab := fourMachines()
		tab.Rows[0].LastSent = nil
		if got := rowsOf(t, tab); got[2] != [2]string{"Shares its activity", "yes, nothing sent yet"} {
			t.Errorf("sharing row %q", got[2])
		}
	})
	t.Run("sharing off", func(t *testing.T) {
		if got := rowsOf(t, &metrics.Machines{Sharing: false, Rows: fourMachines().Rows[:1]}); got[2] != [2]string{"Shares its activity", "no"} {
			t.Errorf("sharing row %q", got[2])
		}
	})
	t.Run("a hostile id is not drawn", func(t *testing.T) {
		tab := fourMachines()
		tab.Rows[0].Machine = "<b>x</b>"
		c := cardOf(t, machinesNode(t, tab, &metrics.ServerState{OK: true}), "w-this-pc")
		if c.first("span.idc") != nil || strings.Contains(allText(c), "<b>") {
			t.Errorf("an id that is not one reached the card: %q", allText(c))
		}
	})
	t.Run("this PC is not in the table", func(t *testing.T) {
		tab := fourMachines()
		tab.Rows = tab.Rows[1:]
		for _, r := range rowsOf(t, tab) {
			if r[0] == "Machine" || r[0] == "Shares its activity" {
				t.Errorf("row %q without this PC", r)
			}
		}
	})
}

// Every ignored reason has a word; the pages draw the shared one.
func TestMachinesCardUsesTheSharedWords(t *testing.T) {
	for reason, word := range views.IgnoredWords {
		tab := fourMachines()
		tab.Ignored = []metrics.IgnoredRow{{Machine: "e19b77a0", Reason: reason}}
		c := machinesCardOf(t, machinesNode(t, tab, &metrics.ServerState{OK: true}))
		if got := c.first("tr.ign span.st").text(); got != "ignored: "+word {
			t.Errorf("%s reads %q, want %q", reason, got, "ignored: "+word)
		}
	}
}

// spaced is s with each run of white space as one space.
func spaced(s string) string { return strings.Join(strings.Fields(s), " ") }

// The sharing-off item names the setting that is off, and its remedy sets that
// one: feedback.enabled, feedback.share_ledger or both.
func TestSharingOffNamesTheSettingThatIsOff(t *testing.T) {
	cases := []struct{ off, named, set string }{
		{metrics.OffLedger, "feedback.share_ledger is off.", "set share_ledger = true under [feedback]"},
		{metrics.OffFeedback, "feedback.enabled is off.", "set enabled = true under [feedback]"},
		{metrics.OffBoth, "feedback.enabled and feedback.share_ledger are off.", "set enabled = true and share_ledger = true under [feedback]"},
	}
	for _, c := range cases {
		t.Run(c.off, func(t *testing.T) {
			n := machinesNode(t, &metrics.Machines{Sharing: false, Off: c.off, Rows: fourMachines().Rows[:1]}, &metrics.ServerState{OK: true})
			for _, card := range []*node{cardOf(t, n, "w-attention"), machinesCardOf(t, n)} {
				var body string
				for _, it := range card.find("li.att-item") {
					if b := allText(it.first("p")); strings.Contains(b, "does not share") {
						body = b
					}
				}
				if !strings.Contains(body, c.named) || !strings.Contains(body, c.set) {
					t.Errorf("the item %q lacks %q or %q", body, c.named, c.set)
				}
				if c.off == metrics.OffFeedback && strings.Contains(body, "share_ledger") {
					t.Errorf("the item blames share_ledger when it is on: %q", body)
				}
			}
		})
	}
}

// A host that runs the server only is not a PC of the list: with no other PC
// sent yet, the card does not say "this PC only".
func TestMachinesCardServerOnlyHostWithNoPC(t *testing.T) {
	tab := fourMachines()
	tab.Rows, tab.Ignored = nil, nil
	c := machinesCardOf(t, machinesNode(t, tab, &metrics.ServerState{OK: true}))
	if aside := c.first("div.card-head span.muted.small"); aside == nil || aside.text() != "no PC yet" {
		t.Errorf("aside %v", aside)
	}
	if p := c.first("p.small.sub"); p == nil || !strings.HasPrefix(p.text(), "No PC shares its activity on this server yet. This PC runs the server only") {
		t.Errorf("sentence %v", p)
	}
	if strings.Contains(allText(c), "this PC only") || strings.Contains(allText(c), "Only this PC") || len(c.find("table")) != 0 {
		t.Errorf("the card says this PC shares, or draws a table of nothing: %q", allText(c))
	}
}
