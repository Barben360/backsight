package dashboard

import (
	"html/template"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The pages about one session and one memory carry ids, words and numbers
// only: every value the view stores as a name (a path, an outcome, a
// decision, a reason, a check) is shown as the page's word for it, and one
// it has no word for is shown as a neutral word, never as stored. The text
// of a memory is the text page's alone.

// ----- /sessions/{id} -----

// TurnChip is a memory shown in a turn: a link to the memory with its
// short id, its rank, where it was shown and what became of it, all in words.
type TurnChip struct {
	Href, Title string
	Class       string // used, ignored or "": the border
	Swatch      string // the class of the shape's swatch
	ID, Rank    string // Rank is "#3", or "" without one
	Where, Word string
}

// TurnLine is a line of a turn: a name and either a sentence (Text, in
// Class) or the chips of the memories.
type TurnLine struct {
	Key, Text, Class string
	Chips            []TurnChip
}

// TurnView is a turn of the rail.
type TurnView struct {
	Name  string
	Time  format.Time
	Lines []TurnLine
}

// LegendItem is a swatch and the word it stands for.
type LegendItem struct{ Swatch, Word string }

// TurnsCard is the card of the turn rail; Note says why there is none.
type TurnsCard struct {
	Head   Head
	Legend []LegendItem
	Note   NoData
	Rows   []TurnView
}

// SessionDetail is the page about one session.
type SessionDetail struct {
	Found   bool
	ID      Ref
	Project Ref
	Span    format.Time
	Turns   string // "5 turns"
	Sent    Word
	Figs    []Fig
	Rail    TurnsCard
}

// addedPaths are the paths whose shows are added to a prompt or a question.
var addedPaths = []string{"prompt", "gate"}

// checkLines say how a turn ended: the words and the class of the line.
var checkLines = map[string]Word{
	"declared":   {views.CheckWords["declared"], "st st-ok"},
	"undeclared": {views.CheckWords["undeclared"], "st st-watch"},
	"skipped":    {views.CheckWords["skipped"], "muted"},
	"pending":    {"waiting for the end-of-turn check", "muted"},
}

// turnLines are the lines of turn t, in the order they happen.
func (d PageData) turnLines(t metrics.TurnRow) []TurnLine {
	l := d.L
	var lines []TurnLine
	add := func(key, text, class string) { lines = append(lines, TurnLine{Key: key, Text: text, Class: class}) }
	chips := func(key string, in func(path string) bool) {
		var cs []TurnChip
		for _, c := range t.Shows {
			if in(c.Path) {
				cs = append(cs, d.chipOf(c))
			}
		}
		if len(cs) > 0 {
			lines = append(lines, TurnLine{Key: key, Chips: cs})
		}
	}
	chips("At session start", func(p string) bool { return p == "briefing" })

	added := 0
	for _, c := range t.Shows {
		if slices.Contains(addedPaths, c.Path) {
			added++
		}
	}
	decision := wordOr(views.DecisionWords, t.Decision, "")
	switch {
	case t.Decision == "skip":
		text := decision
		if t.Reason != "" {
			text += ": " + wordOr(views.SkipWords, t.Reason, views.SkipWords["unknown"])
		}
		add("Before the prompt", text, "muted")
	case t.Decision == "recall" && t.Empty != "":
		add("Before the prompt", decision+": nothing added ("+emptyWord(t.Empty)+")", "muted")
	case t.Decision == "recall" && added > 0:
		add("Before the prompt", decision+": "+l.Int(int64(added))+" added", "")
	case t.Decision == "recall":
		add("Before the prompt", decision, "")
	}
	if t.Searches > 0 {
		call := " memory tool calls"
		if t.Searches == 1 {
			call = " memory tool call"
		}
		add("Claude's search", l.Int(t.Searches)+call, "")
	}
	chips("Memories shown", func(p string) bool { return p != "briefing" && p != "read" })
	chips("Opened by Claude", func(p string) bool { return p == "read" })
	if len(t.Declared) > 0 {
		said := make([]string, len(t.Declared))
		for i, v := range t.Declared {
			said[i] = wordOr(views.DeclaredWords, v, views.DeclaredWords["invalid"])
		}
		add("Claude said", strings.Join(said, ", "), "")
	}
	switch w, ok := checkLines[t.Check]; {
	case ok:
		add("End of turn", w.Word, w.Class)
	case t.Check != "":
		add("End of turn", "result not recorded", "muted")
	case len(t.Shows) == 0:
		add("End of turn", "no memory shown, nothing to say", "muted")
	default:
		add("End of turn", "nothing to check", "muted")
	}
	return lines
}

// emptyWord is why a prompt got nothing, in words.
func emptyWord(reason string) string {
	if slices.Contains([]string{"no_candidates", "below_floor", "all_seen", "capped", "unknown"}, reason) {
		return views.EmptyWords.Replace(reason)
	}
	return views.EmptyWords.Replace("unknown")
}

// chipOf is a shown memory as the rail draws it.
func (d PageData) chipOf(c metrics.ShowChip) TurnChip {
	shape := shapeOf(c.Outcome)
	ref := idLink("/memories/", c.MemoryID)
	ch := TurnChip{Href: ref.Href, ID: ref.Text, Swatch: swatchOf(shape), Where: pathWord(c.Path), Word: chart.ShapeWords[shape]}
	if shape == "used" || shape == "ignored" {
		ch.Class = shape
	}
	title := ref.Full
	if c.Rank != nil && *c.Rank > 0 {
		ch.Rank = "#" + d.L.Int(*c.Rank)
		title += " · rank " + d.L.Int(*c.Rank)
	}
	ch.Title = title + " · " + ch.Where + " · " + ch.Word
	return ch
}

// turnView is turn t of the rail.
func (d PageData) turnView(t metrics.TurnRow) TurnView {
	v := TurnView{Name: "Turn " + dash, Lines: d.turnLines(t)}
	if t.N != nil {
		v.Name = "Turn " + d.L.Int(*t.N)
	}
	if !t.At.IsZero() {
		at := t.At.In(zoneOr(d.Loc))
		v.Time = format.Time{Text: at.Format("15:04"), Datetime: at.Format(time.RFC3339), Title: when(d.Loc, t.At)}
	}
	return v
}

// tokensAbout is the tokens that many characters make, a quarter of them.
func tokensAbout(chars int64) int64 { return int64(math.Round(float64(chars) / 4)) }

func sessionOf(d PageData) SessionDetail {
	r := d.resultOf("sessions")
	t := r.Tables.Session
	l := d.L
	id := d.ID
	if t != nil && t.Totals.ID != "" {
		id = t.Totals.ID
	}
	pg := SessionDetail{ID: idLink("/sessions/", id)}
	pg.Rail.Head = fixedHead("session-turns", "Turn by turn", "machine", cliFor("sessions", id, allTimeParams(d.P)))
	if t == nil {
		pg.Rail.Note = NoData{Text: "No turn of this session was recorded."}
		return pg
	}
	pg.Found = true
	tot := t.Totals
	pg.Project = d.projectLink(tot.Project)
	if !tot.Start.IsZero() {
		pg.Span = format.Rel(r.GeneratedAt, tot.Start, d.Loc)
		if !tot.Last.IsZero() && tot.Last.After(tot.Start) {
			pg.Span.Title += " → " + format.Clock(r.GeneratedAt, tot.Last, d.Loc)
		}
	}
	pg.Turns = format.Count(l, int64(len(t.Turns)), "turn", "turns")
	if tot.Sent != "" {
		pg.Sent = Word{views.Word(views.SentWords, tot.Sent), sentClass[tot.Sent]}
		if _, ok := views.SentWords[tot.Sent]; !ok {
			pg.Sent = Word{views.SentWords["unknown"], ""}
		}
	}
	distinct := map[string]bool{}
	for _, turn := range t.Turns {
		for _, c := range turn.Shows {
			distinct[c.MemoryID] = true
		}
	}
	pg.Figs = []Fig{
		{Label: "Prompts", Value: l.Int(tot.Prompts), Sub: l.Int(tot.PromptsWithMemory) + " got memory"},
		{Label: "Memories shown", Value: l.Int(tot.Shown), Sub: l.Int(int64(len(distinct))) + " distinct"},
		{Label: "Used by Claude", Value: l.Int(tot.Used), Swatch: "f-used"},
		{Label: "Reported wrong", Value: l.Int(tot.Wrong), Icon: "bad"},
		{Label: "Context added", Value: l.Int(tot.ContextChars), Sub: "characters · ≈ " + l.Int(tokensAbout(tot.ContextChars)) + " tokens"},
	}
	pg.Rail.Head.Pager = pagerOf(d.Frame.Path, pagingOf(d.P, len(t.Turns)))
	for _, o := range outcomeShapes {
		pg.Rail.Legend = append(pg.Rail.Legend, LegendItem{Swatch: o.class, Word: chart.ShapeWords[o.shape]})
	}
	for _, turn := range turnsPage(t.Turns, d.P) {
		pg.Rail.Rows = append(pg.Rail.Rows, d.turnView(turn))
	}
	if len(t.Turns) == 0 {
		pg.Rail.Note = NoData{Text: "No turn of this session was recorded."}
	}
	return pg
}

// ----- /memories/{id} -----

// DoLine is a thing the reader can do with a memory, as a command.
type DoLine struct{ Label, Command string }

// StripCard is the card of the strip timeline; AllShows is the link that
// asks for every show when the strip draws only some.
type StripCard struct {
	Head     Head
	HTML     template.HTML
	AllShows string
}

// MemoryDetail is the page about one memory.
type MemoryDetail struct {
	Found         bool
	ID            Ref
	FullID        string // what the copy button copies
	Project       Ref
	Kind, Swatch  string
	Created       format.Time
	Server        Word
	ServerMissing bool // the server did not answer: no kind, creation or state
	State         Word // what was said of the memory, with the icon in Icon
	StateIcon     string
	Figs          []Fig
	TextHref      string // "" for an id that cannot be a path segment
	TextHead      Head
	Strip         StripCard
	RankHead      Head
	Rank          template.HTML
	DoHead        Head
	Do            []DoLine
}

// serverClass colours a memory's state on the server.
var serverClass = map[string]string{"active": "st-ok", "valid": "st-ok", "invalidated": "st-watch"}

// stateNotes are the sentences under the text card for a memory with a state of its own.
var stateNotes = map[string]struct{ text, class, icon string }{
	"wrong":      {"This memory was reported wrong or out of date.", "st-watch", "watch"},
	"hidden":     {"This memory is hidden: Claude is not shown it until it is brought back.", "st-few", "info"},
	"never_used": {"This memory is often shown but never used.", "st-learn", "info"},
}

// doLabel names a command of the memory page for the reader.
func doLabel(cmd string) string {
	switch {
	case strings.Contains(cmd, "--restore"):
		return "Bring it back"
	case strings.HasPrefix(cmd, "backsight forget"):
		return "Hide it from recall"
	case strings.HasPrefix(cmd, "backsight read"):
		return "Read it in the terminal"
	}
	return "Run in a terminal"
}

// allShowsHref is path asking for the largest page of shows, from the first.
func allShowsHref(path string) string {
	base := without(path, "limit", "offset")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "limit=" + strconv.Itoa(metrics.MaxLimit)
}

// rankingNote says what the learned ranking does to a memory.
func rankingNote(word string) string {
	switch word {
	case "raised":
		return "shown a little higher"
	case "lowered":
		return "shown a little lower"
	}
	return "no effect on the order"
}

func memoryOf(d PageData) MemoryDetail {
	r := d.resultOf("memories")
	m := r.Tables.Memory
	l := d.L
	id := d.ID
	if m != nil && m.Row.ID != "" {
		id = m.Row.ID
	}
	pg := MemoryDetail{ID: idLink("/memories/", id), FullID: views.Plain(id, ""), TextHref: pathHref("/memories/", id, "/text")}
	pg.TextHead = fixedHead("memory-text", "What does it say?", "server", "")
	pg.Strip.Head = withInfo(fixedHead("memory-strip", "When and where was it shown?", "machine", cliFor("memories", id, allTimeParams(d.P))),
		"About this timeline", "Strip timeline", "ledger shows · path · outcome · rank · boost",
		"One mark per time this memory was shown, on the row of where it was shown. Shape says what Claude did with it. Hover or focus a mark for its day, its rank, and its rank without the learned ranking.")
	pg.RankHead = fixedHead("memory-rank", "How high was it ranked when shown?", "machine", "")
	pg.DoHead = fixedHead("memory-do", "What can I do with it?", "", "")
	if m == nil {
		pg.Strip.HTML = stripOf(l, d.more(), nil, r.GeneratedAt, d.Loc)
		pg.Rank = rankOf(l, d.more(), nil)
		return pg
	}
	pg.Found = true
	row := m.Row
	pg.Project = d.projectLink(row.Project)
	for _, k := range glanceKinds {
		if k.key == m.Kind {
			pg.Kind, pg.Swatch = k.one, k.class
		}
	}
	if m.Kind != "" && pg.Kind == "" {
		pg.Kind = "memory of another kind"
	}
	pg.Created = d.rel(r.GeneratedAt, m.Created)
	if m.ServerState != "" {
		pg.Server = Word{wordOr(views.ServerStateWords, m.ServerState, "state not recorded"), serverClass[m.ServerState]}
	}
	pg.ServerMissing = m.Kind == "" && m.ServerState == "" && m.Created == nil
	if n, ok := stateNotes[row.State]; ok {
		pg.State, pg.StateIcon = Word{n.text, n.class}, n.icon
	}

	last := func(prefix string, t *time.Time, never string) string {
		if t == nil || t.IsZero() {
			return never
		}
		return prefix + d.rel(r.GeneratedAt, t).Text
	}
	rate := dash
	if row.Rate != nil {
		rate = l.Percent(*row.Rate)
	}
	ranking := rankingWord(row.Boost)
	boost := Fig{Label: "Learned ranking now", Value: ranking, Sub: rankingNote(ranking), WordValue: true}
	if d.Frame.Prefs.Dev {
		boost.Dev = signed(row.Boost)
	}
	pg.Figs = []Fig{
		{Label: "Shown", Value: l.Int(row.Shown), Sub: last("last ", row.LastShown, "never shown")},
		{Label: "Used by Claude", Value: l.Fixed(row.Used, 1), Swatch: "f-used", Sub: "recent-weighted · " + last("last ", row.LastUsed, "never used")},
		{Label: "Shown, then ignored", Value: l.Fixed(row.Ignored, 1), Swatch: "f-ignored", Sub: "recent-weighted"},
		{Label: "Used when judged", Value: rate},
		boost,
	}

	// The strip draws this page of shows; the oldest show is on the last page
	// only, so only there does the memory's creation bound the strip.
	shown := *m
	shown.Retirements = pagedRetirements(m, r.Tables.Page)
	if pageOf := r.Tables.Page; pageOf != nil && pageOf.Offset+len(m.Shows) < pageOf.Total {
		shown.Created = nil
	}
	pg.Strip.HTML = stripOf(l, d.more(), &shown, r.GeneratedAt, d.Loc)
	if pageOf := r.Tables.Page; len(m.Shows) == 0 && pageOf != nil && pageOf.Total > 0 {
		pg.Strip.HTML = `<p class="empty">No show on this page.</p>`
	}
	pg.Strip.Head.Pager = pagerOf(d.Frame.Path, r.Tables.Page)
	if pageOf := r.Tables.Page; pageOf != nil && pageOf.Total > len(m.Shows) && pageOf.Limit < metrics.MaxLimit {
		pg.Strip.AllShows = allShowsHref(d.Frame.Path)
	}
	pg.Rank = rankOf(l, d.more(), m.RankHistogram)
	for _, c := range m.Commands {
		pg.Do = append(pg.Do, DoLine{Label: doLabel(c), Command: c})
	}
	return pg
}

// memlink is the link to a memory's page as an id reference.
func memlink(id string) Ref { return idLink("/memories/", id) }

// allTimeParams are p without the window: the pages about one session or
// one memory read the whole history.
func allTimeParams(p views.Params) views.Params {
	p.Days, p.DaysSet, p.Since, p.Until, p.TZ = 0, false, "", "", ""
	return p
}

// pagedRetirements are the retirements the strip draws for the page of
// shows it holds: those inside the span of the page's shows. The first page
// runs on to now, and the last one back to the memory's creation (all of
// them when it is not known), so a hide older than a page of recent shows
// does not stretch that page.
func pagedRetirements(m *metrics.MemoryPage, pg *metrics.Page) []metrics.RetireMark {
	if pg == nil || len(m.Shows) == 0 {
		return m.Retirements
	}
	lo, hi := m.Shows[0].At, m.Shows[0].At
	for _, s := range m.Shows {
		if s.At.Before(lo) {
			lo = s.At
		}
		if s.At.After(hi) {
			hi = s.At
		}
	}
	first, last := pg.Offset <= 0, pg.Offset+len(m.Shows) >= pg.Total
	var out []metrics.RetireMark
	for _, r := range m.Retirements {
		switch {
		case !first && r.At.After(hi):
		case !last && r.At.Before(lo):
		case last && m.Created != nil && r.At.Before(*m.Created) && r.At.Before(lo):
		default:
			out = append(out, r)
		}
	}
	return out
}
