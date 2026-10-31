package cli

import (
	"fmt"
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

// footer names the scope of the numbers.
func footer(r metrics.Result) string {
	machines := "this PC"
	if r.Scope.Machines != "" && r.Scope.Machines != "this" {
		machines = r.Scope.Machines
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
			lines = append(lines, metricLine(d, m, r.Scope.ServerOK))
			if m.Status == metrics.StatusNoData {
				continue
			}
			for _, x := range metricDetails(d, m, r.Window.Loc) {
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
func metricLine(d metrics.Def, m metrics.Metric, serverUp bool) string {
	value := formatValue(d, m)
	if m.Status == metrics.StatusNoData {
		value = "—"
	}
	switch {
	case m.Status == metrics.StatusTooFew:
		value = "—"
		if d.Unit == "ratio" && m.Num != nil && m.Den != nil {
			value = fmt.Sprintf("%s of %s", grouped(*m.Num), grouped(*m.Den))
		}
	case m.Status != metrics.StatusNoData && d.Unit == "ratio" && m.Num != nil && m.Den != nil && *m.Den < handful:
		value = fmt.Sprintf("%s of %s", grouped(*m.Num), grouped(*m.Den)) // a percentage over so few would be a guess
	case m.Status != metrics.StatusNoData && d.ID == "server.reach" && m.Value != nil:
		value = map[bool]string{true: "yes", false: "no"}[serverUp] // what the view's header says, not the skipped-prompt count
	case m.Status != metrics.StatusNoData && d.ID == "recall.empty_reasons" && m.Value != nil:
		value = countOf(int64(*m.Value), "prompt", "prompts")
	case m.Status != metrics.StatusNoData && d.ID == "doctor.check" && m.Value != nil:
		value = grouped(int64(*m.Value)) + " failed"
	case m.Status != metrics.StatusNoData && d.ID == "jobs.last_run" && m.Value != nil:
		value = ageText(int64(*m.Value)) // the last tick's age, as the jobs table prints it
	}
	var parts []string
	if m.Den != nil {
		parts = append(parts, "n="+grouped(*m.Den))
	}
	parts = append(parts, metricStatusText(d, m))
	if slices.Contains(m.Tags, "unreliable") {
		parts = append(parts, "unreliable")
	}
	if slices.Contains(m.Tags, "judged_28d") {
		parts = append(parts, "judged on the last 28 days")
	}
	if slices.Contains(m.Tags, "not_filtered") {
		parts = append(parts, "not filtered by project")
	}
	if dl := deltaText(d, m); dl != "" {
		parts = append(parts, dl)
	}
	return fmt.Sprintf("%-44s %-24s (%s)", labelOf(d), value, strings.Join(parts, ", "))
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

func whenText(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02 15:04") }

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
	if s := t.System; s != nil {
		systemText(b, *s, loc)
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
