package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The text of `stats <view>`: plain words for the labels of §9, the same
// numbers as --json. Everything printed here is a field of the Result, which
// holds no free text (the canary test reads this output too).

// The words and number formats are shared with the dashboard: internal/views.
var (
	viewQuestions    = views.Questions
	familyHeadings   = views.FamilyHeadings
	axisNames        = views.AxisNames
	axisStateWords   = views.AxisStateWords
	noDataWords      = views.NoDataWords
	serverErrorWords = views.ServerErrorWords
	emptyWords       = views.EmptyWords
	kindWords        = views.KindWords
	sentWords        = views.SentWords
	outcomeWords     = views.OutcomeWords
	checkWords       = views.CheckWords
	verdictWords     = views.VerdictWords
	unitWords        = views.UnitWords
	pathWords        = views.PathWords
	memoryStateWords = views.MemoryStateWords

	headingOf        = views.Heading
	labelOf          = views.Label
	metricStatusText = views.StatusText
	deltaText        = views.DeltaText
	formatValue      = views.FormatValue
	percentOf        = views.Percent
	thousands        = views.Thousands
	grouped          = views.Grouped
	plainText        = views.Plain
	clipText         = views.Clip
	idText           = views.ShortID
	ageText          = views.Age
	word             = views.Word
	metricDetails    = views.MetricDetails
	pricesHint       = views.PricesHint
)

// viewText is the text of a view's result; dev adds the developer metrics.
func viewText(r metrics.Result, dev bool) string {
	var b strings.Builder
	loc := r.Window.Loc
	if loc == nil {
		loc = time.UTC
	}
	q := viewQuestions[r.View]
	if q == "" {
		q = r.View
	}
	fmt.Fprintf(&b, "%s (%s → %s, %s)\n", q, r.Window.Since.In(loc).Format("2006-01-02 15:04"), r.Window.Until.In(loc).Format("2006-01-02 15:04"), r.Window.TZ)
	if r.Scope.Project != "" {
		fmt.Fprintf(&b, "Project: %s\n", clipText(r.Scope.Project))
	}
	if s := r.Server; s != nil && !s.OK {
		why := serverErrorWords[s.Error]
		if why == "" {
			why = "not answering"
		}
		fmt.Fprintf(&b, "Memory server: %s. The numbers that come from it are missing.", why)
		if s.LastOK != nil {
			fmt.Fprintf(&b, " Last answer %s.", s.LastOK.In(loc).Format("2006-01-02 15:04"))
		}
		b.WriteByte('\n')
	}
	if v := r.Verdict; v != nil {
		b.WriteString("\nVerdict (last " + strconv.Itoa(v.WindowDays) + " days)\n")
		for _, a := range axisNames {
			ax, ok := v.Axes[a.Key]
			if !ok {
				continue
			}
			line := fmt.Sprintf("%-8s %s", a.Name, axisStateWords[ax.State])
			if len(ax.Reasons) > 0 {
				line += " — " + ax.Reasons[0].Text
			}
			// Right judged on 28 days: its cause, then that note.
			if n := len(ax.Reasons); n > 1 && ax.WindowDays == 28 && ax.Reasons[n-1].Severity == metrics.SeverityInfo {
				line += " " + ax.Reasons[n-1].Text
			}
			b.WriteString(line + "\n")
		}
	}
	if s := r.Summary; s != nil {
		b.WriteString("\n" + summaryText(*s, r.Window.Days) + "\n")
	}
	metricsText(&b, r, dev)
	if len(r.Attention) > 0 {
		b.WriteString("\nNeeds attention\n")
		for i, it := range r.Attention {
			fmt.Fprintf(&b, "%d. %s\n", i+1, it.Text)
			if it.Command != "" {
				fmt.Fprintf(&b, "   Run: %s\n", it.Command)
			}
		}
		if r.AttentionMore > 0 {
			fmt.Fprintf(&b, "%d more: backsight stats system\n", r.AttentionMore)
		}
	}
	if len(r.Info) > 0 {
		b.WriteString("\nWorth a look\n")
		for _, it := range r.Info {
			label := it.Metric
			if d, ok := metrics.DefOf(it.Metric); ok {
				label = d.Label
			}
			fmt.Fprintf(&b, "%s: %s  (%s)\n", label, grouped(it.Count), it.Command)
		}
	}
	tablesText(&b, r, loc)
	if t := r.Text; t != nil && t.Reason != "" {
		why := noDataWords[t.Reason]
		if why == "" {
			why = "the memory server did not answer"
		}
		fmt.Fprintf(&b, "\nMemory %s: its text is not available (%s).\n", idText(t.ID), why)
	} else if t != nil {
		fmt.Fprintf(&b, "\nMemory %s, as read from the server\n%s\n", idText(t.ID), plainText(t.Text, "\n\t"))
	}
	b.WriteString("\n" + footer(r) + "\n")
	return b.String()
}

// footer names the scope of the numbers. With all PCs it counts them, says
// how far the other PCs' figures reach, how many documents were set aside
// and that a project filter leaves the other PCs' figures whole.
func footer(r metrics.Result) string {
	machines := "this PC"
	if r.Scope.Machines == "all" {
		machines = "all PCs on this server"
		if p := r.Scope.PCs; p != nil {
			machines += fmt.Sprintf(" (%d)", p.Count)
			if p.Until != nil {
				machines += " · other PCs up to " + *p.Until
			}
			if p.Ignored > 0 {
				machines += " · " + countOf(int64(p.Ignored), "document", "documents") + " ignored"
			}
		}
		if r.Scope.OthersUnfiltered {
			machines += " · other PCs not filtered"
		}
	}
	server := r.Scope.Server
	if server == "" {
		server = "(no server)"
	}
	return fmt.Sprintf("Activity on %s · memory on %s · --json for every field", machines, server)
}

func summaryText(s metrics.Summary, days int) string {
	cost := "extraction cost unknown: " + pricesHint
	if s.ExtractionCost != nil {
		cost = "extraction cost " + fmt.Sprintf("%.2f", *s.ExtractionCost) + " " + s.Currency
	} else if s.PriceSet {
		cost = "extraction cost not known"
	}
	return fmt.Sprintf("In this window (%s): %s got memory, Claude used it in %s, about %s tokens added; %s (server, all PCs).",
		countOf(int64(days), "day", "days"), countOf(s.PromptsWithMemory, "prompt", "prompts"), countOf(s.TurnsWithMemoryUsed, "turn", "turns"),
		thousands(float64(s.TokensApprox)), cost)
}

// metricsText prints the metrics by family, in the catalog's order.
func metricsText(b *strings.Builder, r metrics.Result, dev bool) {
	for _, fh := range familyHeadings {
		var lines []string
		for _, d := range metrics.Catalog {
			m, ok := r.Metrics[d.ID]
			if !ok || headingOf(d) != fh.Family || (d.Dev && !dev) {
				continue
			}
			lines = append(lines, metricLine(d, m, r.Scope.ServerOK, staysThisPC(r.Scope, d)))
			if m.Status == metrics.StatusNoData {
				continue
			}
			for _, x := range metricDetails(d, m, r.Window.Loc) {
				lines = append(lines, "  "+x)
			}
			loc := r.Window.Loc
			if loc == nil {
				loc = time.UTC
			}
			for _, x := range blockDetails(d, m, loc) {
				lines = append(lines, "  "+x)
			}
		}
		if len(lines) > 0 {
			b.WriteString("\n" + fh.Heading + "\n" + strings.Join(lines, "\n") + "\n")
		}
	}
}

// handful is the sample size under which a rate is shown as its counts.
const handful = 10

// metricLine is `label  value  (n=…, status, delta vs previous)`.
func metricLine(d metrics.Def, m metrics.Metric, serverUp, thisPC bool) string {
	value := formatValue(d, m)
	if m.Status == metrics.StatusNoData {
		value = "—"
	}
	switch {
	case m.Status == metrics.StatusTooFew && slices.Contains(m.Tags, "average") && m.Value != nil:
		// The average over every PC's sessions is a figure of its own: only
		// the status (judged on this PC's high value) is too few.
	case m.Status == metrics.StatusTooFew:
		value = "—"
		if d.Unit == "ratio" && m.Num != nil && m.Den != nil {
			value = fmt.Sprintf("%s of %s", grouped(*m.Num), grouped(*m.Den))
		}
	case m.Status != metrics.StatusNoData && d.Unit == "ratio" && m.Num != nil && m.Den != nil && *m.Den < handful:
		value = fmt.Sprintf("%s of %s", grouped(*m.Num), grouped(*m.Den)) // a percentage over so few would be a guess
	case m.Status != metrics.StatusNoData && d.ID == "server.reach" && m.Value != nil:
		value = "no" // what the view's header says, not the skipped-prompt count
		if serverUp {
			value = "yes"
		}
	case m.Status != metrics.StatusNoData && d.ID == "recall.empty_reasons" && m.Value != nil:
		value = countOf(int64(*m.Value), "prompt", "prompts")
	case m.Status != metrics.StatusNoData && d.ID == "doctor.check" && m.Value != nil:
		value = grouped(int64(*m.Value)) + " failed"
	case m.Status != metrics.StatusNoData && d.ID == "jobs.last_run" && m.Value != nil:
		value = ageText(int64(*m.Value)) // the last tick's age, as the jobs table prints it
	}
	var parts []string
	if slices.Contains(m.Tags, "approximate") {
		parts = append(parts, "approximate") // a range read from the PCs' buckets
	}
	if m.Den != nil {
		n := "n=" + grouped(*m.Den)
		if slices.Contains(m.Tags, "average") {
			n += " sessions"
		}
		parts = append(parts, n)
	}
	if thisPC {
		parts = append(parts, "this PC") // the other PCs send nothing the figure could add up
	}
	status := metricStatusText(d, m)
	if slices.Contains(m.Tags, "status_this_pc") {
		status = "status on this PC: " + status // the other PCs send no figure the status could use
	}
	parts = append(parts, status)
	if slices.Contains(m.Tags, "unreliable") {
		parts = append(parts, "unreliable")
	}
	if slices.Contains(m.Tags, "judged_28d") {
		parts = append(parts, "judged on the last 28 days")
	}
	if slices.Contains(m.Tags, "not_filtered") {
		parts = append(parts, "not filtered by project")
	}
	for _, tag := range m.Tags {
		if w, ok := views.TagWords[tag]; ok {
			parts = append(parts, w)
		}
	}
	if dl := deltaText(d, m); dl != "" {
		parts = append(parts, dl)
	}
	return fmt.Sprintf("%-44s %-24s (%s)", labelOf(d), value, strings.Join(parts, ", "))
}

// thisPCBesideMerged are the figures that sit beside merged ones on the Cost
// view and stay this PC's: a prompt's context and the search's time. The text
// marks them as the page's icon does, so a reader does not take them for the
// all-PCs figures printed on the lines around them.
var thisPCBesideMerged = []string{"context.per_prompt", "search.latency"}

// staysThisPC says the metric is marked "this PC" in a result that adds the
// PCs up.
func staysThisPC(sc metrics.Scope, d metrics.Def) bool {
	return sc.Machines == "all" && sc.PCs != nil && slices.Contains(thisPCBesideMerged, d.ID)
}

// countOf is n with its noun, singular for 1.
func countOf(n int64, one, many string) string { return views.CountOf(n, one, many) }

// table writes aligned columns.
func table(b *strings.Builder, head []string, rows [][]string) {
	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(head, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
	for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		b.WriteString(strings.TrimRight(l, " ") + "\n")
	}
}

// section writes a heading and its table, or `none` for no rows.
func section(b *strings.Builder, heading string, head []string, rows [][]string) {
	b.WriteString("\n" + heading + "\n")
	if len(rows) == 0 {
		b.WriteString("none\n")
		return
	}
	table(b, head, rows)
}

func whenText(t time.Time, loc *time.Location) string { return views.When(t, loc) }

func whenPtr(t *time.Time, loc *time.Location) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return whenText(*t, loc)
}

func n64(v int64) string { return grouped(v) }

func sessionRows(rows []metrics.SessionRow, loc *time.Location) [][]string {
	var out [][]string
	for _, s := range rows {
		out = append(out, []string{idText(s.ID), clipText(s.Project), whenText(s.Start, loc), n64(s.Prompts), n64(s.PromptsWithMemory),
			n64(s.Shown), n64(s.Used), n64(s.Wrong), thousands(float64(s.ContextChars)), word(sentWords, s.Sent)})
	}
	return out
}

var sessionHead = []string{"Session", "Project", "Started", "Prompts", "With memory", "Shown", "Used", "Wrong", "Context chars", "Sent"}

// tablesText prints whichever tables the view filled.
func tablesText(b *strings.Builder, r metrics.Result, loc *time.Location) {
	t := r.Tables
	if a := t.Activity; a != nil {
		fmt.Fprintf(b, "\nActivity\n%s typed, %d recalls, %d of them added memory\n", countOf(a.Prompts, "prompt", "prompts"), a.Recalls, a.WithHits)
	}
	if r.View == "overview" {
		section(b, "Lately", sessionHead, sessionRows(t.Lately, loc))
	}
	if t.Helped != nil {
		var rows [][]string
		for _, h := range t.Helped {
			kind := "—"
			if h.FactType != "" {
				kind = word(kindWords, h.FactType)
			}
			rows = append(rows, []string{idText(h.Memory), clipText(h.Project), kind, n64(h.Used), n64(h.Shown)})
		}
		section(b, "Helped most", []string{"Memory", "Project", "Kind", "Used", "Shown"}, rows)
	}
	if t.OutcomeMix != nil {
		var rows [][]string
		for _, d := range t.OutcomeMix {
			note := ""
			if d.Partial {
				note = "partial day"
			}
			rows = append(rows, []string{d.Day, n64(d.Used), n64(d.Ignored), n64(d.NotDeclared), n64(d.ShownOnly), n64(d.Pending), note})
		}
		section(b, "Memories shown, by what happened", []string{"Day", "Used by Claude", "Shown, then ignored", "Not declared", "Shown only", "Pending", "Note"}, rows)
	}
	if t.Reported != nil {
		var rows [][]string
		for _, x := range t.Reported {
			rows = append(rows, []string{idText(x.MemoryID), word(verdictWords, x.Verdict), idText(x.Session), clipText(x.Project), whenText(x.At, loc)})
		}
		section(b, "Reported wrong or out of date", []string{"Memory", "Reported", "Session", "Project", "When"}, rows)
	}
	if t.Extraction != nil {
		var rows [][]string
		for _, x := range t.Extraction {
			cost := "—"
			if x.Cost != nil {
				cost = fmt.Sprintf("%.2f", *x.Cost)
			}
			rows = append(rows, []string{x.Day, x.Operation, n64(x.Input), n64(x.Cached), n64(x.Output), cost})
		}
		section(b, "What the server's model spent, by day", []string{"Day", "Server task", "Input", "Cached", "Output", "Cost"}, rows)
	}
	if t.ByProject != nil {
		var rows [][]string
		for _, x := range t.ByProject {
			rows = append(rows, []string{clipText(x.Project), n64(x.Memories)})
		}
		section(b, "Memories per project", []string{"Project", "Memories"}, rows)
	}
	if t.TopShown != nil {
		var rows [][]string
		for _, x := range t.TopShown {
			rows = append(rows, []string{idText(x.Memory), clipText(x.Project), n64(x.Shown), percentOf(x.Share) + " %", n64(x.Used)})
		}
		section(b, "Most shown", []string{"Memory", "Project", "Shown", "Share of all shows", "Used"}, rows)
	}
	if rv := t.Review; rv != nil {
		for _, l := range []struct {
			name string
			rows []metrics.ReviewRow
		}{{"Often shown, never used", rv.NeverUsed}, {"Hidden", rv.Hidden}, {"Reported wrong", rv.Reported}} {
			section(b, "To review: "+l.name, []string{"Memory", "Project", "Shown", "Last shown", "Look at it"}, reviewRows(l.rows, loc))
		}
	}
	if t.FailedOperations != nil {
		var rows [][]string
		for _, x := range t.FailedOperations {
			rows = append(rows, []string{x.Type, ageText(x.AgeMS), n64(x.Retries)})
		}
		section(b, "Server tasks that failed", []string{"Task", "Age", "Retries"}, rows)
	}
	if t.NeverExtracted != nil {
		section(b, "Sessions sent that gave no memories", []string{"Session", "Project", "Sent", "What happened", "Retries"}, neverRows(t.NeverExtracted, loc))
	}
	if t.Jobs != nil {
		var rows [][]string
		for _, j := range t.Jobs {
			rows = append(rows, []string{j.Name, j.Status, whenText(j.LastAt, loc), n64(j.MS) + " ms", ageText(j.AgeMS)})
		}
		section(b, "Background jobs", []string{"Job", "Last result", "Last run", "Took", "Age"}, rows)
	}
	if t.Doctor != nil {
		var rows [][]string
		for _, c := range t.Doctor {
			rows = append(rows, []string{metrics.DoctorName(c.ID), c.Status, c.Fix})
		}
		section(b, "Health checks", []string{"Check", "Status", "Fix"}, rows)
	}
	if t.DoctorHistory != nil {
		doctorHistoryText(b, t.DoctorHistory)
	}
	if s := t.System; s != nil {
		systemText(b, *s, loc)
	}
	if m := t.Machines; m != nil {
		machinesText(b, *m, loc)
	}
	projectsText(b, t, loc)
	if t.Sessions != nil {
		section(b, "Sessions", sessionHead, sessionRows(t.Sessions, loc))
	}
	if s := t.Session; s != nil {
		sessionPageText(b, *s, loc)
	}
	memoriesText(b, t, loc)
	daysText(b, t, loc)
	releasesText(b, t, loc)
	if t.Defs != nil {
		defsText(b, t.Defs)
	}
	if p := t.Page; p != nil && p.Total > p.Limit {
		fmt.Fprintf(b, "\nShowing %d–%d of %d; --offset %d for the next page\n", p.Offset+1, min(p.Offset+p.Limit, p.Total), p.Total, p.Offset+p.Limit)
	}
}

func reviewRows(rows []metrics.ReviewRow, loc *time.Location) [][]string {
	var out [][]string
	for _, x := range rows {
		out = append(out, []string{idText(x.ID), clipText(x.Project), n64(x.Shown), whenPtr(x.LastShown, loc), x.Command})
	}
	return out
}

func systemText(b *strings.Builder, s metrics.SystemInfo, loc *time.Location) {
	b.WriteString("\nSystem\n")
	for _, r := range views.SystemRows(s, func(t time.Time) string { return whenText(t, loc) }) {
		fmt.Fprintf(b, "%-15s %s\n", r.Label, r.Text)
	}
}

// machinesText lists the PCs that share their figures and the documents set
// aside; with sharing off it says so.
func machinesText(b *strings.Builder, m metrics.Machines, loc *time.Location) {
	if !m.Sharing {
		b.WriteString("\nPCs\nSharing with other PCs is off (feedback.enabled and feedback.share_ledger): this PC's figures are neither sent nor read.\n")
	}
	var rows [][]string
	for _, r := range m.Rows {
		who := r.Machine
		if who == "" {
			who = "(not yet named)"
		}
		state := "quiet"
		switch {
		case r.Stale:
			state = "stopped sending"
		case r.Active:
			state = "active"
		}
		if r.This {
			who += " (this PC)"
		}
		upTo := "—"
		if r.Until != nil {
			upTo = *r.Until
		}
		sent := "never"
		if r.LastSent != nil {
			sent = whenText(*r.LastSent, loc)
		}
		version := r.BinaryVersion
		if version == "" {
			version = "—"
		}
		rows = append(rows, []string{who, r.OS, r.FirstSeen, sent, version, upTo, state})
	}
	heading := "PCs"
	if m.Sharing {
		fetched := "never fetched"
		if m.FetchedAt != nil {
			fetched = "fetched " + whenText(*m.FetchedAt, loc)
		}
		heading += " (" + fetched + ")"
		section(b, heading, []string{"PC", "OS", "First seen", "Last sent", "Version", "Figures up to", "State"}, rows)
	} else if len(rows) > 0 {
		table(b, []string{"PC", "OS", "First seen", "Last sent", "Version", "Figures up to", "State"}, rows)
	}
	if len(m.Ignored) > 0 {
		var ign [][]string
		for _, g := range m.Ignored {
			who := g.Machine
			if who == "" {
				who = "(unknown PC)"
			}
			ign = append(ign, []string{who, word(views.IgnoredWords, g.Reason)})
		}
		section(b, "Documents ignored", []string{"PC", "Why"}, ign)
	}
}

func projectRow(p metrics.ProjectRow, loc *time.Location) []string {
	mem := "—"
	if p.MemoriesOnServer != nil {
		mem = n64(*p.MemoriesOnServer)
	}
	name := clipText(p.Project)
	if p.Project == "" {
		name = "(no project)"
	}
	return []string{name, n64(p.Sessions), n64(p.Prompts), n64(p.PromptsWithMemory), n64(p.UsedTurns), n64(p.ReportedWrong), mem, whenPtr(p.LastActivity, loc)}
}

var projectHead = []string{"Project", "Sessions", "Prompts", "With memory", "Used turns", "Reported wrong", "Memories", "Last activity"}

func projectsText(b *strings.Builder, t metrics.Tables, loc *time.Location) {
	if t.Projects != nil {
		var rows [][]string
		for _, p := range t.Projects {
			rows = append(rows, projectRow(p, loc))
		}
		if p := t.Pooled; p != nil {
			rows = append(rows, projectRow(p.Totals, loc))
			rows[len(rows)-1][0] = p.Label
		}
		section(b, "Projects", projectHead, rows)
	}
	if p := t.Project; p != nil {
		fmt.Fprintf(b, "\nProject %s\n", clipText(p.Project))
		if p.NoMemories {
			b.WriteString("It has no memories yet.\n")
		}
		if p.AttributionDisagreements > 0 {
			fmt.Fprintf(b, "%s attributed to another project by the server\n", countOf(p.AttributionDisagreements, "memory", "memories"))
		}
		section(b, "Most used", []string{"Memory", "Project", "Shown", "Last shown", "Look at it"}, reviewRows(p.TopUsed, loc))
		section(b, "Often shown, never used", []string{"Memory", "Project", "Shown", "Last shown", "Look at it"}, reviewRows(p.NeverUsed, loc))
		section(b, "Sessions", sessionHead, sessionRows(p.Sessions, loc))
	}
}

func sessionPageText(b *strings.Builder, s metrics.SessionPage, loc *time.Location) {
	section(b, "Session "+idText(s.Totals.ID), sessionHead, sessionRows([]metrics.SessionRow{s.Totals}, loc))
	var rows [][]string
	for _, t := range s.Turns {
		n := "—"
		if t.N != nil {
			n = n64(*t.N)
		}
		what := t.Decision
		switch {
		case t.Reason != "":
			what += ": " + t.Reason
		case t.Empty != "":
			what += ": nothing added (" + emptyWords.Replace(t.Empty) + ")"
		}
		var shows []string
		for _, c := range t.Shows {
			rank := ""
			if c.Rank != nil {
				rank = fmt.Sprintf(" #%d", *c.Rank)
			}
			shows = append(shows, fmt.Sprintf("%s%s %s (%s)", idText(c.MemoryID), rank, word(outcomeWords, c.Outcome), word(pathWords, c.Path)))
		}
		var said []string
		for _, id := range t.Declared {
			said = append(said, idText(id))
		}
		rows = append(rows, []string{n, whenText(t.At, loc), what, strings.Join(shows, "; "), n64(t.Searches), strings.Join(said, " "), word(checkWords, t.Check)})
	}
	section(b, "Turns", []string{"Prompt", "When", "What memory did", "Shown", "Searches", "Claude said it used", "End of turn"}, rows)
}

// promptText names the prompt a memory was shown at: its session (as the
// sessions table cuts it) and the prompt's number in it ("#3"), the session
// alone when the number is unknown; never the internal turn id.
func promptText(s metrics.ShowMark) string {
	sess := idText(s.Session)
	if sess == "" {
		sess = "—"
	}
	if s.Prompt == nil {
		return sess
	}
	return fmt.Sprintf("%s #%d", sess, *s.Prompt)
}

func memoryRow(m metrics.MemoryRow, loc *time.Location) []string {
	rate := "—"
	if m.Rate != nil {
		rate = percentOf(*m.Rate) + " %"
	}
	return []string{idText(m.ID), clipText(m.Project), n64(m.Shown), fmt.Sprintf("%.1f", m.Used), fmt.Sprintf("%.1f", m.Ignored), rate,
		fmt.Sprintf("%+.2f", m.Boost), whenPtr(m.LastShown, loc), whenPtr(m.LastUsed, loc), word(memoryStateWords, m.State)}
}

func memoriesText(b *strings.Builder, t metrics.Tables, loc *time.Location) {
	head := []string{"Memory", "Project", "Shown", "Used", "Ignored", "Used rate", "Learned ranking", "Last shown", "Last used", "State"}
	if t.Memories != nil {
		var rows [][]string
		for _, m := range t.Memories {
			rows = append(rows, memoryRow(m, loc))
		}
		section(b, "Memories (recent-weighted counts)", head, rows)
	}
	p := t.Memory
	if p == nil {
		return
	}
	section(b, "Memory "+idText(p.Row.ID), head, [][]string{memoryRow(p.Row, loc)})
	if p.Kind == "" && p.ServerState == "" && p.Created == nil {
		b.WriteString("Kind, creation and state on the server: unknown (the server did not answer)\n")
	} else {
		created, kind, state := "unknown", "unknown", "unknown"
		if p.Created != nil {
			created = whenText(*p.Created, loc)
		}
		if p.Kind != "" {
			kind = word(kindWords, p.Kind)
		}
		if p.ServerState != "" {
			state = p.ServerState
		}
		fmt.Fprintf(b, "Kind %s, created %s, on the server: %s\n", kind, created, state)
	}
	if p.Origin != nil {
		for _, l := range originLine(*p.Origin, loc) {
			b.WriteString(l + "\n")
		}
	}
	var rows [][]string
	for _, s := range p.Shows {
		rank := "—"
		if s.Rank != nil {
			rank = n64(*s.Rank)
		}
		rows = append(rows, []string{whenText(s.At, loc), word(pathWords, s.Path), word(outcomeWords, s.Outcome), promptText(s), rank})
	}
	section(b, "Showings", []string{"When", "Shown", "What happened", "Session and prompt", "Rank"}, rows)
	rows = nil
	for _, r := range p.Retirements {
		rows = append(rows, []string{whenText(r.At, loc), r.Kind})
	}
	section(b, "Hidden and restored", []string{"When", "What"}, rows)
	for _, c := range p.Commands {
		fmt.Fprintf(b, "Run: %s\n", c)
	}
}

func daysText(b *strings.Builder, t metrics.Tables, loc *time.Location) {
	if t.Calendar != nil {
		recorded := 0
		for _, c := range t.Calendar {
			if c.Recorded {
				recorded++
			}
		}
		fmt.Fprintf(b, "\ncalendar: %d days recorded of %d\n", recorded, len(t.Calendar))
	}
	if t.Days != nil {
		var rows [][]string
		for _, d := range t.Days {
			rows = append(rows, []string{d.Day, n64(d.Prompts), n64(d.Recalls), n64(d.PromptsWithMemory), n64(d.UsedTurns), n64(d.Wrong),
				thousands(float64(d.ContextChars)), n64(d.Sessions), n64(d.SentOK), n64(d.SentFailed)})
		}
		section(b, "Days", []string{"Day", "Prompts", "Recalls", "With memory", "Used turns", "Wrong", "Context chars", "Sessions", "Sent", "Send failed"}, rows)
	}
	if d := t.Day; d != nil {
		section(b, "Sessions of "+d.Day, sessionHead, sessionRows(d.Sessions, loc))
		if len(d.Markers) > 0 {
			markersText(b, "What changed on "+d.Day, d.Markers, loc)
		}
	}
}

func releasesText(b *strings.Builder, t metrics.Tables, loc *time.Location) {
	if t.Releases != nil {
		var rows [][]string
		for _, x := range t.Releases {
			bank := "—"
			if x.BankSize != nil {
				bank = n64(*x.BankSize)
			}
			rows = append(rows, []string{x.Version, whenText(x.First, loc), whenText(x.Last, loc), n64(x.N), fmt.Sprintf("%.1f", x.PromptsPerDay), bank})
		}
		section(b, "Versions that ran", []string{"Version", "First", "Last", "Events", "Prompts per day", "Memories"}, rows)
	}
	if len(t.Markers) > 0 {
		markersText(b, "What changed", t.Markers, loc)
	}
	if t.Compare != nil {
		compareText(b, *t.Compare, loc)
	}
	if len(t.Settings) > 0 {
		keys := make([]string, 0, len(t.Settings))
		for k := range t.Settings {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteString("\nSettings in force\n")
		for _, k := range keys {
			fmt.Fprintf(b, "%-34s %s\n", k, strconv.FormatFloat(t.Settings[k], 'f', -1, 64))
		}
	}
	if t.Note != "" {
		b.WriteString("\n" + strings.ToUpper(t.Note[:1]) + t.Note[1:] + "\n")
	}
}

func defsText(b *strings.Builder, defs []metrics.DefRow) {
	if len(defs) == 1 {
		d := defs[0]
		fmt.Fprintf(b, "\n%s (%s)\n%s\n\n%s\n\nIf it is bad: %s\nSee: %s\n", d.Label, d.ID, d.Question, d.Definition, d.Action, d.Command)
		if d.N > 0 {
			fmt.Fprintf(b, "In the last 7 days: n=%s", grouped(d.N))
			if d.MDC != nil && *d.MDC < 1 {
				fmt.Fprintf(b, ", smallest change it can show %s pts", percentOf(*d.MDC))
			}
			b.WriteByte('\n')
		}
		return
	}
	var rows [][]string
	for _, d := range defs {
		mdc := "—"
		if d.MDC != nil {
			mdc = "too few to tell"
			if *d.MDC < 1 {
				mdc = percentOf(*d.MDC) + " pts"
			}
		}
		rows = append(rows, []string{d.ID, d.Label, word(unitWords, d.Unit), n64(int64(d.MinN)), n64(d.N), mdc})
	}
	section(b, "Metrics", []string{"Id", "What it is", "Unit", "Needs n", "n now", "Smallest change"}, rows)
}

// extraAs reads an extra of a metric as a T, whatever type the computing
// code stored it in or a decoded envelope gave it.
func extraAs[T any](m metrics.Metric, key string) (v T, ok bool) {
	raw, found := m.Extra[key]
	if !found || raw == nil {
		return v, false
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &v) != nil {
		return v, false
	}
	return v, true
}

// spanText is a duration the way a person reads it: minutes up to two
// hours, then hours, then days.
func spanText(ms float64) string {
	min := int64(math.Round(ms / 60000))
	switch {
	case ms < 60000:
		return "under 1 min"
	case min < 120:
		return strconv.FormatInt(min, 10) + " min"
	case min < 48*60:
		return strconv.FormatInt(int64(math.Round(float64(min)/60)), 10) + " h"
	}
	return strconv.FormatInt(int64(math.Round(float64(min)/(24*60))), 10) + " days"
}

// hopWords name the steps of the way from a session to a recalled memory.
var hopWords = map[string]string{
	"last_to_sent": "last turn to sent", "sent_to_extracted": "sent to turned into memories",
	"extracted_to_recalled": "turned into memories to shown again", "whole_way": "the whole way",
}

// funnelRest are the funnel's counts beside the four steps, in the order
// the line says them.
var funnelRest = []string{"waiting_or_failed", "never", "on_server", "not_recalled", "before_record", "unresolved", "gone"}

// blockDetails are the indented lines under the metrics this release added:
// what their number does not say by itself.
func blockDetails(d metrics.Def, m metrics.Metric, loc *time.Location) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	switch d.ID {
	case "pipeline.time_to_recall":
		f, ok := extraAs[map[string]int64](m, "funnel")
		if !ok || f["ended"] == 0 {
			break
		}
		add("%s %d → %s %d → %s %d → %s %d", views.FunnelWords["ended"], f["ended"], views.FunnelWords["sent"], f["sent"],
			views.FunnelWords["extracted"], f["extracted"], views.FunnelWords["recalled"], f["recalled"])
		var rest []string
		for _, k := range funnelRest {
			if f[k] > 0 {
				rest = append(rest, fmt.Sprintf("%s %d", views.FunnelWords[k], f[k]))
			}
		}
		if len(rest) > 0 {
			add("%s", strings.Join(rest, ", "))
		}
		hops, _ := extraAs[[]metrics.Hop](m, "hops")
		for _, h := range hops {
			if h.N == 0 {
				continue
			}
			if h.P50 == nil { // a typical time needs 20 sessions
				add("%s: %s, too few for a typical time", word(hopWords, h.Name), countOf(h.N, "session", "sessions"))
				continue
			}
			line := fmt.Sprintf("%s: typical %s", word(hopWords, h.Name), spanText(*h.P50))
			if h.P95 != nil {
				line += ", slowest 1 in 20: " + spanText(*h.P95)
			}
			add("%s (%s)", line, countOf(h.N, "session", "sessions"))
		}
	case "memory.yield":
		for _, p := range m.Series {
			add("%s", yieldWeekLine(p))
		}
		if w, _ := extraAs[string](m, "withheld"); w != "" {
			n, _ := extraAs[int64](m, "unresolved")
			add("shares withheld: the list of which memories are new is still catching up (%s not placed yet)", grouped(n))
		}
	case "secrets.suspects":
		if at, ok := extraAs[time.Time](m, "scanned_at"); ok {
			line := "last scan " + whenText(at, loc)
			if age, ok := extraAs[int64](m, "age_days"); ok {
				if age == 0 {
					line += ", today"
				} else {
					line += ", " + countOf(age, "day", "days") + " ago"
				}
			}
			if pages, ok := extraAs[int64](m, "pages"); ok && pages > 0 {
				line += " (" + countOf(pages, "page", "pages") + " read)"
			}
			add("%s", line)
		}
	case "doctor.history":
		if m.Num != nil {
			without, _ := extraAs[int64](m, "days_without")
			add("%s in the window, %s without a run", countOf(*m.Num, "run", "runs"), countOf(without, "day", "days"))
		}
	case "bank.active_share":
		if n, _ := extraAs[int64](m, "unresolved"); n > 0 {
			add("%s shown, not placed yet, count as older", countOf(n, "memory", "memories"))
		}
		if n, _ := extraAs[int64](m, "gone"); n > 0 {
			add("%s shown, since forgotten, left out", countOf(n, "memory", "memories"))
		}
	case "shows.concentration":
		if share, ok := extraAs[float64](m, "top10_share"); ok && m.Status != metrics.StatusTooFew { // too few shows: no share to quote
			shows, _ := extraAs[int64](m, "shows")
			add("the 10 most shown memories take %s %% of the %s", percentOf(share), countOf(shows, "show", "shows"))
		}
	case "boost.set_changed":
		prompts, _ := extraAs[int64](m, "prompts")
		pc, _ := extraAs[int64](m, "prompts_set_changed")
		searches, _ := extraAs[int64](m, "searches")
		sc, _ := extraAs[int64](m, "searches_set_changed")
		add("prompts %s of %s, searches %s of %s", grouped(pc), grouped(prompts), grouped(sc), grouped(searches))
	case "recall.by_lang":
		t, ok := extraAs[struct {
			Reference string `json:"reference"`
			RefNum    int64  `json:"reference_num"`
			RefDen    int64  `json:"reference_den"`
			Gaps      []struct {
				Lang     string     `json:"lang"`
				Num, Den int64      `json:"-"`
				Value    float64    `json:"value"`
				Gap      float64    `json:"gap"`
				Interval [2]float64 `json:"interval"`
			} `json:"gaps"`
			LeftOut []struct {
				Lang string `json:"lang"`
				Num  int64  `json:"num"`
				Den  int64  `json:"den"`
			} `json:"left_out"`
		}](m, "test")
		if !ok || t.Reference == "" {
			break
		}
		for _, g := range t.Gaps {
			add("%s: %s %% of prompts got memory, %s pts behind %s (%s to %s)", g.Lang, percentOf(g.Value), signedPoints(g.Gap), t.Reference, signedPoints(g.Interval[0]), signedPoints(g.Interval[1]))
		}
		for _, l := range t.LeftOut {
			add("%s: %d of %d, too few to compare", l.Lang, l.Num, l.Den)
		}
	case "bank.growth", "extraction.tokens":
		if from, _ := extraAs[string](m, "record_from"); from != "" {
			add("older days come from the days this PC recorded, from %s", from)
		}
	}
	return out
}

// signedPoints is a ratio difference in whole points, with its sign (a real
// minus sign for a fall, none for a zero).
func signedPoints(r float64) string {
	n := int(math.Round(r * 100))
	switch {
	case n > 0:
		return "+" + strconv.Itoa(n)
	case n < 0:
		return "−" + strconv.Itoa(-n)
	}
	return "0"
}

func yieldWeekLine(p metrics.Point) string {
	head := "week of " + p.Week + ": "
	switch {
	case p.Complete == nil || !*p.Complete:
		return head + "not complete yet"
	case p.Den == nil || p.Num == nil:
		return head + "the new memories of that week were not recorded"
	}
	line := fmt.Sprintf("%s%s of %s", head, grouped(*p.Num), grouped(*p.Den))
	switch {
	case p.Value != nil && p.CI != nil:
		line += fmt.Sprintf(" (%s %%, %s to %s %%)", percentOf(*p.Value), percentOf(p.CI[0]), percentOf(p.CI[1]))
	case p.Value != nil:
		line += " (" + percentOf(*p.Value) + " %)"
	default:
		line += ", too few for a share"
	}
	if p.Tokens != nil {
		line += ", " + grouped(*p.Tokens) + " tokens"
		if p.TokensPerUsed != nil {
			line += ", " + grouped(int64(math.Round(*p.TokensPerUsed))) + " tokens per useful memory"
		}
	}
	return line
}

// neverRows lists the sessions sent that gave no memories.
func neverRows(rows []metrics.NeverExtractedRow, loc *time.Location) [][]string {
	var out [][]string
	for _, x := range rows {
		out = append(out, []string{idText(x.Session), clipText(x.Project), whenText(x.Sent, loc), word(views.NeverExtractedWords, x.State), n64(int64(x.Retries))})
	}
	return out
}

func doctorHistoryText(b *strings.Builder, rows []metrics.DoctorHistoryRow) {
	var out [][]string
	for _, r := range rows {
		change := "—"
		if c := r.LastChange; c != nil {
			change = fmt.Sprintf("%s, %s → %s", c.Day, word(views.DayStatusWords, c.From), word(views.DayStatusWords, c.To))
		}
		out = append(out, []string{metrics.DoctorName(r.Check), word(views.DayStatusWords, r.Now), n64(r.OK), n64(r.Watch), n64(r.Bad), change})
	}
	section(b, "Health checks over time", []string{"Check", "Now", "Days ok", "Days with a warning", "Days with a problem", "Last change"}, out)
}

func numText(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// markerText says what a marker records.
func markerText(m metrics.Marker) string {
	if m.Kind == "release" {
		return "new version " + m.Version + " (after " + m.Previous + ")"
	}
	var parts []string
	for _, c := range m.Changes {
		switch {
		case c.From == nil && c.To != nil:
			parts = append(parts, c.Key+" set to "+numText(*c.To))
		case c.To == nil && c.From != nil:
			parts = append(parts, c.Key+" removed (was "+numText(*c.From)+")")
		case c.From != nil && c.To != nil:
			parts = append(parts, c.Key+" "+numText(*c.From)+" → "+numText(*c.To))
		}
	}
	return "settings changed: " + strings.Join(parts, "; ")
}

func markersText(b *strings.Builder, heading string, ms []metrics.Marker, loc *time.Location) {
	var rows [][]string
	for _, m := range ms {
		rows = append(rows, []string{whenText(m.At, loc), markerText(m)})
	}
	section(b, heading, []string{"When", "What"}, rows)
}

// originLine says where a memory came from, and when its session went through
// each step.
func originLine(o metrics.Origin, loc *time.Location) []string {
	switch o.State {
	case metrics.OriginThisPC:
		lines := []string{fmt.Sprintf("Came from %s (%s, %s)", views.OriginWords[o.State], idText(o.Session), clipText(o.Project))}
		var steps []string
		for _, s := range []struct {
			name string
			at   *time.Time
		}{{"ended", o.Ended}, {"sent", o.Sent}, {"turned into memories", o.Extracted}} {
			if s.at != nil {
				steps = append(steps, s.name+" "+whenText(*s.at, loc))
			}
		}
		if o.FirstShown != nil {
			steps = append(steps, "first shown "+whenText(*o.FirstShown, loc))
		} else {
			steps = append(steps, "never shown")
		}
		return append(lines, "Session "+strings.Join(steps, ", "))
	case metrics.OriginOtherPC:
		line := "Came from " + views.OriginWords[o.State]
		if o.Created != nil {
			line += ", made " + *o.Created
		}
		return []string{line}
	case metrics.OriginNoSession:
		return []string{"Came from " + views.OriginWords[o.State]}
	}
	return []string{"Came from a source " + views.OriginWords[metrics.OriginUnknown]}
}

// compareText is the comparison of two versions: the pooled difference of the
// matched groups, the raw one when it disagrees, the groups and the settings.
func compareText(b *strings.Builder, c metrics.Comparison, loc *time.Location) {
	label := c.Measure
	if d, ok := metrics.DefOf(c.Measure); ok {
		label = d.Label
	}
	unit := map[string]string{"used.turn_rate": "turns", "recall.hit_rate": "prompts", "verdict.wrong_rate": "verdicts"}[c.Measure]
	fmt.Fprintf(b, "\nComparison of %s and %s on %s\n", c.A, c.B, label)
	if c.TooFew != nil || c.Diff == nil || c.CI == nil {
		why := ""
		if t := c.TooFew; t != nil {
			why = fmt.Sprintf("%s has %s and %s in its best group; %s", t.Version, countOf(int64(t.Days), "active day", "active days"),
				countOf(int64(t.Units), strings.TrimSuffix(unit, "s"), unit), map[string]string{
					"days": "not enough active days", "units": "not enough " + unit, "sessions": "not enough sessions"}[t.Why])
			if t.RetryAfter != nil {
				why += ", at the earliest from " + t.RetryAfter.In(loc).Format("2006-01-02")
			}
		}
		fmt.Fprintf(b, "%s: %s\n", word(views.ReadingWords, "too_few"), why)
		return
	}
	fmt.Fprintf(b, "matched difference %s pts (%s to %s), %s\n", signedPoints(*c.Diff), signedPoints(c.CI[0]), signedPoints(c.CI[1]), word(views.ReadingWords, c.Reading))
	if c.RawDisagrees && c.RawA.Den > 0 && c.RawB.Den > 0 {
		a, bb := float64(c.RawA.Num)/float64(c.RawA.Den), float64(c.RawB.Num)/float64(c.RawB.Den)
		fmt.Fprintf(b, "unmatched %s pts (%s %% → %s %%): read the matched figure\n", signedPoints(bb-a), percentOf(a), percentOf(bb))
	}
	fmt.Fprintf(b, "%s in %s, %d in %s\n", countOf(int64(c.SessionsA), "session", "sessions"), c.A, c.SessionsB, c.B)
	var rows [][]string
	for _, s := range c.Strata {
		side := func(x metrics.StratumSide) string {
			return fmt.Sprintf("%s of %s (%s)", grouped(x.Num), grouped(x.Den), countOf(int64(x.Days), "day", "days"))
		}
		result := "not compared: " + map[string]string{"days": "not enough active days", "units": "not enough " + unit, "sessions": "not enough sessions"}[s.Why]
		if s.Eligible && s.Diff != nil && s.CI != nil {
			result = fmt.Sprintf("%s pts (%s to %s), %s", signedPoints(*s.Diff), signedPoints(s.CI[0]), signedPoints(s.CI[1]), strings.ToLower(word(views.ReadingWords, s.Reading)))
		}
		path := word(pathWords, s.Path)
		if s.Path == "unknown" {
			path = "not shown to Claude itself"
		}
		rows = append(rows, []string{clipText(s.Project), path, side(s.A), side(s.B), result})
	}
	b.WriteString("\n")
	table(b, []string{"Project", "Shown", "How " + c.A + " did", "How " + c.B + " did", "Difference"}, rows)
	if len(c.SettingsDiffer) > 0 {
		var parts []string
		for _, s := range c.SettingsDiffer {
			p := s.Key + " " + ptrNum(s.A) + " → " + ptrNum(s.B)
			if s.ChangedAt != nil {
				p += " (changed " + whenText(*s.ChangedAt, loc) + ")"
			}
			parts = append(parts, p)
		}
		fmt.Fprintf(b, "\nSettings that differ: %s\n", strings.Join(parts, "; "))
	}
	if c.SettingsUnknown {
		b.WriteString("\nSettings of a version are not known: nothing was recorded before its first turn.\n")
	}
}

func ptrNum(v *float64) string {
	if v == nil {
		return "—"
	}
	return numText(*v)
}
