package dashboard

import (
	"fmt"
	"html/template"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// observational is what the Releases page says first about its own numbers.
const observational = "Observational: weeks, projects and memory differ."

// replayCmd is the example the Releases page gives for trying another
// learned-ranking setting.
const replayCmd = "backsight feedback replay --set boost_weight=0.15"

// ReleasesPage is the Releases page: the versions that ran, the settings in
// force and how to replay past recalls under another setting.
type ReleasesPage struct {
	Observational string
	Head          Head
	Note          NoData
	Rows          []ReleaseLine
	Foot          string
	Settings      SettingsCard
	Replay        ReplayCard
	Compare       CompareCard
	Used          UsedCard
}

// ReplayCard points at the command that replays past recalls.
type ReplayCard struct {
	Head      Head
	Text, Cmd string
}

// ReleaseLine is a row of the versions table. Version is the raw version,
// the row's id; Href is the link to that row.
type ReleaseLine struct {
	Version, Href string
	Span          string
	Prompts       string
	Judged        string
	Flags         []Cell
	Top           Ref
	TopShare      string
	Shares        string // every project's share, for the hover
	PerDay, Bank  string
}

// SettingsCard is the settings in force, by name.
type SettingsCard struct {
	Head Head
	Note NoData
	Rows []SettingLine
}

// SettingLine is one setting and its value.
type SettingLine struct{ Name, Value string }

// releaseIDPrefix starts the id of a release row, so a version named like an
// element of the page ("main", "copy-status") cannot take its id.
const releaseIDPrefix = "release-"

// versionHref is the link to a version's row on the Releases page. A
// version without a row in the window opens the page at its top.
func versionHref(version string) string {
	return "/releases#" + (&url.URL{Fragment: releaseIDPrefix + version}).EscapedFragment()
}

// versionHref is the link to a version's row on the Releases page; the
// page's window travels with it, as the row may be outside the default one.
func (d PageData) versionHref(version string) string {
	return "/releases" + windowQuery(d.P, "days", "since", "until", "tz") + "#" + (&url.URL{Fragment: releaseIDPrefix + version}).EscapedFragment()
}

// dayText is a day as "Oct 8", with the year when it is not now's.
func dayText(t, now time.Time, loc *time.Location) string {
	t = t.In(zoneOr(loc))
	if now.IsZero() || t.Year() == now.In(zoneOr(loc)).Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// topShare is the project with the largest share of a version's prompts,
// the first by name among equals; ok is false when it has none.
func topShare(m map[string]float64) (name string, share float64, ok bool) {
	for k, v := range m {
		if !ok || v > share || v == share && k < name {
			name, share, ok = k, v, true
		}
	}
	return name, share, ok
}

// flagshipCell is a version's flagship metric as the table shows it: the
// value, or for a rate with too little behind it the plain counts.
func flagshipCell(l format.Locale, d metrics.Def, row map[string]metrics.Metric) Cell {
	m, found := row[d.ID]
	switch {
	case !found:
		return Cell{Text: dash}
	case m.Value != nil:
		return Cell{Text: l.Value(d, m)}
	case d.Unit == "ratio" && m.Num != nil && m.Den != nil && *m.Den > 0:
		c := Cell{Text: l.Int(*m.Num) + " of " + l.Int(*m.Den)}
		if *m.Num > 0 { // nothing found is a plain "0 of N", as the canvas has it
			c.Class = "st-few"
		}
		return c
	case m.Status == metrics.StatusTooFew:
		return Cell{Text: dash, Class: "st-few"}
	}
	return Cell{Text: dash}
}

func releasesOf(d PageData) ReleasesPage {
	r := d.resultOf("releases")
	l := d.L
	p := ReleasesPage{Observational: observational, Head: fixedHead("releases", "Per version", "machine", d.CLI)}
	p.Head.Chip = "developer"
	p.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	for _, row := range r.Tables.Releases {
		ln := ReleaseLine{
			Version: row.Version, Href: d.versionHref(row.Version),
			Span:    dayText(row.First, r.GeneratedAt, d.Loc) + " → " + dayText(row.Last, r.GeneratedAt, d.Loc),
			Prompts: l.Int(row.N), Judged: dash, Top: Ref{None: true}, TopShare: dash,
			PerDay: l.Fixed(row.PromptsPerDay, 1), Bank: dash,
		}
		if row.Version == d.Frame.Version { // the running binary is in use now
			ln.Span = dayText(row.First, r.GeneratedAt, d.Loc) + " → now"
		}
		// The turns judged are those that showed memory and said what they used.
		if m, ok := row.Flagship["used.turn_rate"]; ok && m.Den != nil {
			ln.Judged = l.Int(*m.Den)
		}
		for _, id := range metrics.ReleaseFlagship {
			if def, ok := metrics.DefOf(id); ok {
				ln.Flags = append(ln.Flags, flagshipCell(l, def, row.Flagship))
			}
		}
		if name, share, ok := topShare(row.ProjectShares); ok {
			ln.Top, ln.TopShare = d.projectLink(name), l.Percent(share)
			if name == metrics.NoProject {
				ln.Top = Ref{None: true}
			}
			ln.Shares = strings.Join(shares(row.ProjectShares), "; ")
		}
		if row.BankSize != nil {
			ln.Bank = l.Int(*row.BankSize)
		}
		p.Rows = append(p.Rows, ln)
	}
	if len(p.Rows) == 0 {
		p.Note = NoData{Text: "No version recorded a prompt in this window."}
	} else {
		p.Foot = "Attributed per event to the binary that recorded it, not per day."
	}

	p.Settings = SettingsCard{Head: fixedHead("releases-settings", "Which settings are in effect?", "", "")}
	p.Settings.Head.Chip = "developer"
	for _, name := range slices.Sorted(maps.Keys(r.Tables.Settings)) {
		p.Settings.Rows = append(p.Settings.Rows, SettingLine{Name: name, Value: settingValue(l, r.Tables.Settings[name])})
	}
	if len(p.Settings.Rows) == 0 {
		p.Settings.Note = NoData{Text: "No setting is reported."}
	}
	p.Replay = ReplayCard{
		Head: fixedHead("releases-replay", "What would another setting do?", "", ""),
		Text: "Replay recorded recalls with another learned-ranking setting, without touching memory or config.toml:",
		Cmd:  replayCmd,
	}
	p.Replay.Head.Chip = "developer"
	p.Compare = d.compareOf(r)
	p.Used = d.usedOf(r)
	return p
}

// settingValue is a setting as a reader writes it: whole numbers grouped,
// the others with the decimals they have.
func settingValue(l format.Locale, v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if _, frac, ok := strings.Cut(s, "."); ok {
		return l.Fixed(v, len(frac))
	}
	return l.Fixed(v, 0)
}

// DefsPage is the Definitions page: the catalog, one row per metric, in a
// section per axis.
type DefsPage struct {
	Head   Head
	Note   NoData
	Tabs   []AxisTab
	Groups []DefGroup
}

// AxisTab is a link to an axis's section, with how many metrics it holds.
type AxisTab struct{ Label, Href, Count string }

// DefGroup is the metrics of one axis; ID is its section's anchor.
type DefGroup struct {
	ID, Title string
	Rows      []DefLine
}

// DefLine is a row of the catalog table; ID is the raw metric id, the
// row's anchor.
type DefLine struct {
	ID, Label, Question, Definition, Limits, Unit string
	Where, MinN, N, Smallest                      string
}

// axisOrder are the axes of the catalog, in the order the page lists them.
var axisOrder = []struct{ Family, Label string }{
	{"reach", "Found"}, {"quality", "Right"}, {"cost", "Cost"}, {"bank", "Bank"}, {"pipeline", "Flowing"}, {"review", "To review"},
}

func defsOf(d PageData) DefsPage {
	r := d.resultOf("defs")
	l := d.L
	p := DefsPage{Head: fixedHead("defs", "Catalog", "", d.CLI)}
	p.Head.Chip = "developer"
	byAxis := map[string][]DefLine{}
	for _, row := range r.Tables.Defs {
		def := row.Def
		ln := DefLine{
			ID: def.ID, Label: def.Label, Question: def.Question, Definition: def.Definition,
			Limits: limitsText(def, l), Unit: views.Word(views.UnitWords, def.Unit),
			Where: "this PC", MinN: dash, N: dash, Smallest: dash,
		}
		if def.ID == "hooks.failures" {
			ln.Definition = hooksFailuresWhat
		}
		if def.Scope == "server" {
			ln.Where = "server"
		}
		if def.MinN > 0 {
			ln.MinN = l.Int(int64(def.MinN))
		}
		if def.Unit == "ratio" {
			ln.N = l.Int(row.N)
			ln.Smallest = smallest(def, metrics.Metric{MDC: row.MDC}, l)
		}
		fam := views.Heading(def)
		if fam == "" { // a metric without a family is listed under Other
			fam = "other"
		}
		byAxis[fam] = append(byAxis[fam], ln)
	}
	add := func(family, label string) {
		if rows := byAxis[family]; len(rows) > 0 {
			id := "axis-" + family
			p.Groups = append(p.Groups, DefGroup{ID: id, Title: label, Rows: rows})
			p.Tabs = append(p.Tabs, AxisTab{Label: label, Href: "#" + id, Count: l.Int(int64(len(rows)))})
			delete(byAxis, family)
		}
	}
	for _, a := range axisOrder {
		add(a.Family, a.Label)
	}
	for _, fam := range slices.Sorted(maps.Keys(byAxis)) { // a family the page does not know yet
		add(fam, strings.ToUpper(fam[:1])+fam[1:])
	}
	if len(p.Groups) == 0 {
		p.Note = NoData{Text: "No metric is defined."}
	}
	return p
}

// What the comparison says about itself.
const (
	compareObservational = "Observational, not an experiment."
	compareCaveat        = "The two versions ran on different weeks, on different work, with different memory. Matching by project and place removes some of that, not all."
	compareSettingsTail  = "A difference may come from the setting, not the release."
	compareNoClear       = "No clear difference: the interval includes 0. That is not proof that they are the same."
	compareNotKnown      = "Settings of a version are not known: nothing was recorded before its first turn."
	compareMixDiffers    = "not comparable: the mix of projects differs"
)

// The gates the comparison applies to a group: the numbers its sentences
// name (the view decides; these only word it).
const (
	compareMinDays     = 7
	compareMinUnits    = 30
	compareMinSessions = 2
	comparePooled      = 10 // sessions per version for the pooled comparison
)

// unitWords name a measure's units.
type unitWords struct{ one, many, said1, saidN string }

// compareUnit is what a measure counts, in the plural and the singular, and
// what the sentence about too little data calls its units.
var compareUnit = map[string]unitWords{
	"used.turn_rate":     {"turn", "turns", "turn that said what it used", "turns that said what they used"},
	"recall.hit_rate":    {"prompt", "prompts", "prompt", "prompts"},
	"verdict.wrong_rate": {"verdict", "verdicts", "verdict", "verdicts"},
}

// CompareCard is the card that compares two versions: the picker, then what
// the view answered.
type CompareCard struct {
	Head   Head
	Form   CompareForm
	Hint   string // no comparison asked: what to do
	Intro  string // which versions, on which measure
	Few    string // the comparison is too few: why
	Result *CompareResult
}

// CompareForm is the picker: a GET form to the Releases page, which turns
// the two versions into one comparison.
type CompareForm struct {
	Keep          [][2]string // the window's parameters, carried
	A, B, Measure []Option
}

// Option is an option of a select.
type Option struct {
	Value, Text string
	Selected    bool
}

// CompareResult is a comparison that reads.
type CompareResult struct {
	Observational, Caveat string
	Settings              []SettingDiffLine
	SettingsTail          string
	Unknown               string
	Big, Sub              string
	Pill                  Word
	Raw                   string
	NoClear               string
	Chart                 template.HTML
	Caption               string
	A, B                  string
	Rows                  []CompareRow
	Pooled, Unmatched     CompareRow
	Weights               string
}

// SettingDiffLine is a setting that differs: its name, then the rest of its
// sentence.
type SettingDiffLine struct{ Key, Rest string }

// CompareRow is a row of the comparison's table; Label spans the first two
// columns of the pooled and unmatched rows.
type CompareRow struct {
	Label, Project, Shown string
	A, B                  CompareCell
	Diff, Interval        string
	Reading, Dir          string
	Strong, Muted         bool
}

// CompareCell is one version's result in a group: its share, or a note that
// it has too little, and the counts behind it.
type CompareCell struct {
	Few          bool
	Rate, Counts string
}

// signedPts is a difference of shares in whole points with its sign and a
// true minus: "+11", "−6", "0".
func signedPts(frac float64) string {
	r := math.Round(frac * 100)
	switch {
	case r > 0:
		return fmt.Sprintf("+%.0f", r)
	case r < 0:
		return fmt.Sprintf("−%.0f", -r)
	}
	return "0"
}

// ptsWord is signedPts with its unit: "+11 pts", "+1 pt".
func ptsWord(frac float64) string {
	if r := math.Round(frac * 100); r == 1 || r == -1 {
		return signedPts(frac) + " pt"
	}
	return signedPts(frac) + " pts"
}

// compareCLI is the command that gives the comparison on the page.
func (d PageData) compareCLI(c *metrics.Comparison) string {
	if c == nil {
		return d.CLI + " --compare A,B"
	}
	s := d.CLI + " --compare " + quoteArg(c.A+","+c.B)
	if d.P.Measure != "" {
		s += " --measure " + quoteArg(d.P.Measure)
	}
	return s
}

func (d PageData) compareOf(r *metrics.Result) CompareCard {
	cmp := r.Tables.Compare
	l := d.L
	c := CompareCard{Head: fixedHead("releases-compare", "Do two versions differ?", "machine", d.compareCLI(cmp))}
	c.Head.Chip = "developer"
	c.Head.Info = Info{
		Label: "About comparing two versions", Name: "Comparing two versions",
		What:   "Turns are matched by project and by where memory was shown. Each matched pair gives a difference; they are pooled, weighted by size, with a 95 % interval that counts each session once (turns of one session are not independent).",
		Limits: "Shown when each version has at least 7 days and 30 turns that said what they used in that project and place.",
		Paras:  []string{"“Differs” when the interval leaves out 0, “no clear difference” otherwise. Never “better”: weeks, work and memory differ."},
		Term:   "stats releases --compare A,B · stratified, clustered by session",
	}
	var versions []string
	for _, row := range r.Tables.Releases {
		versions = append(versions, row.Version)
	}
	a, b, measure := "", "", d.P.Measure
	if cmp != nil {
		a, b, measure = cmp.A, cmp.B, cmp.Measure
		for _, v := range []string{a, b} {
			if !slices.Contains(versions, v) { // asked for, though it ran outside the window
				versions = append(versions, v)
			}
		}
	} else if len(versions) > 1 {
		a, b = versions[1], versions[0]
	}
	if measure == "" {
		measure = metrics.CompareMeasures[0]
	}
	c.Form.Keep = slices.Clone(d.Frame.Keep)
	for _, v := range versions {
		c.Form.A = append(c.Form.A, Option{Value: v, Text: v, Selected: v == a})
		c.Form.B = append(c.Form.B, Option{Value: v, Text: v, Selected: v == b})
	}
	for _, id := range metrics.CompareMeasures {
		def, _ := metrics.DefOf(id)
		c.Form.Measure = append(c.Form.Measure, Option{Value: id, Text: def.Label, Selected: id == measure})
	}
	if cmp == nil {
		c.Hint = "Pick two versions to see whether they differ."
		if len(r.Tables.Releases) < 2 {
			c.Hint = "Two versions need to have run in this window to compare them: pick a longer window."
		}
		return c
	}

	unit, ok := compareUnit[cmp.Measure]
	if !ok {
		unit = compareUnit[metrics.CompareMeasures[0]]
	}
	label := cmp.Measure
	if def, ok := metrics.DefOf(cmp.Measure); ok {
		label = def.Label
	}
	c.Intro = "Comparing " + cmp.A + " with " + cmp.B + " on " + lowerFirst(label) + ":"
	if cmp.TooFew != nil || cmp.Diff == nil || cmp.CI == nil {
		c.Head.Pill = Word{Word: "too few", Class: "st-few"}
		c.Few = d.tooFewText(r, *cmp, unit)
		return c
	}

	res := &CompareResult{Observational: compareObservational, Caveat: compareCaveat, SettingsTail: compareSettingsTail,
		A: cmp.A, B: cmp.B}
	for _, s := range cmp.SettingsDiffer {
		val := func(v *float64) string {
			if v == nil {
				return "none"
			}
			return settingValue(l, *v)
		}
		line := SettingDiffLine{Key: s.Key, Rest: "was " + val(s.A) + " under " + cmp.A + " and " + val(s.B) + " under " + cmp.B}
		if s.ChangedAt != nil {
			line.Rest += " (changed " + dayText(*s.ChangedAt, r.GeneratedAt, d.Loc) + ")"
		}
		res.Settings = append(res.Settings, line)
	}
	if cmp.SettingsUnknown {
		res.Unknown = compareNotKnown
	}
	res.Big = ptsWord(*cmp.Diff)
	res.Sub = "(" + signedPts(cmp.CI[0]) + " to " + signedPts(cmp.CI[1]) + ") " + cmp.B + " minus " + cmp.A + ", matched"
	res.Pill = Word{Word: views.Word(views.ReadingWords, cmp.Reading), Class: "st-info"}
	if cmp.RawDisagrees && cmp.RawA.Den > 0 && cmp.RawB.Den > 0 {
		ra, rb := float64(cmp.RawA.Num)/float64(cmp.RawA.Den), float64(cmp.RawB.Num)/float64(cmp.RawB.Den)
		res.Raw = "Unmatched, the raw numbers say the opposite, " + ptsWord(rb-ra) + " (" + l.Percent(ra) + " → " + l.Percent(rb) +
			"): the mix of projects differs between the two versions. Read the matched figure."
	}
	if cmp.Reading == "no_clear_difference" {
		res.NoClear = compareNoClear
	}

	var forest []chart.ForestRow
	var weights []string
	eligible := 0
	for _, s := range cmp.Strata {
		name := s.Project + " · " + pathWord(s.Path)
		if s.Path == "unknown" {
			name = s.Project + " · not shown to Claude itself"
		}
		row := CompareRow{Project: s.Project, Shown: strings.TrimPrefix(name, s.Project+" · ")}
		fr := chart.ForestRow{Name: name}
		row.A = d.sideCell(s.A, s.Why, unit.many)
		row.B = d.sideCell(s.B, s.Why, unit.many)
		if s.Eligible && s.Diff != nil && s.CI != nil {
			eligible++
			row.Diff, row.Interval = ptsWord(*s.Diff), "("+signedPts(s.CI[0])+" to "+signedPts(s.CI[1])+")"
			row.Reading, row.Strong, row.Dir = readingOf(s.Reading, *s.CI, cmp.B)
			fr.Diff, fr.CI = pct(*s.Diff), &[2]float64{s.CI[0] * 100, s.CI[1] * 100}
			if s.Weight != nil {
				weights = append(weights, l.Fixed(*s.Weight, 0))
			}
		} else {
			row.Diff, row.Muted = dash, true
			row.Reading = map[string]string{
				"days":     "needs " + strconv.Itoa(compareMinDays) + " days in each version",
				"units":    "needs " + strconv.Itoa(compareMinUnits) + " " + unit.many + " in each version",
				"sessions": "needs " + strconv.Itoa(compareMinSessions) + " sessions in each version",
			}[s.Why]
			if row.Reading == "" {
				row.Reading = dash
			}
			fr.FewText = d.fewGroupText(s, cmp, unit.many)
		}
		res.Rows = append(res.Rows, row)
		forest = append(forest, fr)
	}
	pooled := CompareRow{Label: "Matched, pooled (" + strconv.Itoa(eligible) + " of " + strconv.Itoa(len(cmp.Strata)) + " pairs)",
		Diff: ptsWord(*cmp.Diff), Interval: "(" + signedPts(cmp.CI[0]) + " to " + signedPts(cmp.CI[1]) + ")"}
	pooled.Reading, pooled.Strong, pooled.Dir = readingOf(cmp.Reading, *cmp.CI, cmp.B)
	if cmp.PooledA != nil {
		pooled.A.Rate = l.Percent(*cmp.PooledA)
	}
	if cmp.PooledB != nil {
		pooled.B.Rate = l.Percent(*cmp.PooledB)
	}
	res.Pooled = pooled
	forest = append(forest, chart.ForestRow{Name: "Matched, pooled", Diff: pct(*cmp.Diff), CI: &[2]float64{cmp.CI[0] * 100, cmp.CI[1] * 100}, Pooled: true})

	raw := CompareRow{Label: "All " + unit.many + ", unmatched", Muted: true, Diff: dash, Reading: "raw figures, not matched"}
	if cmp.RawDisagrees {
		raw.Reading = compareMixDiffers
	}
	if cmp.RawA.Den > 0 && cmp.RawB.Den > 0 {
		ra, rb := float64(cmp.RawA.Num)/float64(cmp.RawA.Den), float64(cmp.RawB.Num)/float64(cmp.RawB.Den)
		raw.A = CompareCell{Rate: l.Percent(ra), Counts: l.Int(cmp.RawA.Num) + " of " + l.Int(cmp.RawA.Den)}
		raw.B = CompareCell{Rate: l.Percent(rb), Counts: l.Int(cmp.RawB.Num) + " of " + l.Int(cmp.RawB.Den)}
		raw.Diff = ptsWord(rb - ra)
	}
	res.Unmatched = raw
	if len(weights) > 0 {
		res.Weights = "Pooled rates are each version's rate in the matched pairs, weighted by pair size (the two versions' " + unit.many +
			" combined as a·b / (a + b): " + joinAnd(weights) + "), so the same weights apply to both versions."
	}
	res.Caption = "Each project and place, and the matched total"
	res.Chart = chart.Forest{Label: "Difference in " + lowerFirst(label) + ", " + cmp.B + " minus " + cmp.A + ", per project and place, with 95 % intervals",
		B: cmp.B, Rows: forest, More: d.compareCLI(cmp) + " --json"}.HTML()
	c.Result = res
	return c
}

// pct is a share as a number of percentage points.
func pct(frac float64) *float64 { v := frac * 100; return &v }

// joinAnd is "46", "46 and 51", "46, 51 and 60".
func joinAnd(xs []string) string {
	if len(xs) < 2 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// readingOf words a reading of an interval: the reading, whether it is
// stressed, and for a difference which version is higher.
func readingOf(reading string, ci [2]float64, b string) (word string, strong bool, dir string) {
	word = views.Word(views.ReadingWords, reading)
	if reading != "differs" {
		return word, false, ""
	}
	if ci[0] > 0 {
		return word, true, "higher in " + b
	}
	return word, true, "lower in " + b
}

// sideCell is one version's side of a group. A side that misses the gate the
// group failed is "too few", with the counts that show it.
func (d PageData) sideCell(x metrics.StratumSide, why, unit string) CompareCell {
	l := d.L
	few := why == "days" && x.Days < compareMinDays || why == "units" && x.Den < compareMinUnits
	counts := l.Int(x.Num) + " of " + l.Int(x.Den) + " " + unit
	switch {
	case few && why == "days": // the days are the reason: say them whole
		counts += ", " + views.CountOf(int64(x.Days), "day", "days")
	case !few:
		counts += ", " + l.Int(int64(x.Days)) + " d"
	}
	cell := CompareCell{Few: few, Counts: counts}
	if !few && x.Den > 0 {
		cell.Rate = l.Percent(float64(x.Num) / float64(x.Den))
	}
	return cell
}

// fewGroupText says why a group is not drawn: the version short of the gate
// it failed and what it has.
func (d PageData) fewGroupText(s metrics.StratumRow, c *metrics.Comparison, unit string) string {
	switch s.Why {
	case "days":
		v, x := c.A, s.A
		if s.B.Days < s.A.Days {
			v, x = c.B, s.B
		}
		return "too few: " + v + " ran " + views.CountOf(int64(x.Days), "day", "days") + ", " + strconv.Itoa(compareMinDays) + " needed"
	case "units":
		v, x := c.A, s.A
		if s.B.Den < s.A.Den {
			v, x = c.B, s.B
		}
		return "too few: " + v + " has " + views.CountOf(x.Den, unitOne(unit), unit) + ", " + strconv.Itoa(compareMinUnits) + " needed"
	case "sessions":
		return "too few: each version needs " + strconv.Itoa(compareMinSessions) + " sessions"
	}
	return "too few"
}

// unitOne is the singular of a plural unit word of compareUnit.
func unitOne(many string) string {
	for _, u := range compareUnit {
		if u.many == many {
			return u.one
		}
	}
	return many
}

// tooFewText is the sentence of a comparison the view cannot make.
func (d PageData) tooFewText(r *metrics.Result, c metrics.Comparison, unit unitWords) string {
	t := c.TooFew
	if t == nil {
		return "Too few to compare."
	}
	s := "Too few to compare. " + t.Version + " has run " + views.CountOf(int64(t.Days), "day", "days") + " with " +
		views.CountOf(int64(t.Units), unit.said1, unit.saidN) + "; each version needs " + strconv.Itoa(compareMinDays) + " days and " +
		strconv.Itoa(compareMinUnits) + " such " + unit.many + " in at least one shared project and place."
	if t.Why == "sessions" {
		sessions := c.SessionsB
		if t.Version == c.A {
			sessions = c.SessionsA
		}
		s += " Together they need " + strconv.Itoa(comparePooled) + " sessions in each version; " + t.Version + " has " + strconv.Itoa(sessions) + "."
	}
	if t.RetryAfter != nil {
		s += " Try again around " + dayText(*t.RetryAfter, r.GeneratedAt, d.Loc) + "."
	}
	return s
}

// UsedCard is the chart of the turns where Claude used memory, per day, with
// the new versions and the settings changes marked.
type UsedCard struct {
	Head  Head
	Chart template.HTML
}

func (d PageData) usedOf(r *metrics.Result) UsedCard {
	u := UsedCard{Head: fixedHead("releases-used", "When did Claude use memory?", "machine", d.CLI)}
	u.Head.Aside = "new versions and settings changes are marked"
	label := "Turns where Claude used memory, per day, with new versions and settings changes marked"
	v, err := metricOf(r, "used.turn_rate")
	if err != nil || !v.Found || len(v.M.Series) == 0 {
		u.Chart = chart.Line{Label: label, Size: chart.Full, Empty: "No days to draw in this window."}.HTML()
		return u
	}
	class, name := styleOf(v)
	c := chart.Line{Label: label, Size: chart.Full, XName: "Day", X: xsOf(v.M.Series), Rate: true, More: d.more(),
		Empty: "No days to draw in this window.", NName: barName(v.ID), Fmt: d.L.Percent, Merge: chart.MergeRate}
	s := chart.Series{Name: name, Class: class, Band: true}
	for _, p := range v.M.Series {
		c.N = append(c.N, countOf(p.Den))
		s.Y = append(s.Y, chart.Y{V: p.Value, Num: countOf(p.Num), Den: countOf(p.Den)})
	}
	c.Series = []chart.Series{s}
	at := map[string]int{}
	for i, p := range v.M.Series {
		at[pointKey(p)] = i
	}
	for _, m := range r.Tables.Markers {
		i, ok := at[m.At.In(zoneOr(d.Loc)).Format(dayKey)]
		if !ok {
			continue
		}
		lines := d.markersOf([]metrics.Marker{m}).Lines
		if len(lines) == 0 {
			continue
		}
		ln := lines[0]
		day := dayText(m.At, r.GeneratedAt, d.Loc)
		switch ln.Kind {
		case "release":
			title := day + ": new version " + ln.Version.Text
			if ln.Previous != "" {
				title += " (was " + ln.Previous + ")"
			}
			c.Markers = append(c.Markers, chart.Mark2{X: i, Kind: "release", Label: ln.Version.Text, Title: title})
		case "config":
			var ch []string
			for _, x := range ln.Changes {
				ch = append(ch, x.Key+" "+x.From+" → "+x.To)
			}
			c.Markers = append(c.Markers, chart.Mark2{X: i, Kind: "config", Label: "settings changed",
				Title: day + ": settings changed, recorded at " + ln.At.Text + " (" + strings.Join(ch, ", ") + ")"})
		}
	}
	u.Chart = c.HTML()
	return u
}

// pickVersions turns the Releases picker's a, b and measure into the
// comparison the view takes: compare=A,B with the measure first, then the
// other parameters. It returns the page to redirect to, or "" when the query
// holds no picked version. q is left as the view reads it, so the same
// refusals apply (two different versions, a known measure).
func pickVersions(path string, q url.Values) string {
	_, hasA := q["a"]
	_, hasB := q["b"]
	if !hasA && !hasB {
		return ""
	}
	a, b, measure := q.Get("a"), q.Get("b"), q.Get("measure")
	q.Del("a")
	q.Del("b")
	q.Del("measure")
	q.Set("compare", a+","+b)
	if measure != "" {
		q.Set("measure", measure)
	}
	rest := url.Values{} // the window and the filters travel with the comparison
	for k, vs := range q {
		if k != "compare" && k != "measure" {
			rest[k] = vs
		}
	}
	base, _, _ := strings.Cut(path, "?")
	out := base + "?compare=" + url.QueryEscape(a) + "," + url.QueryEscape(b)
	if measure != "" {
		out += "&measure=" + url.QueryEscape(measure)
	}
	if len(rest) > 0 {
		out += "&" + rest.Encode()
	}
	return out
}
