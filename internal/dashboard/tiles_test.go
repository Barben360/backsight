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
	funcs["tiles"] = func(*metrics.Result, views.Params, format.Locale, *time.Location, bool, ...*metrics.Result) []TileGroup {
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

// allTile is the tile called name of the merged Overview r, with this PC's own
// answer own, the page asking for all PCs.
func allTile(t *testing.T, r, own *metrics.Result, p views.Params, name string) Tile {
	t.Helper()
	for _, g := range tilesOf(r, p, en, r.Window.Loc, false, own) {
		for _, tl := range g.Tiles {
			if tl.Name == name {
				return tl
			}
		}
	}
	t.Fatalf("no tile %s", name)
	return Tile{}
}

// speedAs is the merged Speed metric of the design's boards: status st over
// 325 prompts, the slowest 1 in 20 in [lo, hi] ms and the typical one in
// p50.
func speedAs(r *metrics.Result, st metrics.Status, value, prev float64, lo int64, hi *int64, p50 []any) {
	p := scValue("recall.latency", st, value, 325, scTags("approximate"), scDeltaFrom(prev), scMDC(300),
		scExtra(map[string]any{"p50": 840.0, "p95": 1600.0, "range_ms": rangeMS(lo, hi), "p50_range_ms": p50}), scDaily(1000, 1.5, 1.5, 1.5, 1.5, 1.5, 1.5, 1.5))
	r.Metrics["recall.latency"] = p.m
}

// Speed from merged buckets is a range, labelled approximate, with the
// status word the limits on bucket edges give exactly; this PC's own tile is
// the exact value as shipped.
func TestSpeedTileShowsTheBucketRange(t *testing.T) {
	own := scenarioNamed(t, "healthy")
	for _, c := range []struct {
		name                       string
		st                         metrics.Status
		value, prev                float64
		lo                         int64
		hi                         *int64
		big, unit, word, delta     string
		subHas, subHasNot, noteHas string
		class                      string
	}{
		{"ok", metrics.StatusOK, 2000, 2000, 1000, i64p(2000), "1.0–2.0", "s", "ok", "= same bucket as the week before",
			"slowest 1 in 20, from the PCs’ time buckets · typical 500 ms–1.0 s", "limit", "A range, not a value: other PCs send their times in buckets", ""},
		{"watch", metrics.StatusWatch, 2500, 2000, 2000, i64p(2500), "2.0–2.5", "s", "watch", "▲ one bucket slower, worse",
			"· limit 2.5 s", "", "Over 2.0 s for sure: the limits sit on bucket edges", "is-watch"},
		{"attention", metrics.StatusBad, 5000, 2000, 2500, i64p(5000), "2.5–5.0", "s", "attention", "▲ 2 buckets slower, worse",
			"· limit 2.5 s", "", "Over 2.5 s for sure", "is-bad"},
		{"open", metrics.StatusBad, 5000, 2000, 5000, nil, "over 5.0", "s", "attention", "▲ 3 buckets slower, worse",
			"· limit 2.5 s", "", "Over 2.5 s for sure", "is-bad"},
		{"adjacent", metrics.StatusBad, 5000, 2500, 2500, i64p(5000), "2.5–5.0", "s", "attention", "▲ one bucket slower, worse",
			"· limit 2.5 s", "", "Over 2.5 s for sure", "is-bad"},
		{"faster", metrics.StatusOK, 1000, 2000, 500, i64p(1000), "500 ms–1.0", "s", "ok", "▼ one bucket faster, better",
			"typical 500 ms–1.0 s", "limit", "A range, not a value", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := allPCsOverview(t)
			speedAs(r, c.st, c.value, c.prev, c.lo, c.hi, []any{500.0, 1000.0})
			tl := allTile(t, r, own, allPCsPage, "Speed")
			if tl.Big != c.big || tl.Unit != c.unit || tl.CI != "approximate" || !tl.Approx {
				t.Errorf("value %q %q %q approx %t, want %q %q approximate", tl.Big, tl.Unit, tl.CI, tl.Approx, c.big, c.unit)
			}
			if tl.Word != c.word || tl.Class != c.class {
				t.Errorf("word %q class %q, want %q %q", tl.Word, tl.Class, c.word, c.class)
			}
			if got := deltaText(tl.Delta); got != c.delta {
				t.Errorf("delta %q, want %q", got, c.delta)
			}
			if !strings.Contains(tl.Sub, c.subHas) || c.subHasNot != "" && strings.Contains(tl.Sub, c.subHasNot) {
				t.Errorf("sub %q, want %q without %q", tl.Sub, c.subHas, c.subHasNot)
			}
			if !strings.HasPrefix(tl.Note, c.noteHas) {
				t.Errorf("note %q, want %q", tl.Note, c.noteHas)
			}
			if tl.Head.PCs == nil || tl.Head.PCs.Text != "all PCs (4)" {
				t.Errorf("chip %+v", tl.Head.PCs)
			}
			// The page: the range in the big number, the italic word and the chip.
			art := renderAllPCs(t, r, own, nil, allPCsPage).find("article.tile")[4]
			if big := art.first("span.big").text(); big != c.big {
				t.Errorf("drawn value %q", big)
			}
			if ci := art.first("span.ci.approx"); ci == nil || ci.text() != "approximate" {
				t.Errorf("no span.ci.approx: %v", ci)
			}
			if chip := art.first("span.chip"); chip == nil || chip.text() != "all PCs (4)" || art.first("span.i-pc") != nil {
				t.Errorf("chip %v", chip)
			}
		})
	}

	// Too few prompts: no range to show, no claim.
	r := allPCsOverview(t)
	m := r.Metrics["recall.latency"]
	m.Value, m.Status, m.Delta, m.Previous = nil, metrics.StatusTooFew, nil, nil
	m.Extra = map[string]any{"range_ms": (*[2]*int64)(nil), "p50_range_ms": nil}
	r.Metrics["recall.latency"] = m
	if tl := allTile(t, r, own, allPCsPage, "Speed"); tl.Big != "—" || tl.Approx || tl.Word != "too few" {
		t.Errorf("no range: %q approx %t word %q", tl.Big, tl.Approx, tl.Word)
	}

	// This PC's tile is as shipped: the exact value, no italic word, "this PC".
	art := renderTiles(t, scenarioNamed(t, "healthy"), en, false).find("article.tile")[4]
	if art.first("span.big").text() != "1.6" || art.first("span.ci") != nil || art.first("span.chip").text() != "this PC" {
		t.Errorf("this PC's Speed tile changed: %s", art.text())
	}
}

// Context on all PCs is the average per session; the status stays this PC's
// high value, which the tile names beside the average.
func TestContextTileShowsTheAverage(t *testing.T) {
	r, own := allPCsOverview(t), scenarioNamed(t, "healthy")
	tl := allTile(t, r, own, allPCsPage, "Context")
	for _, f := range []struct{ name, got, want string }{
		{"big", tl.Big, "2,300"}, {"unit", tl.Unit, "chars"}, {"ci", tl.CI, "≈ 580 tokens"},
		{"sub", tl.Sub, "average per session, all PCs · this PC’s high value 3,600"},
		{"note", tl.Note, "Other PCs send totals, not sizes: the status is judged on this PC’s high value."},
		{"word", tl.Word, "ok"}, {"delta", deltaText(tl.Delta), ""}, {"link", tl.LinkText, "This PC’s 34 sessions →"},
	} {
		if f.got != f.want {
			t.Errorf("%s: %q, want %q", f.name, f.got, f.want)
		}
	}
	if tl.Head.PCs == nil || tl.Head.PCs.Text != "all PCs (4)" || tl.Approx {
		t.Errorf("head %+v approx %t", tl.Head.PCs, tl.Approx)
	}

	// This PC's status too few: the average still shows (not "715,300 of 311").
	m := r.Metrics["context.per_session"]
	m.Status = metrics.StatusTooFew
	r.Metrics["context.per_session"] = m
	if tl := allTile(t, r, own, allPCsPage, "Context"); tl.Big != "2,300" || tl.Word != "too few" || tl.Delta != nil {
		t.Errorf("too few on this PC: %q %q %v", tl.Big, tl.Word, tl.Delta)
	}
	// No high value on this PC (under its minimum): the sub says what it can.
	m.Extra = map[string]any{"p50": nil, "p95": nil}
	r.Metrics["context.per_session"] = m
	if tl := allTile(t, r, own, allPCsPage, "Context"); tl.Sub != "average per session, all PCs" {
		t.Errorf("sub without a high value: %q", tl.Sub)
	}
	// This PC alone: the high value and its delta, as shipped.
	if tl := tileNamed(t, "healthy", "Context"); tl.Big != "3,600" || tl.Sub != "high · typical 2,100 chars" || tl.Delta == nil {
		t.Errorf("this PC's Context tile changed: %+v", tl)
	}
}

// Reported wrong leaves "too few" once the verdicts of all PCs are added up,
// and the tile says why; added up and still too few, it stays too few.
func TestWrongTileLeavesTooFewWhenMerged(t *testing.T) {
	r, own := allPCsOverview(t), scenarioNamed(t, "healthy") // this PC alone: 1 of 14, too few
	tl := allTile(t, r, own, allPCsPage, "Reported wrong")
	if tl.Big != "12" || tl.Unit != "%" || tl.CI != "(4–30 %)" || tl.Word != "ok" || tl.Sub != "3 of 25 verdicts" {
		t.Errorf("merged: %q %q %q %q %q", tl.Big, tl.Unit, tl.CI, tl.Word, tl.Sub)
	}
	if want := "Enough verdicts once the PCs are added up (20 needed)."; tl.Note != want {
		t.Errorf("note %q, want %q", tl.Note, want)
	}
	// Without this PC's answer there is nothing to compare: no claim.
	if got := allTile(t, r, nil, allPCsPage, "Reported wrong").Note; got != "" {
		t.Errorf("note without this PC's answer: %q", got)
	}
	// Added up and still under 20.
	m := r.Metrics["verdict.wrong_rate"]
	m.Status, m.Num, m.Den, m.CI = metrics.StatusTooFew, scN(2), scN(15), nil
	r.Metrics["verdict.wrong_rate"] = m
	tl = allTile(t, r, own, allPCsPage, "Reported wrong")
	if tl.Big != "2 of 15" || tl.Word != "too few" || strings.Contains(tl.Note, "Enough verdicts") {
		t.Errorf("still too few: %q %q %q", tl.Big, tl.Word, tl.Note)
	}
	// This PC's tile keeps its note.
	if got := tileNamed(t, "healthy", "Reported wrong"); got.Note != "1 of 14: too few to judge." || got.Big != "1 of 14" {
		t.Errorf("this PC: %q %q", got.Big, got.Note)
	}
}

// A merged tile under a project filter says the other PCs are counted whole;
// without a filter, or with this PC selected, it does not.
func TestMergedTilesSayOtherPCsNotFiltered(t *testing.T) {
	own := scenarioNamed(t, "healthy")
	filtered := allPCsOverview(t)
	for id, m := range filtered.Metrics {
		if metrics.MergedMetric(id) {
			m.Tags = append(append([]string{}, m.Tags...), "others_not_filtered")
			filtered.Metrics[id] = m
		}
	}
	p := allPCsPage
	p.Project = "backsight"
	doc := renderAllPCs(t, filtered, own, nil, p)
	tiles := doc.find("article.tile")
	if len(tiles) != 6 {
		t.Fatalf("%d tiles", len(tiles))
	}
	for _, art := range tiles {
		nf := art.find("span.chip-nf")
		if len(nf) != 1 || nf[0].text() != "other PCs not filtered" ||
			nf[0].attr("title") != "The project filter applies to this PC only: the other PCs’ figures are counted whole." {
			t.Errorf("%s: chips %v", art.first("h3").text(), nf)
		}
		if c := art.first("span.chip"); c == nil || c.text() != "all PCs (4)" {
			t.Errorf("%s: first chip %v", art.first("h3").text(), c)
		}
	}
	for _, g := range tilesOf(filtered, p, en, filtered.Window.Loc, false, own) {
		for _, tl := range g.Tiles {
			if !tl.Head.OthersNF {
				t.Errorf("%s: OthersNF false", tl.Name)
			}
		}
	}
	// No project filter: the tag is absent and so is the chip.
	for _, art := range renderAllPCs(t, allPCsOverview(t), own, nil, allPCsPage).find("article.tile") {
		if nf := art.find("span.chip-nf"); len(nf) != 0 {
			t.Errorf("%s: chip without a filter: %v", art.first("h3").text(), nf)
		}
	}
	// This PC selected: the tiles carry neither chip.
	for _, art := range renderTiles(t, scenarioNamed(t, "healthy"), en, false).find("article.tile") {
		if art.first("span.chip-nf") != nil || art.first("span.chip").text() != "this PC" {
			t.Errorf("this PC's tile %s changed", art.first("h3").text())
		}
	}
}

// A merged tile's link leads to this PC's events, and says how many: the
// others' never reach this PC. A PC with no activity shows in "Based on".
func TestTileLinkNamesThisPCsCount(t *testing.T) {
	r, own := allPCsOverview(t), scenarioNamed(t, "healthy")
	u := own.Metrics["used.turn_rate"]
	u.Den = scN(40) // the Used link counts the turns Claude declared (61), not the ones that used memory
	own.Metrics["used.turn_rate"] = u
	want := map[string]string{
		"Found": "This PC’s 167 prompts →", "Used": "This PC’s 61 turns →", "Reported wrong": "This PC’s 14 verdicts →",
		"Context": "This PC’s 34 sessions →", "Speed": "This PC’s 167 prompts →", "Flowing": "This PC’s 239 sessions →",
	}
	for name, w := range want {
		if got := allTile(t, r, own, allPCsPage, name).LinkText; got != w {
			t.Errorf("%s: %q, want %q", name, got, w)
		}
	}
	// The Found tile's line says what this PC alone had.
	if got := allTile(t, r, own, allPCsPage, "Found").Sub; got != "98 of 325 prompts · this PC 46 of 167" {
		t.Errorf("Found's sub %q", got)
	}
	if got := allTile(t, r, nil, allPCsPage, "Found").Sub; got != "98 of 325 prompts" {
		t.Errorf("Found's sub without this PC's answer %q", got)
	}
	// Without this PC's answer the link still says whose events it leads to.
	if got := allTile(t, r, nil, allPCsPage, "Found").LinkText; got != "This PC’s events →" {
		t.Errorf("without this PC's answer: %q", got)
	}
	// The link still goes to this PC's events.
	if got := allTile(t, r, own, allPCsPage, "Found").Link; got != "/sessions?days=7&has=memory" {
		t.Errorf("link %q", got)
	}
	// With this PC selected, the shipped words.
	if got := tileNamed(t, "healthy", "Found").LinkText; got != "167 prompts →" {
		t.Errorf("this PC: %q", got)
	}
	// 2 of the 4 PCs had activity: the ⓘ says on how many the figure rests.
	tl := allTile(t, r, own, allPCsPage, "Found")
	if got := tl.Head.Info.BasedOn; got != "325 prompts on the 2 PCs with activity this week" {
		t.Errorf("based on %q", got)
	}
	r.Scope.PCs = b8PCs(4)
	if got := allTile(t, r, own, allPCsPage, "Found").Head.Info.BasedOn; got != "325 prompts" {
		t.Errorf("every PC active: %q", got)
	}
	r.Scope.PCs = b8PCs(1)
	if got := allTile(t, r, own, allPCsPage, "Found").Head.Info.BasedOn; got != "325 prompts on the 1 PC with activity this week" {
		t.Errorf("one PC active: %q", got)
	}
}

// The items of "What should I do?" are this PC's: a tile that adds up the PCs
// does not carry one in its note (it would quote this PC's number under all
// PCs' figure), unless its status is this PC's (Context, Flowing).
func TestMergedTileLeavesThisPCsItemsToTheirCard(t *testing.T) {
	own := scenarioNamed(t, "healthy")
	r := allPCsOverview(t)
	r.Attention = []metrics.Item{
		scItem("recall.hit_rate", metrics.SeverityWatch, "found", scFoundText, "backsight stats quality", "why"),
		scItem("pipeline.flow", metrics.SeverityWatch, "flowing", scWaitedText, "backsight doctor", "why"),
		scItem("context.per_session", metrics.SeverityWatch, "cost", "Context is high on this PC.", "backsight stats cost", "why"),
	}
	if got := allTile(t, r, own, allPCsPage, "Found").Note; got != "" {
		t.Errorf("Found on all PCs quotes this PC's item: %q", got)
	}
	if got := allTile(t, r, own, allPCsPage, "Flowing").Note; got != scWaitedText {
		t.Errorf("Flowing's status is this PC's: note %q", got)
	}
	if got := allTile(t, r, own, allPCsPage, "Context").Note; got != "Context is high on this PC." {
		t.Errorf("Context's status is this PC's: note %q", got)
	}
	// With this PC selected the Found tile says it.
	one := scenarioNamed(t, "qualitywatch")
	if got := allTile(t, one, nil, tilePage, "Found").Note; got != scFoundText {
		t.Errorf("this PC: %q", got)
	}
}
