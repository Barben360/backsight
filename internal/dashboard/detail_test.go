package dashboard

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

var detailBase = time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)

// detailNode renders a page of the dashboard on its golden, with the
// request path the page's links are built from, after edit.
func detailNode(t *testing.T, name, path string, p views.Params, edit func(d *PageData)) *node {
	t.Helper()
	return parseHTML(t, render(t, name, func(d *PageData) {
		d.L, d.P, d.Frame.Path = en, p, path
		d.CLI = "backsight stats " + name
		if edit != nil {
			edit(d)
		}
	}))
}

// turnsOf is n numbered turns, each with one memory shown and used.
func turnsOf(n int) []metrics.TurnRow {
	out := make([]metrics.TurnRow, n)
	for i := range out {
		out[i] = metrics.TurnRow{N: ptrI(int64(i + 1)), At: detailBase.Add(time.Duration(i) * time.Minute), Decision: "recall",
			Shows: []metrics.ShowChip{{MemoryID: fmt.Sprintf("mem-%04d-0000-0000", i), Path: "prompt", Outcome: "credit", Rank: ptrI(1)}},
			Check: "declared"}
	}
	return out
}

// lineOf is the text of the line named key in a turn of the rail; ok is
// false when the turn has no such line.
func lineOf(turn *node, key string) (string, bool) {
	for _, l := range turn.find("div.turn-line") {
		if k := l.first("span.k"); k != nil && k.text() == key {
			return strings.TrimSpace(strings.TrimPrefix(l.text(), key)), true
		}
	}
	return "", false
}

// textOrNone is the text of n, "" for no element.
func textOrNone(n *node) string {
	if n == nil {
		return ""
	}
	return n.text()
}

// allText is all the text of a subtree, <code> included.
func allText(n *node) string {
	var b strings.Builder
	for _, it := range n.items {
		if it.kid == nil {
			b.WriteString(it.text)
		} else {
			b.WriteString(allText(it.kid))
		}
	}
	return b.String()
}

func lineKeys(turn *node) []string {
	var keys []string
	for _, l := range turn.find("div.turn-line") {
		keys = append(keys, l.first("span.k").text())
	}
	return keys
}

func TestTurnRail(t *testing.T) {
	tab := goldenResult(t, "session-page").Tables.Session
	n := detailNode(t, "session", "/sessions/sess-long-1", views.Params{Limit: 50}, nil)
	turns := mainOf(t, n).find("ol.rail li.turn")
	if len(tab.Turns) == 0 || len(turns) != min(len(tab.Turns), 50) {
		t.Fatalf("%d turns on the rail, the golden has %d", len(turns), len(tab.Turns))
	}
	chips := mainOf(t, n).find("a.mchip")
	if len(chips) == 0 {
		t.Fatal("no memory chip on the rail")
	}
	for _, a := range chips {
		id := strings.TrimPrefix(a.attr("href"), "/memories/")
		if id == a.attr("href") || id == "" {
			t.Errorf("a chip links to %q, not a memory", a.attr("href"))
		}
		full, err := url.PathUnescape(id)
		if err != nil || !strings.HasPrefix(a.attr("title"), full+" · ") {
			t.Errorf("chip title %q does not start with the full id of %q", a.attr("title"), id)
		}
		text := a.text()
		if !strings.HasPrefix(text, views.ShortID(full)) || !strings.HasSuffix(text, views.Word(views.OutcomeWords, "pending")) {
			t.Errorf("chip text %q: want it to start with %q and end with the outcome word", text, views.ShortID(full))
		}
	}
}

// Every outcome is told by its word, and the border by its class.
func TestTurnChipsTellTheOutcomeInWords(t *testing.T) {
	one := ptrI(1)
	var shows []metrics.ShowChip
	cases := []struct{ outcome, word, class string }{
		{"credit", "used by Claude", "used"}, {"confident_miss", "shown, then ignored", "ignored"},
		{"unknown", "not declared", ""}, {"shown", "shown only", ""}, {"pending", "pending", ""},
	}
	for i, c := range cases {
		shows = append(shows, metrics.ShowChip{MemoryID: fmt.Sprintf("mem-%d-aaaaaaaa", i), Path: "prompt", Outcome: c.outcome, Rank: one})
	}
	n := detailNode(t, "session", "/sessions/s", views.Params{Limit: 50}, func(d *PageData) {
		d.V["sessions"].Tables.Session.Turns = []metrics.TurnRow{{N: one, At: detailBase, Shows: shows}}
	})
	chips := mainOf(t, n).find("a.mchip")
	if len(chips) != len(cases) {
		t.Fatalf("%d chips, want %d", len(chips), len(cases))
	}
	for i, c := range cases {
		a := chips[i]
		if !strings.HasSuffix(a.text(), c.word) {
			t.Errorf("%s: chip text %q does not end with %q", c.outcome, a.text(), c.word)
		}
		if c.class != "" && a.attr("class") != "mchip "+c.class || c.class == "" && a.attr("class") != "mchip" {
			t.Errorf("%s: chip class %q, want %q", c.outcome, a.attr("class"), strings.TrimSpace("mchip "+c.class))
		}
		if !strings.Contains(a.text(), "#1") || !strings.Contains(a.attr("title"), "rank 1 · "+views.Word(views.PathWords, "prompt")+" · "+c.word) {
			t.Errorf("%s: chip %q title %q lacks the rank, the place or the outcome", c.outcome, a.text(), a.attr("title"))
		}
	}
}

func TestTurnLinesAreWordsInTheirOrder(t *testing.T) {
	one, two := ptrI(1), ptrI(2)
	chip := func(id, path, outcome string, rank *int64) metrics.ShowChip {
		return metrics.ShowChip{MemoryID: id, Path: path, Outcome: outcome, Rank: rank}
	}
	rows := []metrics.TurnRow{
		{N: one, At: detailBase, Decision: "recall", Searches: 2, Declared: []string{"used", "wrong"}, Check: "declared",
			Shows: []metrics.ShowChip{chip("mem-read-0001", "read", "pending", nil), chip("mem-prompt-01", "prompt", "credit", one),
				chip("mem-brief-001", "briefing", "shown", nil), chip("mem-gate-0001", "gate", "confident_miss", two), chip("mem-prompt-02", "prompt", "credit", two)}},
		{N: two, At: detailBase.Add(time.Minute), Decision: "skip", Reason: "same_topic", Check: ""},
		{N: ptrI(3), At: detailBase.Add(2 * time.Minute), Decision: "recall", Empty: "below_floor", Check: "undeclared"},
		{N: nil, At: detailBase.Add(3 * time.Minute), Check: "pending", Shows: []metrics.ShowChip{chip("mem-late-0001", "search", "pending", nil)}},
	}
	n := detailNode(t, "session", "/sessions/s", views.Params{Limit: 50}, func(d *PageData) {
		d.V["sessions"].Tables.Session.Turns = rows
	})
	turns := mainOf(t, n).find("li.turn")
	if len(turns) != len(rows) {
		t.Fatalf("%d turns, want %d", len(turns), len(rows))
	}
	// The first turn: the lines in the order things happen, the chips grouped by place, in the view's order.
	wantKeys := []string{"At session start", "Before the prompt", "Claude's search", "Memories shown", "Opened by Claude", "Claude said", "End of turn"}
	if got := lineKeys(turns[0]); strings.Join(got, "|") != strings.Join(wantKeys, "|") {
		t.Errorf("turn 1 lines %q, want %q", got, wantKeys)
	}
	shown := []string{}
	for _, a := range turns[0].find("div.turn-line a.mchip") {
		shown = append(shown, views.ShortID(strings.TrimPrefix(a.attr("href"), "/memories/")))
	}
	if got, want := strings.Join(shown, " "), "mem-brie mem-prom mem-gate mem-prom mem-read"; got != want {
		t.Errorf("chips in order %q, want %q", got, want)
	}
	for key, want := range map[string]string{
		"Before the prompt": "looked for memory: 3 added", "Claude's search": "2 memory tool calls",
		"Claude said": "used, reported wrong", "End of turn": "said what it used",
	} {
		if got, _ := lineOf(turns[0], key); got != want {
			t.Errorf("turn 1 %q: %q, want %q", key, got, want)
		}
	}
	if !strings.Contains(turns[0].first("span.st-ok").text(), "said what it used") {
		t.Error("an end of turn that went well is not marked")
	}
	for i, want := range []struct{ key, text string }{
		{"Before the prompt", "no memory looked up: same topic as before"},
		{"Before the prompt", "looked for memory: nothing added (below the cut-off)"},
	} {
		if got, _ := lineOf(turns[i+1], want.key); got != want.text {
			t.Errorf("turn %d %q: %q, want %q", i+2, want.key, got, want.text)
		}
	}
	if got, _ := lineOf(turns[1], "End of turn"); got != "no memory shown, nothing to say" {
		t.Errorf("a turn with nothing to check: %q", got)
	}
	if got, _ := lineOf(turns[2], "End of turn"); got != "didn't say" || turns[2].first("span.st-watch") == nil {
		t.Errorf("an undeclared turn: %q, marked %v", got, turns[2].first("span.st-watch") != nil)
	}
	if got, _ := lineOf(turns[3], "End of turn"); got != "waiting for the end-of-turn check" {
		t.Errorf("a pending turn: %q", got)
	}
	if got := turns[3].first("div.turn-n b").text(); got != "Turn —" {
		t.Errorf("a turn with no number is named %q", got)
	}
	if _, ok := lineOf(turns[1], "Claude's search"); ok {
		t.Error("a turn with no search has a search line")
	}
	if _, ok := lineOf(turns[1], "Memories shown"); ok {
		t.Error("a turn that showed nothing has a chips line")
	}
}

// The rail shows the words of the page. A stored value the page has no word
// for is never shown as stored, so free text in the journal cannot reach it.
func TestTurnRailShowsNoStoredValue(t *testing.T) {
	const bad = "FREE-TEXT-CANARY"
	n := render(t, "session", func(d *PageData) {
		d.L, d.P = en, views.Params{Limit: 50}
		s := d.V["sessions"].Tables.Session
		s.Totals.Sent = bad
		s.Turns = []metrics.TurnRow{
			{N: ptrI(1), At: detailBase, Decision: bad + "-d", Reason: bad + "-r", Empty: bad + "-e", Declared: []string{bad + "-v"}, Check: bad + "-c",
				Shows: []metrics.ShowChip{{MemoryID: "mem-1-0000-0000", Path: bad + "-p", Outcome: bad + "-o"}}},
			{N: ptrI(2), At: detailBase, Decision: "skip", Reason: bad + "-r2"},
			{N: ptrI(3), At: detailBase, Decision: "recall", Empty: bad + "-e2"},
		}
	})
	if strings.Contains(n, bad) {
		i := strings.Index(n, bad)
		t.Errorf("the page shows a stored value: %q", n[max(0, i-60):min(len(n), i+60)])
	}
	node := parseHTML(t, n)
	if got, _ := lineOf(mainOf(t, node).find("li.turn")[1], "Before the prompt"); got != "no memory looked up: reason not recorded" {
		t.Errorf("an unknown skip reason: %q", got)
	}
}

func TestTurnRailIsPaged(t *testing.T) {
	const path = "/sessions/sess-long-1"
	setTurns := func(d *PageData) { d.V["sessions"].Tables.Session.Turns = turnsOf(120) }
	count := func(n *node) int { return len(mainOf(t, n).find("li.turn")) }
	first := detailNode(t, "session", path+"?days=30&limit=50&tz=Europe%2FParis", views.Params{Limit: 50, Days: 30, DaysSet: true}, setTurns)
	if got := count(first); got != 50 {
		t.Fatalf("first page: %d turns, want 50", got)
	}
	names := mainOf(t, first).find("li.turn div.turn-n b")
	if names[0].text() != "Turn 1" || names[49].text() != "Turn 50" {
		t.Errorf("first page runs %q to %q", names[0].text(), names[49].text())
	}
	var next, prev []string
	for _, a := range mainOf(t, first).find("div.pager a") {
		switch a.attr("rel") {
		case "next":
			next = append(next, a.attr("href"))
		case "prev":
			prev = append(prev, a.attr("href"))
		}
	}
	if len(next) != 1 || len(prev) != 0 {
		t.Fatalf("first page: next %q, prev %q", next, prev)
	}
	u, err := url.Parse(next[0])
	if err != nil || u.Path != path || u.Query().Get("offset") != "50" || u.Query().Get("days") != "30" || u.Query().Get("limit") != "50" || u.Query().Get("tz") != "Europe/Paris" {
		t.Errorf("next link %q does not go to offset 50 and keep the page's parameters", next[0])
	}
	if got := strings.TrimSpace(mainOf(t, first).first("div.pager span").text()); got != "1–50 of 120" {
		t.Errorf("pager says %q", got)
	}

	last := detailNode(t, "session", path+"?offset=100", views.Params{Limit: 50, Offset: 100}, setTurns)
	if got := count(last); got != 20 {
		t.Fatalf("?offset=100: %d turns, want 20", got)
	}
	names = mainOf(t, last).find("li.turn div.turn-n b")
	if names[0].text() != "Turn 101" || names[19].text() != "Turn 120" {
		t.Errorf("last page runs %q to %q", names[0].text(), names[19].text())
	}
	var links []string
	for _, a := range mainOf(t, last).find("div.pager a") {
		links = append(links, a.attr("rel")+" "+a.attr("href"))
	}
	if len(links) != 1 || !strings.HasPrefix(links[0], "prev ") || !strings.Contains(links[0], "offset=50") {
		t.Errorf("last page links %q, want one previous link to offset 50", links)
	}

	all := detailNode(t, "session", path+"?limit=500", views.Params{Limit: 500}, setTurns)
	if got := count(all); got != 120 || len(mainOf(t, all).find("div.pager a")) != 0 {
		t.Errorf("limit=500: %d turns, want 120 and no pager link", got)
	}
}

func TestTurnsPage(t *testing.T) {
	turns := turnsOf(120)
	for _, c := range []struct {
		name         string
		p            views.Params
		first, count int
	}{
		{"default size", views.Params{}, 1, 50},
		{"a size", views.Params{Limit: 10, Offset: 5}, 6, 10},
		{"too large a size", views.Params{Limit: 9999}, 1, 120},
		{"a negative offset", views.Params{Limit: 50, Offset: -3}, 1, 50},
		{"the last page", views.Params{Limit: 50, Offset: 100}, 101, 20},
		{"past the end", views.Params{Limit: 50, Offset: 500}, 0, 0},
	} {
		got := turnsPage(turns, c.p)
		if len(got) != c.count || c.count > 0 && *got[0].N != int64(c.first) {
			t.Errorf("%s: %d turns from %v, want %d from %d", c.name, len(got), got, c.count, c.first)
		}
	}
	if len(turnsPage(nil, views.Params{})) != 0 {
		t.Error("no turns, some page")
	}
	// The cached result is shared: the page cuts a copy of the window, never the turns themselves.
	before := *turns[0].N
	_ = turnsPage(turns, views.Params{Limit: 1})
	if len(turns) != 120 || *turns[0].N != before {
		t.Error("turnsPage changed its input")
	}
}

func TestSessionPageHeader(t *testing.T) {
	n := detailNode(t, "session", "/sessions/sess-long-1", views.Params{Limit: 50}, func(d *PageData) {
		s := d.V["sessions"].Tables.Session
		s.Totals.Sent = "failed"
		s.Totals.ContextChars = 3100
		s.Totals.Prompts, s.Totals.PromptsWithMemory, s.Totals.Shown, s.Totals.Used, s.Totals.Wrong = 5, 2, 10, 2, 1
		s.Turns = turnsOf(3)
	})
	m := mainOf(t, n)
	figs := map[string]string{}
	for _, f := range m.find("div.figs div.fig") {
		figs[f.first("span.lab").text()] = f.first("span.v").text() + " | " + textOrNone(f.first("span.s"))
	}
	for label, want := range map[string]string{
		"Prompts": "5 | 2 got memory", "Memories shown": "10 | 3 distinct", "Used by Claude": "2 | ", "Reported wrong": "1 | ",
		"Context added": "3,100 | characters · ≈ 775 tokens",
	} {
		if figs[label] != want {
			t.Errorf("figure %q is %q, want %q", label, figs[label], want)
		}
	}
	if pill := m.first("span.pill"); pill == nil || pill.text() != "failed" || !strings.Contains(pill.attr("class"), "st-bad") {
		t.Errorf("the sent pill is %v", pill)
	}
	if got := m.first("div.session-meta").text(); !strings.Contains(got, "3 turns") {
		t.Errorf("the meta line %q does not count the turns", got)
	}
	if code := m.first("details.cli code"); code == nil || allText(code) != "backsight stats sessions sess-long-1" {
		t.Errorf("the turn card names its command: %v", code)
	}
}

// Every link built from a value of the data escapes it.
func TestSessionPageLinksAreEscaped(t *testing.T) {
	const hostile = "a/b?c#d e"
	n := detailNode(t, "session", "/sessions/x", views.Params{Limit: 50}, func(d *PageData) {
		s := d.V["sessions"].Tables.Session
		s.Totals.ID, s.Totals.Project = hostile, hostile
		s.Turns = []metrics.TurnRow{{N: ptrI(1), At: detailBase, Shows: []metrics.ShowChip{
			{MemoryID: hostile, Path: "prompt", Outcome: "credit"}, {MemoryID: "..", Path: "prompt", Outcome: "shown"}, {MemoryID: ".", Path: "gate", Outcome: "shown"}, {MemoryID: "", Path: "gate", Outcome: "shown"}}}}
	})
	var hrefs []string
	for _, a := range mainOf(t, n).find("a") {
		hrefs = append(hrefs, a.attr("href"))
	}
	esc := url.PathEscape(hostile)
	var sawMemory, sawProject bool
	for _, h := range hrefs {
		sawMemory = sawMemory || h == "/memories/"+esc
		sawProject = sawProject || strings.HasPrefix(h, "/projects/"+esc)
		if strings.Contains(h, "a/b") || strings.Contains(h, "#") || strings.Contains(h, " ") || h == "/memories/." || h == "/memories/.." || h == "/memories/" {
			t.Errorf("link %q is not escaped or is a path of its own", h)
		}
	}
	if !sawMemory || !sawProject {
		t.Errorf("links %q lack the memory (%v) or the project (%v)", hrefs, sawMemory, sawProject)
	}
	if got := len(mainOf(t, n).find("span.mchip")); got != 3 {
		t.Errorf("%d chips without a link, want the 3 whose id cannot be a path", got)
	}
	// The session's own id is a name, not a link.
	crumb := mainOf(t, n).first("nav[aria-label=Breadcrumb]")
	if crumb == nil || len(crumb.find("a")) != 1 {
		t.Errorf("the breadcrumb links more than the list of sessions: %v", crumb)
	}
}

func TestSessionPageWithoutTurnsSaysSo(t *testing.T) {
	n := detailNode(t, "session", "/sessions/s", views.Params{Limit: 50}, func(d *PageData) {
		d.V["sessions"].Tables.Session.Turns = nil
	})
	m := mainOf(t, n)
	if len(m.find("li.turn")) != 0 || !strings.Contains(m.text(), "No turn of this session was recorded.") {
		t.Errorf("a session without turns: %q", m.text())
	}
}

// ----- the memory page -----

// shows is n shows of a memory, the latest first as the view gives them.
func showsOf(n int) []metrics.ShowMark {
	out := make([]metrics.ShowMark, n)
	for i := range out {
		out[i] = metrics.ShowMark{At: detailBase.Add(-time.Duration(i) * time.Hour), Path: "prompt", Outcome: "credit", Session: fmt.Sprintf("sess-%04d-aaaa", i), Rank: ptrI(1)}
	}
	return out
}

func TestMemoryPage(t *testing.T) {
	tab := goldenResult(t, "memory-page").Tables.Memory
	n := detailNode(t, "memory", "/memories/"+url.PathEscape(tab.Row.ID), views.Params{Limit: 50}, nil)
	m := mainOf(t, n)
	var text *node
	for _, a := range m.find("a.btn") {
		if a.text() == "Show text" {
			text = a
		}
	}
	if text == nil || text.attr("href") != "/memories/"+url.PathEscape(tab.Row.ID)+"/text" {
		t.Errorf("Show text: %v", text)
	}
	marks := m.find("path.mkp")
	if len(marks) != len(tab.Shows) || len(marks) == 0 || len(marks) > 100 {
		t.Fatalf("%d marks, the golden has %d shows", len(marks), len(tab.Shows))
	}
	for _, p := range marks {
		if p.attr("tabindex") != "0" {
			t.Errorf("a mark is not a focus stop: %v", p.attr("tabindex"))
		}
	}
	// The bars follow the histogram, in order.
	wantBars := []string{}
	for _, k := range []string{"1", "2", "3", "4", "5", "6+"} {
		wantBars = append(wantBars, fmt.Sprintf("Rank %s: %d times shown", k, tab.RankHistogram[k]))
	}
	if got := barTitles(t, n); strings.Join(got, "|") != strings.Join(wantBars, "|") {
		t.Errorf("rank bars %q, want %q", got, wantBars)
	}
	// Each command is in <code>, with what it does.
	var cmds, labels []string
	for _, dd := range m.find("dl.dl-center dd") {
		cmds = append(cmds, allText(dd.first("code")))
	}
	for _, dt := range m.find("dl.dl-center dt") {
		labels = append(labels, dt.text())
	}
	if strings.Join(cmds, "|") != strings.Join(tab.Commands, "|") {
		t.Errorf("commands %q, want %q", cmds, tab.Commands)
	}
	if got := strings.Join(labels, "|"); got != "Read it in the terminal|Hide it from recall|Bring it back" {
		t.Errorf("command labels %q", got)
	}
	// The copy button for the id.
	var copyID *node
	for _, b := range m.find("button.copy") {
		if b.has("data-copy") && b.attr("data-copy") == tab.Row.ID {
			copyID = b
		}
	}
	if copyID == nil || !copyID.has("hidden") {
		t.Errorf("no hidden copy button for the full id %q", tab.Row.ID)
	}
	for _, b := range m.find("button.copy") {
		if !b.has("hidden") {
			t.Error("a copy button shows without its script")
		}
	}
	// The page names no memory text.
	if len(m.find("pre")) != 0 {
		t.Error("the memory page holds a pre: the text is the text page's")
	}
}

// barTitles are the titles of the rank bars, in the order they are drawn.
func barTitles(t *testing.T, n *node) []string {
	t.Helper()
	var out []string
	for _, rect := range mainOf(t, n).find("rect.f-used") {
		out = append(out, rect.first("title").text())
	}
	return out
}

func TestMemoryHeaderAndFigures(t *testing.T) {
	created := detailBase.Add(-30 * 24 * time.Hour)
	n := detailNode(t, "memory", "/memories/m", views.Params{Limit: 50}, func(d *PageData) {
		d.V["memories"].GeneratedAt = detailBase
		d.Frame.Prefs.Dev = true
		m := d.V["memories"].Tables.Memory
		m.Kind, m.ServerState, m.Created = "world", "invalidated", &created
		m.Row.Project, m.Row.Shown, m.Row.Used, m.Row.Ignored, m.Row.Boost = "backsight", 23, 9.04, 5.96, -0.3
		m.Row.Rate = fp(0.6)
		m.Row.State = "wrong"
	})
	m := mainOf(t, n)
	meta := m.first("div.memory-meta")
	for _, want := range []string{"backsight", "fact about you and your work", "created", "forgotten"} {
		if !strings.Contains(meta.text(), want) {
			t.Errorf("the header %q lacks %q", meta.text(), want)
		}
	}
	if meta.first("time") == nil || meta.first("span.pill") == nil || !strings.Contains(meta.first("span.pill").attr("class"), "st-watch") {
		t.Errorf("the header has no created time or no server pill: %q", meta.text())
	}
	figs := map[string]string{}
	for _, f := range m.find("div.figs div.fig") {
		figs[f.first("span.lab").text()] = f.first("span.v").text()
	}
	for label, want := range map[string]string{"Shown": "23", "Used by Claude": "9.0", "Shown, then ignored": "6.0", "Used when judged": "60 %", "Learned ranking now": "lowered"} {
		if figs[label] != want {
			t.Errorf("figure %q is %q, want %q", label, figs[label], want)
		}
	}
	if w := m.first("div.fig span.v-word"); w == nil || w.text() != "lowered" {
		t.Errorf("the ranking word is not set as a word: %v", w)
	}
	if !strings.Contains(m.first("div.state").text(), "reported wrong") || m.first("div.state span.i-watch") == nil {
		t.Error("a memory reported wrong says nothing of it beside its text")
	}
	// Without the server's answer the page says so, instead of dashes.
	n = detailNode(t, "memory", "/memories/m", views.Params{Limit: 50}, nil)
	if !strings.Contains(mainOf(t, n).text(), "Kind, creation and state on the server are not available: the server did not answer.") {
		t.Error("a memory the server did not answer for does not say so")
	}
}

func TestMemoryPageLinksAreEscaped(t *testing.T) {
	const hostile = "a/b?c#d e"
	n := detailNode(t, "memory", "/memories/x", views.Params{Limit: 50}, func(d *PageData) {
		m := d.V["memories"].Tables.Memory
		m.Row.ID, m.Row.Project = hostile, hostile
		m.Shows[0].Session = hostile
	})
	esc := url.PathEscape(hostile)
	var hrefs []string
	for _, a := range mainOf(t, n).find("a") {
		hrefs = append(hrefs, a.attr("href"))
		if strings.Contains(a.attr("href"), "a/b") || strings.Contains(a.attr("href"), "#") || strings.Contains(a.attr("href"), " ") {
			t.Errorf("link %q is not escaped", a.attr("href"))
		}
	}
	var text, project bool
	for _, h := range hrefs {
		text = text || h == "/memories/"+esc+"/text"
		project = project || strings.HasPrefix(h, "/projects/"+esc)
	}
	if !text || !project {
		t.Errorf("links %q lack the text (%v) or the project (%v)", hrefs, text, project)
	}
	// An id that is a path of its own gets no link to its text.
	for _, id := range []string{"..", ".", ""} {
		n = detailNode(t, "memory", "/memories/x", views.Params{Limit: 50}, func(d *PageData) { d.V["memories"].Tables.Memory.Row.ID = id })
		for _, a := range mainOf(t, n).find("a.btn") {
			t.Errorf("id %q: a button links to %q", id, a.attr("href"))
		}
	}
}

func TestMemoryShowsArePagedAndAllCanBeAsked(t *testing.T) {
	const path = "/memories/mem-long-1?days=30&tz=Europe%2FParis&limit=50"
	page := func(limit, offset, total, shows int, path string) *node {
		return detailNode(t, "memory", path, views.Params{Limit: limit, Offset: offset}, func(d *PageData) {
			d.V["memories"].Tables.Memory.Shows = showsOf(shows)
			d.V["memories"].Tables.Page = &metrics.Page{Limit: limit, Offset: offset, Total: total}
		})
	}
	n := page(50, 50, 120, 50, path+"&offset=50")
	var all, next *node
	for _, a := range mainOf(t, n).find("a") {
		switch {
		case a.text() == "Show all shows":
			all = a
		case a.attr("rel") == "next":
			next = a
		}
	}
	if all == nil || next == nil {
		t.Fatalf("a page of 50 of 120 lacks Show all shows (%v) or the next link (%v)", all, next)
	}
	u, _ := url.Parse(all.attr("href"))
	if u.Query().Get("limit") != "500" || u.Query().Has("offset") || u.Query().Get("days") != "30" || u.Query().Get("tz") != "Europe/Paris" || u.Path != "/memories/mem-long-1" {
		t.Errorf("Show all shows goes to %q", all.attr("href"))
	}
	u, _ = url.Parse(next.attr("href"))
	if u.Query().Get("offset") != "100" || u.Query().Get("days") != "30" || u.Query().Get("limit") != "50" {
		t.Errorf("next goes to %q", next.attr("href"))
	}
	if got := len(mainOf(t, n).find("path.mkp")); got != 50 {
		t.Errorf("%d marks, want the page's 50", got)
	}
	// All shown: nothing more to ask. At the largest page of a longer history: nothing larger to ask.
	for _, c := range []struct {
		name                string
		limit, total, shows int
	}{{"all shown", 50, 50, 50}, {"the largest page", 500, 800, 500}} {
		for _, a := range mainOf(t, page(c.limit, 0, c.total, c.shows, "/memories/m?limit="+fmt.Sprint(c.limit))).find("a") {
			if a.text() == "Show all shows" {
				t.Errorf("%s: Show all shows is offered", c.name)
			}
		}
	}
}

// The strip is drawn from the shows of the page; the memory's creation bounds
// it only on the page that holds the oldest show.
func TestMemoryStripIsBoundedByCreationOnTheLastPageOnly(t *testing.T) {
	created := detailBase.Add(-90 * 24 * time.Hour)
	line := func(offset, total int) bool {
		n := detailNode(t, "memory", "/memories/m", views.Params{Limit: 50, Offset: offset}, func(d *PageData) {
			d.V["memories"].GeneratedAt = detailBase
			m := d.V["memories"].Tables.Memory
			m.Created, m.Shows, m.Retirements = &created, showsOf(50), nil
			d.V["memories"].Tables.Page = &metrics.Page{Limit: 50, Offset: offset, Total: total}
		})
		return mainOf(t, n).first("line.mk") != nil
	}
	if line(0, 120) || line(50, 120) {
		t.Error("the creation bounds a page that does not reach the oldest show")
	}
	if !line(70, 120) || !line(0, 50) {
		t.Error("the creation does not bound the page that holds the oldest show")
	}
}

// ----- stripOf and rankOf -----

func TestStripOfMemoryPage(t *testing.T) {
	loc := parisZone()
	at := time.Date(2026, 10, 15, 10, 0, 0, 0, loc)
	p := &metrics.MemoryPage{Row: metrics.MemoryRow{ID: "7f3a9c21-e04b-4d0e-9a1f-6c55b2d18e07"},
		Shows: []metrics.ShowMark{
			{At: at.Add(24 * time.Hour), Path: "gate", Outcome: "confident_miss", Session: "a41c09e24e1b0c9d2f7a63e85b1d0c4a", Rank: ptrI(3), RankWithoutBoost: ptrI(2)},
			{At: at, Path: "prompt", Outcome: "credit", Session: "a41c09e24e1b0c9d2f7a63e85b1d0c4a", Rank: ptrI(1), RankWithoutBoost: ptrI(2)},
		},
		Retirements: []metrics.RetireMark{{At: at.Add(48 * time.Hour), Kind: "hide"}}}
	out := string(stripOf(en, "backsight stats memories x --json", p, at.Add(10*24*time.Hour), loc))
	n := parseHTML(t, out)
	var rows []string
	for _, tx := range n.find("text.lbl") {
		if tx.attr("x") == "160.0" { // the row names; the retirement is named on its line
			rows = append(rows, tx.text())
		}
	}
	if got := strings.Join(rows, "|"); got != "added to the prompt|before a question" {
		t.Errorf("rows %q, want the paths in their order whatever the order of the shows", got)
	}
	var titles []string
	for _, m := range n.find("path.mkp") {
		titles = append(titles, m.first("title").text())
	}
	want := []string{
		"Oct 16 · before a question · shown, then ignored · rank 3 (without the learned ranking: 2) · session a41c09e2",
		"Oct 15 · added to the prompt · used by Claude · rank 1 (without the learned ranking: 2) · session a41c09e2",
	}
	// The marks are drawn row by row: the prompt row first.
	if len(titles) != 2 || titles[0] != want[1] || titles[1] != want[0] {
		t.Errorf("mark titles %q, want %q", titles, []string{want[1], want[0]})
	}
	if lines := n.find("line.thr-w"); len(lines) != 1 || lines[0].first("title").text() != "Oct 17 · hidden" {
		t.Errorf("retirement lines: %v", lines)
	}
	if n.first("path.mkp.f-used") == nil || n.first("path.mkp.f-ignored") == nil {
		t.Error("the shapes do not follow the outcomes")
	}
	if n.first("line.mk") != nil {
		t.Error("a memory with no creation time has a created line")
	}
	// The strip says what it draws to a reader who cannot see it.
	if label := n.first("svg[role=img]").attr("aria-label"); label != "Every time memory 7f3a9c21 was shown, by where" {
		t.Errorf("label %q", label)
	}
}

func TestStripOfShapesRankAndCreation(t *testing.T) {
	loc := parisZone()
	created := time.Date(2026, 9, 14, 10, 31, 0, 0, loc)
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, loc)
	var shows []metrics.ShowMark
	for _, o := range []string{"credit", "confident_miss", "unknown", "shown", "pending", "undeclared", "SOMETHING-ELSE", ""} {
		shows = append(shows, metrics.ShowMark{At: at, Path: "search", Outcome: o})
	}
	p := &metrics.MemoryPage{Row: metrics.MemoryRow{ID: "m1"}, Shows: shows, Created: &created}
	n := parseHTML(t, string(stripOf(en, "", p, at.Add(time.Hour), loc)))
	var got []string
	for _, m := range n.find("path.mkp") {
		got = append(got, strings.Fields(strings.TrimPrefix(m.attr("class"), "mkp "))[0])
	}
	want := []string{"f-used", "f-ignored", "f-undeclared", "f-shown", "mk-pend", "f-undeclared", "f-shown", "f-shown"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("shapes %q, want %q", got, want)
	}
	for _, m := range n.find("path.mkp") {
		if title := m.first("title").text(); strings.Contains(title, "rank") || strings.Contains(title, "session") || strings.Contains(title, "SOMETHING") {
			t.Errorf("a show without rank, session or known outcome says %q", title)
		}
	}
	created1 := false
	for _, tx := range n.find("text") {
		created1 = created1 || tx.text() == "created"
	}
	if n.first("line.mk") == nil || !created1 {
		t.Error("the creation of the memory is not drawn")
	}
	// A rank without its twin says only the rank; rank 0 is no rank.
	p.Shows = []metrics.ShowMark{{At: at, Path: "prompt", Outcome: "shown", Rank: ptrI(4)}, {At: at, Path: "prompt", Outcome: "shown", Rank: ptrI(0), RankWithoutBoost: ptrI(0)}}
	titles := []string{}
	for _, m := range parseHTML(t, string(stripOf(en, "", p, at, loc))).find("path.mkp") {
		titles = append(titles, m.first("title").text())
	}
	if len(titles) != 2 || titles[0] != "Oct 1 · added to the prompt · shown only · rank 4" || titles[1] != "Oct 1 · added to the prompt · shown only" {
		t.Errorf("titles %q", titles)
	}
}

func TestStripOfKeepsStoredNamesOut(t *testing.T) {
	loc := parisZone()
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, loc)
	p := &metrics.MemoryPage{Row: metrics.MemoryRow{ID: "m1"}, Shows: []metrics.ShowMark{
		{At: at, Path: "FREE-TEXT-CANARY", Outcome: "shown"}, {At: at, Path: "", Outcome: "shown"}, {At: at, Path: "read", Outcome: "shown"}},
		Retirements: []metrics.RetireMark{{At: at, Kind: "FREE-TEXT-CANARY-K"}}}
	out := string(stripOf(en, "", p, at, loc))
	if strings.Contains(out, "CANARY") {
		t.Errorf("the strip shows a stored name: %s", out)
	}
	var rows []string
	for _, tx := range parseHTML(t, out).find("text.lbl") {
		if tx.attr("x") == "160.0" {
			rows = append(rows, tx.text())
		}
	}
	if got := strings.Join(rows, "|"); got != "opened by Claude|elsewhere" {
		t.Errorf("rows %q: a path with no word shares the last row, named by a word", got)
	}
}

func TestStripOfEmptyAndCapped(t *testing.T) {
	loc := parisZone()
	for name, p := range map[string]*metrics.MemoryPage{"nil": nil, "no shows": {Row: metrics.MemoryRow{ID: "m"}},
		"only a retirement": {Row: metrics.MemoryRow{ID: "m"}, Retirements: []metrics.RetireMark{{At: detailBase, Kind: "hide"}}}} {
		out := string(stripOf(en, "", p, detailBase, loc))
		if !strings.Contains(out, "This memory has not been shown yet.") || strings.Contains(out, "<svg") {
			t.Errorf("%s: %q", name, out)
		}
	}
	// 700 shows over 700 hours: the marks stay within the cap, and past 100 none is a focus stop.
	p := &metrics.MemoryPage{Row: metrics.MemoryRow{ID: "m"}, Shows: showsOf(700)}
	n := parseHTML(t, string(stripOf(en, "", p, detailBase, loc)))
	marks := n.find("path.mkp")
	if len(marks) == 0 || len(marks) > 500 {
		t.Errorf("%d marks, want 1 to 500", len(marks))
	}
	for _, m := range marks {
		if m.has("tabindex") {
			t.Error("a mark is a focus stop among more than 100")
			break
		}
	}
	// 100 shows or fewer: each mark is one show and a focus stop.
	n = parseHTML(t, string(stripOf(en, "", &metrics.MemoryPage{Row: metrics.MemoryRow{ID: "m"}, Shows: showsOf(100)}, detailBase, loc)))
	if got := len(n.find("path.mkp[tabindex=0]")); got != 100 {
		t.Errorf("%d focus stops among 100 shows", got)
	}
}

func TestRankOf(t *testing.T) {
	title := func(h map[string]int64) []string {
		var out []string
		for _, r := range parseHTML(t, string(rankOf(en, "", h))).find("rect.f-used") {
			out = append(out, r.first("title").text())
		}
		return out
	}
	got := title(map[string]int64{"1": 3, "2": 0, "6+": 4, "10": 1, "7": 9, "3": 2, "stored-name": 5})
	want := []string{"Rank 1: 3 times shown", "Rank 2: 0 times shown", "Rank 3: 2 times shown", "Rank 7: 9 times shown", "Rank 10: 1 times shown", "Rank 6+: 4 times shown"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("bars %q, want %q: numeric order, 6+ last, no stored name", got, want)
	}
	got = title(map[string]int64{"1": 1, "unranked": 2, "6+": 1})
	if len(got) != 3 || got[0] != "Not ranked: 2 times shown" || got[2] != "Rank 6+: 1 times shown" {
		t.Errorf("with shows that had no rank: %q", got)
	}
	got = title(map[string]int64{"1": 1, "unranked": 0})
	if len(got) != 1 {
		t.Errorf("an empty not-ranked bar is drawn: %q", got)
	}
	for _, h := range []map[string]int64{nil, {}, {"1": 0, "2": 0}} {
		if out := string(rankOf(en, "", h)); !strings.Contains(out, "This memory has not been shown yet.") {
			t.Errorf("%v: %q, want the empty sentence", h, out)
		}
	}
}

// ----- the text page -----

func TestTextPageInTheFrame(t *testing.T) {
	h, mem, path := textHarness(t, "line one\n<b>line</b> two "+textCanary)
	_, b := rawGet(t, h, h.login(), path)
	n := parseHTML(t, b)
	pre := n.find("main pre.memtext")
	if len(pre) != 1 || len(n.find("pre")) != 1 {
		t.Fatalf("%d pre.memtext, %d pre", len(pre), len(n.find("pre")))
	}
	if got := pre[0].text(); got != "line one <b>line</b> two "+textCanary {
		t.Errorf("the text reads %q", got)
	}
	short := views.ShortID(mem)
	if hs := n.find("h1"); len(hs) != 1 || hs[0].text() != "Memory "+short {
		t.Errorf("h1 %v, want Memory %s", hs, short)
	}
	// The frame is there: the top bar, the footer; the window links are not.
	if n.first("header.topbar nav.nav") == nil || n.first("footer.foot") == nil {
		t.Error("the text page lacks the frame")
	}
	for _, a := range n.find("a") {
		href := a.attr("href")
		if strings.Contains(href, "days=") || strings.Contains(href, "since=") || strings.Contains(href, "limit=") {
			t.Errorf("the text page links %q: a window or a parameter", href)
		}
	}
	if len(n.find("form")) != 0 || len(n.find("input")) != 0 {
		t.Error("the text page has a form")
	}
	var back string
	for _, a := range n.find("main a.btn") {
		back = a.attr("href")
	}
	if back != "/memories/"+url.PathEscape(mem) {
		t.Errorf("link back %q", back)
	}
	// Only the text page holds the text.
	for _, p := range []string{"/memories/" + url.PathEscape(mem)} {
		if _, other := rawGet(t, h, h.login(), p); strings.Contains(other, textCanary) {
			t.Errorf("%s holds the text", p)
		}
	}
}

func TestTextPageUnreadableSaysWhy(t *testing.T) {
	n := parseHTML(t, render(t, "memory_text", func(d *PageData) {
		d.V["memories"].Text = &metrics.MemoryText{ID: "mem-long-1", Reason: metrics.ReasonServerDown}
	}))
	m := mainOf(t, n)
	if len(m.find("pre")) != 0 {
		t.Error("a pre without a text")
	}
	st := m.first("div.state")
	if st == nil || st.text() != "The text could not be read: memory server not answering." || st.first("span.i-watch") == nil {
		t.Errorf("reason: %v", st)
	}
}

func TestTextPageLinkBackIsEscaped(t *testing.T) {
	for _, id := range []string{"m?1/x#y", "a b", ".."} {
		n := parseHTML(t, render(t, "memory_text", func(d *PageData) {
			d.V["memories"].Text = &metrics.MemoryText{ID: id, Text: "x"}
		}))
		for _, a := range mainOf(t, n).find("a") {
			h := a.attr("href")
			if h != "/memories" && (id == ".." || h != "/memories/"+url.PathEscape(id)) {
				t.Errorf("id %q: link %q", id, h)
			}
			if strings.ContainsAny(h, "#? ") {
				t.Errorf("id %q: link %q is not escaped", id, h)
			}
		}
	}
}
