package dashboard

import (
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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
		p.Foot = "Attributed per event to the binary that recorded it, not per day. Comparing two versions with matched projects comes later."
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
