package dashboard

import (
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
)

// SharedCard is the card "Has it helped on your other PCs?" of a memory's
// page: its counts on this PC, on the other PCs and together, or the sentence
// that says why there are none. Notice is a boxed sentence (the memory is
// hidden on another PC), Cmd the command it points to, Text and Foot the
// sentences under the table.
type SharedCard struct {
	Head   Head
	Notice *Notice
	Cmd    string
	Table  *SharedTable
	Text   []Run
	Foot   []Run
}

// SharedTable is the table of the counts: Others is the heading of the column
// of the other PCs ("3 other PCs").
type SharedTable struct {
	Others string
	Rows   []SharedRow
}

// SharedRow is a row of the counts: This, Others and All are what its three
// columns say.
type SharedRow struct {
	Label, Swatch     string
	This, Others, All string
}

// The states of tables.memory.shared.state.
const (
	sharedCounted    = "counted"
	sharedNotCounted = "not_counted"
	sharedOff        = "sharing_off"
	sharedUnavail    = "unavailable"
)

// sharedCard is the card of a memory's counts on the other PCs in its states:
// counted, not counted yet, sharing off, and the server not answering. nil
// when this PC has no other PC (the page is as it was without them).
func (d PageData) sharedCard(r *metrics.Result, m *metrics.MemoryPage) *SharedCard {
	sh := m.Shared
	if sh == nil {
		return nil
	}
	now := r.GeneratedAt
	h := fixedHead("memory-shared", "Has it helped on your other PCs?", "", "")
	c := &SharedCard{Head: h}
	silent := serverSilent(d.Frame)
	notAnswering := "the memory server is not answering"
	if since := serverSince(d.Frame); !since.IsZero() {
		notAnswering = "the memory server has not answered since " + format.Clock(now, since, d.Loc)
	}
	if silent {
		c.Head.Pill = Word{"not answering", "st-watch"}
	}
	switch sh.State {
	case sharedOff:
		c.Head.Pill = Word{"sharing off", "st-info"}
		c.Text = []Run{{Text: "Sharing is off on this PC ("}}
		switch sh.Off {
		case metrics.OffFeedback:
			c.Text = append(c.Text, Run{Code: "feedback.enabled"})
		case metrics.OffBoth:
			c.Text = append(c.Text, Run{Code: "feedback.enabled"}, Run{Text: " and "}, Run{Code: "feedback.share_ledger"})
		default:
			c.Text = append(c.Text, Run{Code: "feedback.share_ledger"})
		}
		c.Text = append(c.Text, Run{Text: "): your other PCs’ counts are not read, and the ranking uses this PC’s alone. "},
			Run{Text: "Machines on System", Href: machinesHref}, Run{Text: " says how to turn it on."})
		return c
	case sharedUnavail:
		if !silent {
			return nil // never combined, or the file is old: nothing to say
		}
		c.Text = []Run{{Text: "The other PCs’ counts are not available: " + notAnswering + ". The ranking uses this PC’s counts."}}
		return c
	case sharedNotCounted:
		if sh.Machines == 0 {
			return nil
		}
		c.Text = []Run{{Text: "Not counted on your other PCs yet: none of them has been shown this memory. The ranking uses this PC’s counts."}}
		if silent {
			c.Text = append(c.Text, Run{Text: " The memory server is not answering: " + notAnswering + "."})
		}
		return c
	case sharedCounted:
	default:
		return nil
	}

	n := sh.Machines
	c.Head.PCs = &PCsChip{Text: "all PCs (" + strconv.Itoa(n+1) + ")", Title: "This PC’s counts and the counts your other PCs share"}
	c.Head.CLI = "backsight stats memories " + quoteArg(m.Row.ID) + " --machines all"
	c.Head.Info = Info{
		Label: "About the other PCs’ counts", Name: "Counts from your other PCs",
		What: "How often Claude used this memory, or was shown it and ignored it, on each of your PCs, added up. Recent uses weigh more, as for this PC.",
		Rows: []InfoRow{
			{Term: "Where from", Runs: []Run{{Text: "The copies of their counts your other PCs share through the memory server ("}, {Code: "feedback.share_ledger"}, {Text: "), combined on this PC."}}},
			{Term: "Used for", Runs: []Run{{Text: "The learned ranking: on this PC, recall weighs the “All PCs” column."}}},
		},
		Term: "shared-combined · credits · misses",
	}
	l := d.L
	pct := func(rate *float64) string {
		if rate == nil {
			return dash
		}
		return l.Percent(*rate)
	}
	hiddenHere := m.Row.State == "hidden"
	hidden := SharedRow{Label: "Hidden from recall", This: "no", Others: "no", All: "no"}
	switch {
	case hiddenHere:
		hidden.This, hidden.Others, hidden.All = "yes", dash, "yes" // whether the others hide it is not known
	case sh.HiddenElsewhere != nil:
		hidden.Others, hidden.All = "yes", "yes"
		t := sh.HiddenElsewhere.In(zoneOf(d.Loc))
		since := format.Time{Text: shortDay(t.Format(time.DateOnly), now), Datetime: t.Format(time.RFC3339), Title: t.Format("2006-01-02 15:04")}
		c.Head.Pill = Word{"hidden elsewhere", "st-watch"}
		c.Notice = &Notice{Icon: "watch", Class: "st-watch", Runs: []Run{{Text: "Hidden from recall on one of your other PCs since "}, {Time: since},
			{Text: ". Recall skips it on this PC too. To use it here again:"}}}
		c.Cmd = "backsight forget --restore " + quoteArg(m.Row.ID)
	}
	c.Table = &SharedTable{Others: plural(n, "other PC", "other PCs"), Rows: []SharedRow{
		{Label: "Used by Claude", Swatch: "f-used", This: l.Fixed(sh.This.Used, 1), Others: l.Fixed(sh.Others.Used, 1), All: l.Fixed(sh.All.Used, 1)},
		{Label: "Shown, then ignored", Swatch: "f-ignored", This: l.Fixed(sh.This.Ignored, 1), Others: l.Fixed(sh.Others.Ignored, 1), All: l.Fixed(sh.All.Ignored, 1)},
		{Label: "Used when judged", This: pct(sh.This.Rate), Others: pct(sh.Others.Rate), All: pct(sh.All.Rate)},
		hidden,
	}}
	if silent {
		c.Text = []Run{{Text: "The counts are the ones combined "}}
		if sh.CombinedAt != nil {
			c.Text = append(c.Text, Run{Time: format.Rel(now, *sh.CombinedAt, d.Loc)})
		} else {
			c.Text = append(c.Text, Run{Text: "earlier"})
		}
		c.Text = append(c.Text, Run{Text: ": " + notAnswering + ". Recall keeps using them."})
	}
	if sh.CombinedAt != nil {
		c.Foot = []Run{{Text: "Combined "}, {Time: format.Rel(now, *sh.CombinedAt, d.Loc)}, {Text: " from the copies the other PCs share: memory ids and numbers only. The page shows their sum, not which PC. Shows, the timeline and the ranks below are this PC’s."}}
	} else {
		c.Foot = []Run{{Text: "From the copies the other PCs share: memory ids and numbers only. The page shows their sum, not which PC. Shows, the timeline and the ranks below are this PC’s."}}
	}
	return c
}

// zoneOf is loc, UTC when none.
func zoneOf(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}
