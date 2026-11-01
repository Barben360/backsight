package dashboard

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

var tilePage = views.Params{Days: 7, DaysSet: true}

// tileNamed is the tile called name of scenario sc.
func tileNamed(t *testing.T, sc, name string) Tile {
	t.Helper()
	r := scenarioNamed(t, sc)
	for _, g := range tilesOf(r, tilePage, en, r.Window.Loc, false) {
		for _, tl := range g.Tiles {
			if tl.Name == name {
				return tl
			}
		}
	}
	t.Fatalf("no tile %s", name)
	return Tile{}
}

func deltaText(d *format.Delta) string {
	if d == nil {
		return ""
	}
	return d.Arrow + " " + d.Text
}

func TestTilesFollowTheCopyDeck(t *testing.T) {
	type want struct {
		sc, tile                 string
		big, unit, ci, sub, note string
		word, delta, link, text  string
	}
	const none = "<none>" // the field is not checked
	for _, c := range []want{
		{"healthy", "Found", "28", "%", "(21–35 %)", "46 of 167 prompts", "", "ok", "▲ +2 pts, better", "/sessions?days=7&has=memory", "167 prompts →"},
		{"healthy", "Used", "62", "%", "(50–73 %)", "Said what it used on 100 % of turns.", "Judged on the last 28 days: too few turns in 7.", "ok", none, none, "61 turns →"},
		{"healthy", "Reported wrong", "1 of 14", "", "", "1 reported wrong of 14 verdicts", "1 of 14: too few to judge.", "too few", "", none, "14 verdicts →"},
		{"healthy", "Context", "3,600", "chars", "≈ 900 tokens", "high · typical 2,100 chars", "", "ok", "▼ −200 chars, better", none, "34 sessions →"},
		{"healthy", "Speed", "1.6", "s", "", "slowest 1 in 20 · typical 840 ms", "", "ok", "▲ +0.1 s, worse", none, "167 prompts →"},
		{"healthy", "Flowing", "100", "%", "", "239 of 239 sessions sent", "", "ok", "= no change", none, "239 sessions →"},
		{"qualitywatch", "Found", "14", "%", "(9–20 %)", "usual 27 % · 21 of 150 prompts", scFoundText, "watch", none, none, none},
		{"qualitywatch", "Used", none, none, none, none, "Borderline: the interval still reaches the usual level.", "borderline", none, none, none},
		{"latency", "Speed", "2.3", "s", "", "slowest 1 in 20 · typical 1.1 s · limit 2.5 s", scLatencyText, "watch", none, none, none},
		{"new", "Found", "3 of 9", "", "", "prompts got memory", "3 of 9: too few to judge.", "too few", "", none, none},
		{"new", "Used", "—", "", "", scLearnText, "", "learning", "", none, "1 turn →"},
		{"new", "Reported wrong", "0 of 0", "", "", "no memory judged yet", "Nothing judged yet.", "too few", "", none, "0 verdicts →"},
		{"new", "Context", none, none, "≈ 280 tokens", none, none, "ok", none, none, none},
		{"cantjudge", "Used", "56", "%", "(33–77 %)", "Said what it used on 64 % of turns (16 of 25).", "Unreliable: Claude often didn't say what it used.", "unreliable", "", none, "25 turns →"},
		{"serverdown", "Found", none, none, none, none, "Prompts skipped: server unreachable.", "watch", none, none, none},
		{"serverdown", "Speed", "910", "ms", "", none, "", "ok", none, none, none},
		{"serverdown", "Flowing", "17", "waiting", "", "14 notes and 3 sessions on this PC", "Memory server unreachable since 09:12. Nothing is lost.", "watch", "", "/pipeline?days=7", "Waiting items →"},
		{"broken", "Flowing", "3", "failed", "", "1 set aside · oldest waiting 26 h · 188 of 192 sent", "3 sessions could not be sent; 1 set aside after an error.", "attention", none, none, none},
	} {
		t.Run(c.sc+"/"+c.tile, func(t *testing.T) {
			tl := tileNamed(t, c.sc, c.tile)
			for _, f := range []struct{ name, got, want string }{
				{"big", tl.Big, c.big}, {"unit", tl.Unit, c.unit}, {"ci", tl.CI, c.ci}, {"sub", tl.Sub, c.sub},
				{"note", tl.Note, c.note}, {"word", tl.Word, c.word}, {"delta", deltaText(tl.Delta), c.delta},
				{"link", tl.Link, c.link}, {"link text", tl.LinkText, c.text},
			} {
				if f.want != none && f.got != f.want {
					t.Errorf("%s: %q, want %q", f.name, f.got, f.want)
				}
			}
		})
	}
}

// The Flowing link goes to the waiting items, not to the sessions, and a
// tile with no count says "Events".
func TestTileLinks(t *testing.T) {
	tl := tileNamed(t, "serverdown", "Flowing")
	r := scenarioNamed(t, "serverdown")
	if tl.Link != "/pipeline?days=7" || drillHref(r.Metrics["local.backlog"].Drill, tilePage) != "" {
		t.Errorf("waiting link %q", tl.Link)
	}
	if tl.Delta != nil {
		t.Errorf("delta under the root cause: %+v", tl.Delta)
	}
	r = scenarioNamed(t, "healthy")
	m := r.Metrics["recall.hit_rate"]
	m.Den = nil
	r.Metrics["recall.hit_rate"] = m
	if tl := tilesOf(r, tilePage, en, r.Window.Loc, false)[0].Tiles[0]; tl.LinkText != "Events →" {
		t.Errorf("no count: %q", tl.LinkText)
	}
}

// The tile class follows the status word; broken Flowing is the bad border.
func TestTileClasses(t *testing.T) {
	for _, c := range []struct{ sc, tile, class, wordClass string }{
		{"healthy", "Found", "", "st-ok"}, {"healthy", "Reported wrong", "is-few", "st-few"},
		{"new", "Used", "is-learn", "st-learn"}, {"cantjudge", "Used", "is-unrel", "st-unrel"},
		{"qualitywatch", "Found", "is-watch", "st-watch"}, {"qualitywatch", "Used", "", "st-border"},
		{"broken", "Flowing", "is-bad", "st-bad"},
	} {
		tl := tileNamed(t, c.sc, c.tile)
		if tl.Class != c.class || tl.WordClass != c.wordClass {
			t.Errorf("%s %s: class %q word class %q, want %q %q", c.sc, c.tile, tl.Class, tl.WordClass, c.class, c.wordClass)
		}
	}
}

func TestTileHeads(t *testing.T) {
	tl := tileNamed(t, "healthy", "Context")
	h := tl.Head
	if h.Level != 3 || h.Info.Label != "About Context" || h.Info.Name != "Context: "+def(t, "context.per_session").Question || h.Title != def(t, "context.per_session").Question || h.ID != "w-context-per_session" {
		t.Errorf("head: %+v", h)
	}
	if h.Info.What == "" || h.Info.Limits == "" {
		t.Errorf("the ⓘ lost the metric's definition: %+v", h.Info)
	}
}

func TestTileCLIs(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	got := map[string]string{}
	for _, g := range tilesOf(r, tilePage, en, r.Window.Loc, false) {
		for _, tl := range g.Tiles {
			got[tl.Name] = tl.Head.CLI
		}
	}
	for name, want := range map[string]string{
		"Found": "backsight stats quality --days 7", "Used": "backsight stats sessions --has used --days 7",
		"Reported wrong": "backsight stats sessions --has wrong --days 7", "Context": "backsight stats cost --days 7",
		"Speed": "backsight stats cost --days 7", "Flowing": "backsight stats pipeline --days 7",
	} {
		if got[name] != want {
			t.Errorf("%s: %q, want %q", name, got[name], want)
		}
	}
	// No window flag unless the page set one; the project is quoted.
	g := tilesOf(r, views.Params{Project: "my proj"}, en, r.Window.Loc, false)
	if c := g[0].Tiles[0].Head.CLI; c != "backsight stats quality --project 'my proj'" {
		t.Errorf("project: %q", c)
	}
	// The page's own filters do not leak into a tile's command.
	g = tilesOf(r, views.Params{Has: "wrong", Sort: "used", Limit: 5}, en, r.Window.Loc, false)
	if c := g[0].Tiles[0].Head.CLI; c != "backsight stats quality" {
		t.Errorf("filters: %q", c)
	}
}

func TestTileSparks(t *testing.T) {
	tl := tileNamed(t, "healthy", "Found")
	if !strings.Contains(string(tl.Spark), `aria-label="Found per day, last 7 days: `) || !strings.Contains(string(tl.Spark), "24 %") {
		t.Errorf("spark: %s", tl.Spark)
	}
}

func renderTiles(t *testing.T, r *metrics.Result, l format.Locale, dev bool) *node {
	t.Helper()
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	d.V["overview"] = r
	d.Loc, d.L, d.P = r.Window.Loc, l, tilePage
	d.Frame.Prefs.Dev = dev
	d.CLI = "backsight stats overview"
	var buf bytes.Buffer
	if err := tpls["overview"].ExecuteTemplate(&buf, spec.File, d); err != nil {
		t.Fatal(err)
	}
	return parseHTML(t, buf.String())
}

func TestTileMarkup(t *testing.T) {
	n := renderTiles(t, scenarioNamed(t, "healthy"), en, false)
	tiles := n.find("article.tile")
	var names []string
	for _, a := range tiles {
		h := a.first("h3.tile-name")
		if h == nil || a.has("aria-label") || a.attr("aria-labelledby") != h.attr("id") {
			t.Errorf("tile at line %d: no h3.tile-name it is labelled by", a.line)
			continue
		}
		names = append(names, h.text())
		if a.first("svg.spark[role=img]") == nil {
			t.Errorf("%s: no spark", a.attr("aria-label"))
		}
		if b := a.first("button.copy[hidden]"); b == nil {
			t.Errorf("%s: no hidden copy button", a.attr("aria-label"))
		}
		if a.first("p.tile-q") == nil || a.first("span.big") == nil || a.first("div.tile-st span.st") == nil || a.first("div.tile-foot a") == nil {
			t.Errorf("%s: frame incomplete", a.attr("aria-label"))
		}
	}
	if got := strings.Join(names, ","); got != "Found,Used,Reported wrong,Context,Speed,Flowing" {
		t.Errorf("tiles %q", got)
	}
	var groups []string
	for _, h := range n.find("h2.tgroup-h") {
		groups = append(groups, h.text())
	}
	if got := strings.Join(groups, ","); got != "Found,Right,Cost,Flowing" {
		t.Errorf("groups %q", got)
	}
	if g := n.first("div.tgroups"); g == nil || g.attr("aria-label") != "Key numbers" {
		t.Error("no tgroups region")
	}
	found := n.first("article.tile")
	if got := found.first("span.big").text(); got != "28" {
		t.Errorf("Found big %q", got)
	}
	if d := found.first("span.delta.better"); d == nil || !strings.Contains(d.text(), "+2 pts, better") {
		t.Errorf("Found delta: %v", d)
	}
	if a := found.first("div.tile-foot a"); a.attr("href") != "/sessions?days=7&has=memory" || a.text() != "167 prompts →" {
		t.Errorf("Found link %q %q", a.attr("href"), a.text())
	}
	// A tile with no note renders none; one with a note renders it.
	if found.first("p.tile-note") != nil {
		t.Error("Found has a note")
	}
	if used := tiles[1]; used.first("p.tile-note") == nil {
		t.Error("Used lost its note")
	}
	// A learning tile has no delta and the dashed class.
	ln := renderTiles(t, scenarioNamed(t, "new"), en, false).find("article.tile")[1]
	if !ln.has("class") || !strings.Contains(ln.attr("class"), "is-learn") || ln.first("span.delta") != nil {
		t.Errorf("learning tile: %q", ln.attr("class"))
	}
}

func TestOverviewInGerman(t *testing.T) {
	n := renderTiles(t, scenarioNamed(t, "healthy"), format.FromAcceptLanguage("de-DE"), false)
	tiles := n.find("article.tile")
	if got := tiles[3].first("span.big").text(); got != "3.600" {
		t.Errorf("Context big %q", got)
	}
	if got := tiles[4].first("span.big").text(); got != "1,6" {
		t.Errorf("Speed big %q", got)
	}
}

func TestOverviewDeveloperMode(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	n := renderTiles(t, r, en, true)
	tiles := n.find("article.tile")
	if d := tiles[0].first("span.devid"); d == nil || d.text() != "recall.hit_rate · n 167" {
		t.Errorf("Found devid: %v", d)
	}
	for _, a := range tiles {
		if a.first("span.devid") == nil {
			t.Errorf("%s: no devid", a.attr("aria-label"))
		}
	}
	plain := renderTiles(t, r, en, false)
	if len(plain.find("span.devid")) != 0 {
		t.Error("devid without developer mode")
	}
	var b bytes.Buffer
	tpls, specs, _ := loadPages(Templates())
	d := fixtureData(t, specs["overview"], false)
	d.V["overview"], d.Loc, d.L, d.P = r, r.Window.Loc, en, tilePage
	d.Frame.Prefs.Dev = false
	if err := tpls["overview"].ExecuteTemplate(&b, specs["overview"].File, d); err != nil {
		t.Fatal(err)
	}
	text := visibleText(b.String())
	for _, sp := range tileSpecs {
		if strings.Contains(text, sp.id) {
			t.Errorf("the metric id %s is visible without developer mode", sp.id)
		}
	}
}

// A value with too few events behind it has no number to qualify: no
// tokens, no typical, and the note counts what there is.
func TestTileWithoutAValue(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	for _, id := range []string{"context.per_session", "recall.latency"} {
		m := r.Metrics[id]
		m.Value, m.Status, m.Den, m.Delta = nil, metrics.StatusTooFew, scN(4), nil
		r.Metrics[id] = m
	}
	tiles := tilesOf(r, tilePage, en, r.Window.Loc, false)
	ctx, speed := tiles[2].Tiles[0], tiles[2].Tiles[1]
	if ctx.Big != "—" || ctx.CI != "" || ctx.Sub != "" || ctx.Note != "4 sessions: too few to judge." || ctx.Word != "too few" {
		t.Errorf("context: %+v", ctx)
	}
	if speed.Big != "—" || speed.Sub != "" || speed.Note != "4 prompts: too few to judge." {
		t.Errorf("speed: %+v", speed)
	}
}

func tilesFor(r *metrics.Result) map[string]Tile {
	out := map[string]Tile{}
	for _, g := range tilesOf(r, tilePage, en, r.Window.Loc, false) {
		for _, tl := range g.Tiles {
			out[tl.Name] = tl
		}
	}
	return out
}

// Flowing's note is the first attention item of the axis, whatever metric
// it is about.
func TestFlowingNoteIsTheFirstFlowingItem(t *testing.T) {
	r := scenarioNamed(t, "broken")
	a := r.Attention
	r.Attention = []metrics.Item{a[1], a[0], a[2]} // the waiting backlog first
	if got := tilesFor(r)["Flowing"].Note; got != scWaitedText {
		t.Errorf("note %q, want %q", got, scWaitedText)
	}
}

// Under the root cause the change in sessions sent is not shown, though the
// metric has one.
func TestFlowingHasNoDeltaUnderTheRootCause(t *testing.T) {
	r := scenarioNamed(t, "serverdown")
	m := r.Metrics["pipeline.flow"]
	m.Previous, m.Delta = scF(1), scF(*m.Value-1)
	r.Metrics["pipeline.flow"] = m
	if tl := tilesFor(r)["Flowing"]; tl.Delta != nil || tl.Big != "17" {
		t.Errorf("root cause: delta %+v big %q", tl.Delta, tl.Big)
	}
	// Without the root cause the same delta shows.
	r = scenarioNamed(t, "healthy")
	m = r.Metrics["pipeline.flow"]
	m.Previous, m.Delta = scF(1), scF(*m.Value-1)
	r.Metrics["pipeline.flow"] = m
	if tl := tilesFor(r)["Flowing"]; tl.Delta == nil {
		t.Error("no delta without the root cause")
	}
}

func TestFlowingRootCauseFallbacks(t *testing.T) {
	r := scenarioNamed(t, "serverdown")
	r.Metrics["server.reach"] = scValue("server.reach", metrics.StatusWatch, 96, 0, scExtra(map[string]any{"failing_since": nil})).m
	if got := tilesFor(r)["Flowing"].Note; got != "Memory server unreachable. Nothing is lost." {
		t.Errorf("no failing_since: %q", got)
	}
	// Without the backlog's counts the tile reads the sessions sent.
	r = scenarioNamed(t, "serverdown")
	r.Metrics["local.backlog"] = scValue("local.backlog", metrics.StatusWatch, 17, 0).m
	tl := tilesFor(r)["Flowing"]
	if tl.Big != "82" || tl.Unit != "%" || tl.Sub != "14 of 17 sessions sent" || tl.LinkText == "Waiting items →" {
		t.Errorf("no backlog counts: %+v", tl)
	}
}

// sparkLabel is the label of a tile's spark.
func sparkLabel(t *testing.T, tl Tile) string {
	t.Helper()
	s := string(tl.Spark)
	i := strings.Index(s, `aria-label="`)
	if i < 0 {
		t.Fatalf("no label: %s", s)
	}
	s = s[i+len(`aria-label="`):]
	label, _, _ := strings.Cut(s, `"`)
	return label
}

// The window's first day is cut short and left out; today is kept.
func TestSparkDropsTheCutFirstDay(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	tl := tilesFor(r)["Found"]
	if got, want := sparkLabel(t, tl), "Found per day, last 7 days: 24 %, 31 %, 27 %, 22 %, 30 %, 29 %, 28 %"; got != want {
		t.Errorf("label %q, want %q", got, want)
	}
	if n := strings.Count(string(tl.Spark), "<polyline"); n != 1 {
		t.Errorf("%d lines", n)
	}
	// A series whose first bucket is whole keeps it.
	m := r.Metrics["recall.hit_rate"]
	series := append([]metrics.Point(nil), m.Series...)
	series[0].Partial = false
	m.Series = series
	r.Metrics["recall.hit_rate"] = m
	if got := sparkLabel(t, tilesFor(r)["Found"]); !strings.HasPrefix(got, "Found per day, last 7 days: 24 %, 24 %, 31 %") {
		t.Errorf("whole first day: %q", got)
	}
}

// The tile frame follows its Head: server chip, not-filtered chip, a
// heading id, and no link element for an empty link.
func TestTileFrameBranches(t *testing.T) {
	orig := funcs["tiles"]
	funcs["tiles"] = func(*metrics.Result, views.Params, format.Locale, *time.Location, bool) []TileGroup {
		h := Head{ID: "w-x", Level: 3, Scope: "server", NotFiltered: true}
		return []TileGroup{{Name: "G", Tiles: []Tile{{Name: "T", Head: h, Big: "1"}}}}
	}
	t.Cleanup(func() { funcs["tiles"] = orig })
	n := renderTiles(t, scenarioNamed(t, "healthy"), en, false)
	a := n.first("article.tile")
	if a == nil {
		t.Fatal("no tile")
	}
	if c := a.first("span.chip"); c == nil || c.text() != "server" {
		t.Errorf("chip: %v", c)
	}
	if a.first("span.chip-nf") == nil {
		t.Error("no not-filtered chip")
	}
	if a.first("div.tile-foot a") != nil {
		t.Error("a link for an empty Link")
	}
	if h := a.first("h3.tile-name"); h == nil || h.attr("id") != "w-x" {
		t.Errorf("h3: %v", h)
	}
}

func TestTileMachineChipAndHeadingID(t *testing.T) {
	n := renderTiles(t, scenarioNamed(t, "healthy"), en, false)
	a := n.first("article.tile")
	if c := a.first("span.chip"); c == nil || c.text() != "this PC" {
		t.Errorf("chip: %v", c)
	}
	if a.first("span.chip-nf") != nil {
		t.Error("not-filtered chip on a filtered tile")
	}
	if h := a.first("h3.tile-name"); h.attr("id") != "w-recall-hit_rate" {
		t.Errorf("h3 id %q", h.attr("id"))
	}
}
