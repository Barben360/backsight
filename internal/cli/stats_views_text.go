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
	valueIn          = views.ValueIn
	percentOf        = views.Percent
	thousands        = views.Thousands
	grouped          = views.Grouped
	plainText        = views.Plain
	clipText         = views.Clip
	idText           = views.ShortID
	ageText          = views.Age
	word             = views.Word
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

// pricesHint says how to get a cost: it needs the model's prices.
const pricesHint = "costs need prices, set price_input, price_cached and price_output under [llm] in the config file"

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
func countOf(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return grouped(n) + " " + many
}

// Reading the extras, whose types are those of the computing code.

func extraInt(m metrics.Metric, key string) (int64, bool) {
	switch v := m.Extra[key].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

func extraFloat(m metrics.Metric, key string) (float64, bool) {
	switch v := m.Extra[key].(type) {
	case float64:
		return v, true
	case *float64:
		if v != nil {
			return *v, true
		}
	case int64:
		return float64(v), true
	}
	return 0, false
}

func extraTime(m metrics.Metric, key string, loc *time.Location) (string, bool) {
	var t time.Time
	switch v := m.Extra[key].(type) {
	case time.Time:
		t = v
	case *time.Time:
		if v == nil {
			return "", false
		}
		t = *v
	default:
		return "", false
	}
	if t.IsZero() {
		return "", false
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04"), true
}

// extraCounts reads a map of counts, sorted by key.
func extraCounts(m metrics.Metric, key string) []string {
	var out []string
	switch v := m.Extra[key].(type) {
	case map[string]int64:
		for k, n := range v {
			if n > 0 {
				out = append(out, k+" "+grouped(n))
			}
		}
	case map[string]any:
		for k, x := range v {
			if n, ok := x.(int64); ok && n > 0 {
				out = append(out, k+" "+grouped(n))
			}
		}
	}
	slices.Sort(out)
	return out
}

// metricDetails are the indented lines under a metric: what the number does
// not say by itself (a lower bound, a partial total, a split).
func metricDetails(d metrics.Def, m metrics.Metric, loc *time.Location) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	switch d.ID {
	case "recall.empty_reasons":
		if e := extraCounts(m, "empty"); len(e) > 0 {
			add("why: %s", emptyWords.Replace(strings.Join(e, ", ")))
		}
		if s := extraCounts(m, "skips"); len(s) > 0 {
			add("prompts skipped: %s", strings.Join(s, ", "))
		}
	case "used.turn_rate":
		if w, ok := extraFloat(m, "worst_case"); ok {
			add("worst case, if every turn where Claude did not say counts as unused: %s %%", percentOf(w))
		}
		if p, _ := extraInt(m, "pending"); p > 0 {
			add("%s still open", countOf(p, "turn", "turns"))
		}
	case "context.per_session", "context.per_prompt", "recall.latency", "search.latency":
		p50, ok50 := extraFloat(m, "p50")
		p95, ok95 := extraFloat(m, "p95")
		if ok50 && ok95 {
			add("typical %s, %s %s", valueIn(d.Unit, p50, m), map[bool]string{true: "slowest 1 in 20", false: "high"}[d.Unit == "ms"], valueIn(d.Unit, p95, m))
		}
	case "extraction.tokens", "extraction.cost":
		if d.ID == "extraction.cost" && m.Value == nil {
			add("%s", pricesHint)
		}
		if n, _ := extraInt(m, "failed_operations"); n > 0 {
			add("partial: %s could not be read, so this is a lower bound", countOf(n, "server task", "server tasks"))
		}
	case "bank.size":
		var kinds []string
		for _, k := range []string{"world", "experience", "observation"} {
			if n, ok := extraInt(m, k); ok {
				kinds = append(kinds, fmt.Sprintf("%s %s", grouped(n), kindWords[k]))
			}
		}
		if len(kinds) > 0 {
			add("%s", strings.Join(kinds, ", "))
		}
	case "bank.consolidation":
		if f, _ := extraInt(m, "failed"); f > 0 {
			add("%s failed", countOf(f, "summary", "summaries"))
		}
	case "pipeline.flow":
		ok, _ := extraInt(m, "ok")
		failed, _ := extraInt(m, "failed")
		aside, _ := extraInt(m, "set_aside")
		wait, _ := extraInt(m, "waiting")
		add("sent %s, failed %s, set aside after an error %s, waiting %s", grouped(ok), grouped(failed), grouped(aside), grouped(wait))
	case "local.backlog":
		notes, _ := extraInt(m, "notes_queued")
		sess, _ := extraInt(m, "sessions_waiting")
		aside, _ := extraInt(m, "quarantined")
		if notes+sess+aside > 0 {
			ms, _ := extraInt(m, "oldest_ms")
			add("%s and %s wait on this PC, %s set aside after an error; the oldest %s", countOf(notes, "note", "notes"), countOf(sess, "session", "sessions"), grouped(aside), ageText(ms))
		}
	case "server.queue":
		pending, _ := extraInt(m, "pending")
		proc, _ := extraInt(m, "processing")
		failed, _ := extraInt(m, "failed")
		total, _ := extraInt(m, "failed_total")
		capped, _ := m.Extra["failed_capped"].(bool)
		f := grouped(failed)
		if capped {
			f = grouped(failed) + "+ (a lower bound: more are not read)"
		}
		add("waiting %s, running %s, failed in the last 7 days %s, failed in all %s", grouped(pending), grouped(proc), f, grouped(total))
	case "hooks.failures":
		if l := extraCounts(m, "last_24h"); len(l) > 0 {
			add("in the last 24 h: %s", strings.Join(l, ", "))
		}
	case "server.reach":
		if t, ok := extraTime(m, "failing_since", loc); ok {
			add("not answering since %s", t)
		}
		if n, _ := extraInt(m, "skipped_prompts"); n > 0 {
			add("%s skipped while it was down", countOf(n, "prompt", "prompts"))
		}
	case "doctor.check":
		e, _ := extraInt(m, "error")
		w, _ := extraInt(m, "warn")
		add("%s failed, %s with a warning", grouped(e), grouped(w))
	}
	if slices.Contains(m.Tags, "partial") && d.ID != "extraction.tokens" && d.ID != "extraction.cost" {
		add("partial: some of it could not be read")
	}
	return out
}

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

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

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
	at := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return whenText(t, loc)
	}
	fmt.Fprintf(b, "Memory server   version %s, running in %s, mode %s\n", s.ServerVersion, s.Runtime, s.Mode)
	reach := "no answer recorded yet"
	if !s.Reach.LastOK.IsZero() {
		reach = "last answer " + at(s.Reach.LastOK)
	}
	if !s.Reach.FailingSince.IsZero() {
		reach = "not answering since " + at(s.Reach.FailingSince)
	}
	if !s.Reach.BreakerOpenUntil.IsZero() {
		reach += fmt.Sprintf("; paused until %s after %s", at(s.Reach.BreakerOpenUntil), countOf(int64(s.Reach.BreakerFailures), "failure", "failures"))
	}
	fmt.Fprintf(b, "Reach           %s\n", reach)
	backup := "none yet"
	if !s.Backup.CreatedAt.IsZero() {
		backup = "last " + at(s.Backup.CreatedAt) + ", checked " + at(s.Backup.VerifiedAt) + ", ok: " + yesNo(s.Backup.OK)
	}
	fmt.Fprintf(b, "Backup          %s\n", backup)
	update := "none newer"
	switch {
	case s.Update.CheckFailed:
		update = "the last check failed"
	case s.Update.Newer > 0:
		update = countOf(int64(s.Update.Newer), "newer release", "newer releases")
	}
	checked := "never checked"
	if !s.Update.CheckedAt.IsZero() {
		checked = "checked " + at(s.Update.CheckedAt)
	}
	fmt.Fprintf(b, "Updates         %s (%s)\n", update, checked)
	idx := fmt.Sprintf("ingested %s, %d s behind", at(s.Index.IngestedAt), s.Index.LagSeconds)
	if s.Index.Partial {
		idx += ", still catching up"
	}
	fmt.Fprintf(b, "Activity index  %s\n", idx)
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
