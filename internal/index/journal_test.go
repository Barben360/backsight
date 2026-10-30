package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

var (
	jT0 = time.Date(2026, 10, 30, 12, 0, 0, 123_000_000, time.UTC)
	jT1 = jT0.Add(-2 * time.Hour)
)

func (e *ingestEnv) journal() *feedback.Journal {
	return feedback.NewJournal(e.d.FeedbackDir(), feedback.Common{Session: "s1", Project: "proj", Binary: "1.2.3", V: "v10"})
}

func (e *ingestEnv) jpath(month string) string {
	return filepath.Join(e.d.FeedbackDir(), "journal-"+month+".jsonl")
}

func (e *ingestEnv) appendJ(ev feedback.Event) {
	e.t.Helper()
	if err := e.journal().Append(ev); err != nil {
		e.t.Fatal(err)
	}
}

// jline is a journal line written by hand: schema 1, newline included.
func jline(t *testing.T, ev feedback.Event) string {
	t.Helper()
	ev.Schema = feedback.Schema
	return mustJSON(t, ev) + "\n"
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkRows compares a table's rows, ordered by key, with want: every
// column but source_file and offset, each named.
func (e *ingestEnv) checkRows(table, order string, want []map[string]any) {
	e.t.Helper()
	got := e.rowsOf(fmt.Sprintf(`SELECT * FROM %s ORDER BY %s`, table, order))
	if len(got) != len(want) {
		e.t.Fatalf("%s: %d rows %v, want %d", table, len(got), got, len(want))
	}
	for i, g := range got {
		if len(g) != len(want[i])+2 {
			e.t.Errorf("%s row %d: columns %v, want %d named ones plus source_file and offset", table, i, g, len(want[i]))
		}
		for col, w := range want[i] {
			if v, ok := g[col]; !ok || !sameValue(v, w) {
				e.t.Errorf("%s row %d: %s = %#v, want %#v", table, i, col, v, w)
			}
		}
	}
}

func skey(id string) string { return fmt.Sprintf("s1|t1|%d|%s", jT0.UnixNano(), id) }

func ms(t time.Time) int64 { return t.UnixMilli() }

func ptr(f float64) *float64 { return &f }

func TestJournalRows(t *testing.T) {
	e := newEnv(t)
	at := jT0.Add
	score := ptr(0.9)
	showEv := feedback.Event{TS: jT0, Kind: "shows", Turn: "t1", Path: "prompt", Items: []feedback.Item{
		{ID: "m1", Rank: 1, Reranker: 0.5, Project: "p2", Boost: 0.1, RankWithoutBoost: 2, ScoreWithoutBoost: score},
		{ID: "m2", Rank: 2}, // no project: the line's; no score: NULL
	}}
	for _, ev := range []feedback.Event{
		{TS: jT0, Kind: "turn", Turn: "t1", N: 3, Machine: true},
		{TS: jT0.Add(time.Millisecond), Kind: "turn", Turn: "t0"}, // machine false: omitted by the producer
		showEv,
		{TS: at(1 * time.Second), Kind: "credit", Turn: "t1", Agent: "sub1", IDs: []string{"m1", "m2"}, Verdict: "used"},
		{TS: at(2 * time.Second), Kind: "credit", Turn: "t2", Verdict: "none"},
		{TS: at(3 * time.Second), Kind: "stop", Turn: "t1", Outcome: "declared", StopCheck: "post_turn", Flags: []string{"zeta", "alpha"}},
		{TS: at(4 * time.Second), Kind: "settle", Settled: []feedback.Settled{
			{Key: skey("m1"), ID: "m1", At: jT1, Outcome: "credit"},
			{Key: skey("m2"), ID: "m2", Outcome: "unknown"},
		}},
		{TS: at(5 * time.Second), Kind: "forget", IDs: []string{"m1"}},
		{TS: at(6 * time.Second), Kind: "retire", IDs: []string{"m1", "m2"}, Verdict: "wrong", Outcome: "invalidated", Note: "note-1"},
		{TS: at(7 * time.Second), Kind: "hide", IDs: []string{"m3"}, Verdict: "outdated", Note: "n:2"},
		{TS: at(8 * time.Second), Kind: "unhide", IDs: []string{"m3"}, Outcome: "restored"},
		{TS: at(9 * time.Second), Kind: "recall", Turn: "t1", Path: "search", Flags: []string{feedback.FlagSetChanged}, Items: []feedback.Item{
			{ID: "m1", Rank: 1, RankWithoutBoost: 3, Reranker: 0.25, Boost: 0.05, ScoreWithoutBoost: ptr(0)},
		}},
		{TS: at(10 * time.Second), Kind: "import", Source: "transcripts", Settled: []feedback.Settled{
			{Key: "s1|ref|prompt|m1", ID: "m1", At: jT1, Outcome: "credit", Rank: 4},
		}},
		{TS: at(11 * time.Second), Kind: "import", Source: "telemetry", Outcome: feedback.ImportComplete},
		{TS: at(12 * time.Second), Kind: "stale", IDs: []string{"m1"}},
		{TS: at(13 * time.Second), Kind: "from_the_future"},
	} {
		e.appendJ(ev)
	}
	st := e.ingest()
	if st.Lines != 16 || st.Errors != 0 {
		t.Fatalf("stats %+v", st)
	}
	e.checkRows("turns", `"offset"`, []map[string]any{
		{"ts": ms(jT0), "session": "s1", "turn": "t1", "agent": nil, "project": "proj", "n": 3, "machine": 1, "binary_version": "1.2.3"},
		{"ts": ms(jT0) + 1, "session": "s1", "turn": "t0", "agent": nil, "project": "proj", "n": nil, "machine": nil, "binary_version": "1.2.3"},
	})
	key := func(id string) string { return fmt.Sprintf("s1|t1|%d|%s", jT0.UnixNano(), id) }
	e.checkRows("shows", `"offset", pos`, []map[string]any{
		{"pos": 0, "ts": ms(jT0), "session": "s1", "key": key("m1"), "turn": "t1", "agent": nil, "path": "prompt", "memory_id": "m1", "rank": 1,
			"reranker": 0.5, "boost": 0.1, "rank_without_boost": 2, "score_without_boost": 0.9, "project": "p2"},
		{"pos": 1, "ts": ms(jT0), "session": "s1", "key": key("m2"), "turn": "t1", "agent": nil, "path": "prompt", "memory_id": "m2", "rank": 2,
			"reranker": nil, "boost": nil, "rank_without_boost": nil, "score_without_boost": nil, "project": "proj"},
	})
	e.checkRows("credits", `"offset", pos`, []map[string]any{
		{"pos": 0, "ts": ms(at(time.Second)), "session": "s1", "turn": "t1", "agent": "sub1", "project": "proj", "memory_id": "m1", "verdict": "used"},
		{"pos": 1, "ts": ms(at(time.Second)), "session": "s1", "turn": "t1", "agent": "sub1", "project": "proj", "memory_id": "m2", "verdict": "used"},
		{"pos": 0, "ts": ms(at(2 * time.Second)), "session": "s1", "turn": "t2", "agent": nil, "project": "proj", "memory_id": nil, "verdict": "none"},
	})
	e.checkRows("stops", `"offset"`, []map[string]any{
		{"ts": ms(at(3 * time.Second)), "session": "s1", "turn": "t1", "agent": nil, "outcome": "declared", "stop_check": "post_turn", "flags": "alpha,zeta"},
	})
	e.checkRows("settles", `"offset", pos`, []map[string]any{
		{"pos": 0, "key": skey("m1"), "memory_id": "m1", "show_at": ms(jT1), "outcome": "credit", "settle_ts": ms(at(4 * time.Second))},
		{"pos": 1, "key": skey("m2"), "memory_id": "m2", "show_at": nil, "outcome": "unknown", "settle_ts": ms(at(4 * time.Second))},
	})
	e.checkRows("retirements", `"offset", pos`, []map[string]any{
		{"pos": 0, "ts": ms(at(5 * time.Second)), "session": "s1", "kind": "forget", "memory_id": "m1", "verdict": nil, "outcome": nil, "note_id": nil},
		{"pos": 0, "ts": ms(at(6 * time.Second)), "session": "s1", "kind": "retire", "memory_id": "m1", "verdict": "wrong", "outcome": "invalidated", "note_id": "note-1"},
		{"pos": 1, "ts": ms(at(6 * time.Second)), "session": "s1", "kind": "retire", "memory_id": "m2", "verdict": "wrong", "outcome": "invalidated", "note_id": "note-1"},
		{"pos": 0, "ts": ms(at(7 * time.Second)), "session": "s1", "kind": "hide", "memory_id": "m3", "verdict": "outdated", "outcome": nil, "note_id": "n:2"},
		{"pos": 0, "ts": ms(at(8 * time.Second)), "session": "s1", "kind": "unhide", "memory_id": "m3", "verdict": nil, "outcome": "restored", "note_id": nil},
	})
	e.checkRows("recalls", `"offset", pos`, []map[string]any{
		{"pos": 0, "ts": ms(at(9 * time.Second)), "session": "s1", "turn": "t1", "agent": nil, "path": "search", "memory_id": "m1", "rank": 1,
			"rank_without_boost": 3, "reranker": 0.25, "boost": 0.05, "score_without_boost": 0.0, "flags": "set_changed"},
	})
	e.checkRows("imports", `"offset", pos`, []map[string]any{
		{"pos": 0, "ts": ms(at(10 * time.Second)), "source": "transcripts", "key": "s1|ref|prompt|m1", "memory_id": "m1", "at": ms(jT1), "outcome": "credit", "rank": 4},
		{"pos": 0, "ts": ms(at(11 * time.Second)), "source": "telemetry", "key": nil, "memory_id": nil, "at": nil, "outcome": "complete", "rank": nil},
	})
	e.checkRows("journal_other", `"offset"`, []map[string]any{
		{"ts": ms(at(12 * time.Second)), "session": "s1", "kind": "stale"},
		{"ts": ms(at(13 * time.Second)), "session": "s1", "kind": "from_the_future"},
	})
	if n := e.count("ingest_errors"); n != 0 {
		t.Errorf("%d ingest errors", n)
	}
	rs := e.rowsOf(`SELECT kind, last_ts FROM cursors`)
	if len(rs) != 1 || rs[0]["kind"] != "journal" || rs[0]["last_ts"] != ms(at(13*time.Second)) {
		t.Errorf("cursors %v", rs)
	}
}

func TestShowKeysAreTheFeedbackKeys(t *testing.T) {
	e := newEnv(t)
	j2 := feedback.NewJournal(e.d.FeedbackDir(), feedback.Common{Session: "other", Project: "proj2"})
	e.appendJ(feedback.Event{TS: jT0, Kind: "shows", Turn: "t1", Path: "gate", Items: []feedback.Item{{ID: "a"}, {ID: "b"}, {ID: "c"}}})
	if err := j2.Append(feedback.Event{TS: jT0.Add(time.Nanosecond * 7), Kind: "shows", Turn: "", Path: "briefing", Items: []feedback.Item{{ID: "z", Project: "pz"}}}); err != nil {
		t.Fatal(err)
	}
	e.appendJ(feedback.Event{TS: jT0.Add(time.Minute), Kind: "shows", Agent: "sub", Turn: "t2", Path: "read", Items: []feedback.Item{{ID: "a"}}})
	e.ingest()
	events, skipped, err := feedback.Read(e.d.FeedbackDir(), jT0)
	if err != nil || skipped != 0 {
		t.Fatal(err, skipped)
	}
	var want []string
	for _, ev := range events {
		for _, s := range feedback.ShowsOf(ev) {
			want = append(want, s.Key)
		}
	}
	var got []string
	for _, r := range e.rowsOf(`SELECT key FROM shows ORDER BY "offset", pos`) {
		got = append(got, r["key"].(string))
	}
	if len(want) != 5 || !slices.Equal(got, want) {
		t.Errorf("keys\n got %v\nwant %v", got, want)
	}
	if rs := e.rowsOf(`SELECT project FROM shows WHERE memory_id = 'z'`); len(rs) != 1 || rs[0]["project"] != "pz" {
		t.Errorf("project %v", rs)
	}
}

func TestSettleShowAtIsTheShowTime(t *testing.T) {
	e := newEnv(t)
	show := feedback.Event{TS: jT1, Kind: "shows", Turn: "t1", Path: "prompt", Items: []feedback.Item{{ID: "m1", Rank: 1}}}
	e.appendJ(show)
	full := show
	full.Session = "s1"
	k := feedback.ShowsOf(full)[0]
	t2 := jT1.Add(3 * time.Hour)
	e.appendJ(feedback.Event{TS: t2, Kind: "settle", Settled: []feedback.Settled{{Key: k.Key, ID: "m1", At: k.At, Outcome: "credit"}}})
	e.ingest()
	rs := e.rowsOf(`SELECT s.ts AS shown_ts, t.show_at, t.settle_ts FROM settles t JOIN shows s ON s.key = t.key`)
	if len(rs) != 1 {
		t.Fatalf("join: %v", rs)
	}
	if rs[0]["show_at"] != ms(jT1) || rs[0]["settle_ts"] != ms(t2) || rs[0]["shown_ts"] != ms(jT1) {
		t.Errorf("show_at %v settle_ts %v, want %d and %d", rs[0]["show_at"], rs[0]["settle_ts"], ms(jT1), ms(t2))
	}
}

// finalStops is the turn_final_stop view as session/turn/agent/outcome.
func (e *ingestEnv) finalStops() []string {
	e.t.Helper()
	var out []string
	for _, r := range e.rowsOf(`SELECT session, turn, ifnull(agent, ''), outcome FROM turn_final_stop ORDER BY session, turn, agent`) {
		out = append(out, fmt.Sprintf("%v/%v/%v/%v", r["session"], r["turn"], r["ifnull(agent, '')"], r["outcome"]))
	}
	return out
}

func TestFinalStopOutcome(t *testing.T) {
	e := newEnv(t)
	stop := func(session, turn, agent, outcome string, ts time.Time) feedback.Event {
		return feedback.Event{TS: ts, Kind: "stop", Session: session, Turn: turn, Agent: agent, Outcome: outcome}
	}
	for _, ev := range []feedback.Event{
		stop("a", "1", "", "blocked", jT0), stop("a", "1", "", "declared", jT0.Add(time.Second)), // blocked, then declared
		stop("b", "1", "", "blocked", jT0), // a lone blocked
		stop("c", "1", "", "declared", jT0),
		stop("e", "1", "", "declared", jT0), stop("e", "1", "sub1", "blocked", jT0.Add(time.Second)), // a subagent apart
	} {
		e.appendJ(ev)
	}
	// Across month files, in journal order: the later file wins whatever the
	// ts of its line says (hand-written: the producer stamps by month).
	oct := time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC)
	writeFile(t, e.jpath("2026-10"), readFile(t, e.jpath("2026-10"))+jline(t, stop("d", "1", "", "blocked", oct))+jline(t, stop("f", "1", "", "declared", oct)))
	writeFile(t, e.jpath("2026-11"), jline(t, stop("d", "1", "", "undeclared", oct.Add(-24*time.Hour)))+jline(t, stop("f", "1", "", "blocked", oct.Add(-24*time.Hour))))
	e.ingest()
	want := []string{
		"a/1//declared", "b/1//undeclared", "c/1//declared", "d/1//undeclared",
		"e/1//declared", "e/1/sub1/undeclared", "f/1//undeclared",
	}
	if got := e.finalStops(); !slices.Equal(got, want) {
		t.Errorf("final stops\n got %v\nwant %v", got, want)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDocumentStatus(t *testing.T) {
	e := newEnv(t)
	at := func(s int) string { return jT0.Add(time.Duration(s) * time.Second).Format(time.RFC3339Nano) }
	retain := func(doc, status string, s int) string {
		return tline("retain", at(s), withCommon(map[string]any{"document": doc, "items": 1, "status": status}))
	}
	e.write("2026-10", retain("d1", "queued", 0)+retain("d2", "queued", 1)+retain("d1", "ok", 2)+
		retain("d3", "ok", 3)+retain("d3", "failed", 4)+retain("d4", "ok", 5))
	e.ingest()
	var got []string
	for _, r := range e.rowsOf(`SELECT document, status FROM document_status ORDER BY document`) {
		got = append(got, fmt.Sprintf("%v/%v", r["document"], r["status"]))
	}
	if want := []string{"d1/ok", "d2/queued", "d3/failed", "d4/ok"}; !slices.Equal(got, want) {
		t.Errorf("document status\n got %v\nwant %v", got, want)
	}
}

// TestJournalLineErrors: a line this binary cannot read is skipped with
// its reason, whatever follows is read.
func TestJournalLineErrors(t *testing.T) {
	e := newEnv(t)
	good := func(kind string) feedback.Event { return feedback.Event{TS: jT0, Kind: kind, Turn: "t"} }
	other := jline(t, good("turn"))
	lines := []struct{ line, reason string }{
		{"not json\n", "unparsable"},
		{strings.Replace(other, `"schema":1`, `"schema":2`, 1), "schema"},
		{`{"seq":1,"ts":"2026-10-30T12:00:00Z","kind":"turn"}` + "\n", "schema"}, // no schema
		{strings.Replace(other, `"ts":"`, `"tz":"`, 1), "no_ts"},
		{jline(t, feedback.Event{TS: jT0}), "missing_field"}, // no kind
		{strings.Replace(other, `"turn":"t"`, `"turn":5`, 1), "bad_type"},
		{`{"schema":1,"ts":"2026-10-30T12:00:00Z","kind":"shows","items":[{"id":"m","rank":1.5}]}` + "\n", "bad_type"},
		{other, ""},
	}
	var sb strings.Builder
	offs := make([]int64, len(lines))
	for i, l := range lines {
		offs[i] = int64(sb.Len())
		sb.WriteString(l.line)
	}
	writeFile(t, e.jpath("2026-10"), sb.String())
	st := e.ingest()
	if st.Lines != int64(len(lines)) || st.Errors != int64(len(lines)-1) {
		t.Errorf("stats %+v", st)
	}
	rs := e.rowsOf(`SELECT "offset", reason FROM ingest_errors ORDER BY "offset"`)
	if len(rs) != len(lines)-1 {
		t.Fatalf("errors %v", rs)
	}
	for i, r := range rs {
		if r["offset"] != offs[i] || r["reason"] != lines[i].reason {
			t.Errorf("error %d: %v, want offset %d reason %s", i, r, offs[i], lines[i].reason)
		}
	}
	if n := e.count("turns"); n != 1 {
		t.Errorf("%d turns", n)
	}
}

// TestJournalGuards: closed sets, shapes, names and the note id.
func TestJournalGuards(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("n", nameMax+1)
	e.appendJ(feedback.Event{TS: jT0, Kind: "shows", Path: "evil path", Turn: "t", Items: []feedback.Item{{ID: "bad\nid"}, {ID: long}}})
	e.appendJ(feedback.Event{TS: jT0, Kind: "credit", Verdict: "the user said so", IDs: []string{"ok"}})
	e.appendJ(feedback.Event{TS: jT0, Kind: "stop", Outcome: "weird", StopCheck: "Not A Shape", Flags: []string{"fine", "Not A Flag"}})
	e.appendJ(feedback.Event{TS: jT0, Kind: "settle", Settled: []feedback.Settled{{ID: "m", Outcome: "made up"}}})
	for _, note := range []string{"has space", "semi;colon", strings.Repeat("a", 129), "né"} {
		e.appendJ(feedback.Event{TS: jT0, Kind: "hide", IDs: []string{"m"}, Verdict: note, Outcome: note, Note: note})
	}
	e.appendJ(feedback.Event{TS: jT0, Kind: "retire", IDs: []string{"m"}, Note: strings.Repeat("a", 128)})
	e.appendJ(feedback.Event{TS: jT0, Kind: "import", Source: "somewhere", Settled: []feedback.Settled{{ID: "m", Outcome: "x"}}})
	e.appendJ(feedback.Event{TS: jT0, Kind: "Not A Shape!"})
	e.ingest()
	str := func(q string) []string {
		var out []string
		for _, r := range e.rowsOf(q) {
			for _, v := range r {
				out = append(out, fmt.Sprint(v))
			}
		}
		return out
	}
	if got := str(`SELECT ifnull(path, 'NULL') FROM shows ORDER BY pos`); !slices.Equal(got, []string{"other", "other"}) {
		t.Errorf("show path %v", got)
	}
	if got := str(`SELECT ifnull(memory_id, 'NULL') FROM shows ORDER BY pos`); !slices.Equal(got, []string{"NULL", "NULL"}) {
		t.Errorf("show ids %v", got)
	}
	if got := str(`SELECT verdict FROM credits`); !slices.Equal(got, []string{"other"}) {
		t.Errorf("verdict %v", got)
	}
	if got := e.rowsOf(`SELECT outcome, stop_check, flags FROM stops`); got[0]["outcome"] != "other" || got[0]["stop_check"] != "other" || got[0]["flags"] != "fine,other" {
		t.Errorf("stop %v", got)
	}
	if got := str(`SELECT outcome FROM settles`); !slices.Equal(got, []string{"other"}) {
		t.Errorf("settle outcome %v", got)
	}
	for _, r := range e.rowsOf(`SELECT verdict, outcome, note_id FROM retirements WHERE kind = 'hide'`) {
		if r["note_id"] != nil || r["verdict"] != "other" || r["outcome"] != "other" {
			t.Errorf("hide %v", r)
		}
	}
	if got := e.rowsOf(`SELECT note_id FROM retirements WHERE kind = 'retire'`); got[0]["note_id"] != strings.Repeat("a", 128) {
		t.Errorf("a 128-character note id was refused: %v", got)
	}
	if got := e.rowsOf(`SELECT source, outcome FROM imports`); got[0]["source"] != "other" || got[0]["outcome"] != "other" {
		t.Errorf("import %v", got)
	}
	if got := str(`SELECT kind FROM journal_other`); !slices.Equal(got, []string{"other"}) {
		t.Errorf("kind %v", got)
	}
}

func TestJournalTextNeverStored(t *testing.T) {
	e := newEnv(t)
	e.appendJ(feedback.Event{TS: jT0, Kind: "hide", IDs: []string{"m1"}, Verdict: "wrong", Note: "the user said X"})
	canaries := []string{"T1", "T2", "T3", "T4", "T5"}
	raw := `{"schema":1,"ts":"2026-10-30T12:00:01Z","kind":"shows","session":"s","turn":"t","path":"prompt","text":"T2","prompt":"T3","error":"T4",` +
		`"items":[{"id":"m1","rank":1,"text":"T1","memory":"T5"}]}` + "\n" +
		`{"schema":1,"ts":"2026-10-30T12:00:02Z","kind":"settle","settled":[{"key":"k","id":"m1","outcome":"credit","text":"T1"}],"params":{"T5":1}}` + "\n" +
		`{"schema":1,"ts":"2026-10-30T12:00:03Z","kind":"credit","ids":["m1"],"verdict":"used","reply":"T2","config":"T3"}` + "\n"
	f, err := os.OpenFile(e.jpath("2026-10"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	st := e.ingest()
	if st.Errors != 0 || e.count("shows") != 1 || e.count("settles") != 1 || e.count("credits") != 1 {
		t.Fatalf("stats %+v, rows not ingested", st)
	}
	if rs := e.rowsOf(`SELECT note_id FROM retirements`); len(rs) != 1 || rs[0]["note_id"] != nil {
		t.Errorf("note_id %v, want NULL", rs)
	}
	for _, c := range append(canaries, "the user said X") {
		if hits := textAnywhere(t, e.ix, c); len(hits) != 0 {
			t.Errorf("%q stored: %v", c, hits)
		}
		noBytes(t, e.ix, c)
	}
}

func TestJournalPartialTailWaits(t *testing.T) {
	e := newEnv(t)
	first := jline(t, feedback.Event{TS: jT0, Kind: "turn", Turn: "t1"})
	second := jline(t, feedback.Event{TS: jT0, Kind: "turn", Turn: "t2"})
	writeFile(t, e.jpath("2026-10"), first+second[:len(second)/2])
	if st := e.ingest(); st.Lines != 1 || st.Errors != 0 {
		t.Errorf("stats %+v", st)
	}
	if e.count("turns") != 1 {
		t.Errorf("%d turns, the torn line must wait", e.count("turns"))
	}
	writeFile(t, e.jpath("2026-10"), first+second)
	if st := e.ingest(); st.Lines != 1 || st.Errors != 0 || e.count("turns") != 2 {
		t.Errorf("after completion: %+v, %d turns", st, e.count("turns"))
	}
}

func TestJournalSourcesOrder(t *testing.T) {
	e := newEnv(t)
	line := tline("prompt", ts0, map[string]any{"decision": "skip"})
	e.write("2026-10", line)
	e.write("2026-09", line)
	for _, m := range []string{"2026-09", "2026-10", "2026-11"} {
		writeFile(t, e.jpath(m), jline(t, feedback.Event{TS: jT0, Kind: "turn"}))
	}
	writeFile(t, filepath.Join(e.d.FeedbackDir(), "journal-2026-10.jsonl.bak"), "x\n")
	writeFile(t, filepath.Join(e.d.FeedbackDir(), "ledger.json"), "{}")
	writeFile(t, filepath.Join(e.d.FeedbackDir(), "journal-2026-13.jsonl"), "x\n")
	srcs, err := e.ix.sources()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range srcs {
		got = append(got, s.Kind+":"+s.Rel)
	}
	want := []string{
		"journal:feedback/journal-2026-11.jsonl",
		"telemetry:telemetry/events-2026-10.jsonl", "journal:feedback/journal-2026-10.jsonl",
		"telemetry:telemetry/events-2026-09.jsonl", "journal:feedback/journal-2026-09.jsonl",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sources\n got %v\nwant %v", got, want)
	}
}

// jFreshDump is the dump of a new index over a copy of e's telemetry and
// journal files.
func jFreshDump(t *testing.T, e *ingestEnv) string {
	t.Helper()
	d := tempDirs(t)
	for _, dirs := range [][2]string{{e.d.TelemetryDir(), d.TelemetryDir()}, {e.d.FeedbackDir(), d.FeedbackDir()}} {
		ents, err := os.ReadDir(dirs[0])
		if err != nil {
			continue
		}
		for _, ent := range ents {
			writeFile(t, filepath.Join(dirs[1], ent.Name()), readFile(t, filepath.Join(dirs[0], ent.Name())))
		}
	}
	f := envAt(t, d)
	f.ingest()
	return dump(t, f.ix)
}

func TestBackDatedLineInAnEarlierMonth(t *testing.T) {
	e := newEnv(t)
	oct := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	nov := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	e.appendJ(feedback.Event{TS: oct, Kind: "turn", Turn: "t1"})
	e.appendJ(feedback.Event{TS: oct.Add(time.Hour), Kind: "hide", IDs: []string{"m1"}, Verdict: "wrong"})
	e.appendJ(feedback.Event{TS: nov, Kind: "turn", Turn: "t2"})
	e.ingest()
	if e.count("retirements") != 1 {
		t.Fatal("setup")
	}
	back := oct.Add(2 * time.Hour) // earlier than the November file's last line
	if err := e.journal().AppendFolded(feedback.Event{TS: back, Kind: "unhide", IDs: []string{"m1"}, Outcome: "restored"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.jpath("2026-11")); err != nil {
		t.Fatal(err)
	}
	st := e.ingest()
	if st.Lines != 1 || st.Errors != 0 {
		t.Errorf("stats %+v, want exactly the back-dated line", st)
	}
	rs := e.rowsOf(`SELECT kind, ts, source_file FROM retirements WHERE kind = 'unhide'`)
	if len(rs) != 1 || rs[0]["ts"] != ms(back) || rs[0]["source_file"] != "feedback/journal-2026-10.jsonl" {
		t.Errorf("unhide %v", rs)
	}
	if e.count("retirements") != 2 || e.count("turns") != 2 {
		t.Errorf("%d retirements, %d turns", e.count("retirements"), e.count("turns"))
	}
	if got, want := dump(t, e.ix), jFreshDump(t, e); got != want {
		t.Errorf("incremental differs from a rebuild\n%s\n---\n%s", got, want)
	}
}

func TestJournalIncrementalEqualsRebuild(t *testing.T) {
	e := newEnv(t)
	oct := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	nov := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	dec := time.Date(2026, 12, 2, 9, 0, 0, 0, time.UTC)
	step := func() { e.ingest() }

	e.appendJ(feedback.Event{TS: oct, Kind: "shows", Turn: "t1", Path: "prompt", Items: []feedback.Item{{ID: "m1", Rank: 1}, {ID: "m2", Rank: 2}}})
	e.write("2026-10", tline("prompt", oct.Format(time.RFC3339Nano), map[string]any{"decision": "recall"}))
	step()
	e.appendJ(feedback.Event{TS: nov, Kind: "credit", Turn: "t1", IDs: []string{"m1"}, Verdict: "used"})
	step()
	// a torn tail in November, completed later
	whole := jline(t, feedback.Event{Schema: 1, TS: nov.Add(time.Minute), Kind: "stop", Session: "s1", Turn: "t1", Outcome: "declared"})
	f, err := os.OpenFile(e.jpath("2026-11"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(whole[:20])
	step()
	_, _ = f.WriteString(whole[20:])
	_ = f.Close()
	step()
	// a back-dated line, a December month, garbage
	if err := e.journal().AppendFolded(feedback.Event{TS: oct.Add(time.Hour), Kind: "hide", IDs: []string{"m2"}, Verdict: "wrong"}); err != nil {
		t.Fatal(err)
	}
	step()
	e.appendJ(feedback.Event{TS: dec, Kind: "settle", Settled: []feedback.Settled{{Key: "k", ID: "m1", At: oct, Outcome: "shown"}}})
	g, err := os.OpenFile(e.jpath("2026-12"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = g.WriteString("garbage\n")
	_ = g.Close()
	step()
	if n := e.count("ingest_errors"); n != 1 {
		t.Errorf("%d ingest errors", n)
	}
	if got, want := dump(t, e.ix), jFreshDump(t, e); got != want {
		t.Errorf("incremental differs from a rebuild\n%s\n---\n%s", got, want)
	}

	// a month moved away by hand keeps its rows
	before := jFreshDump(t, e)
	if err := os.Rename(e.jpath("2026-11"), filepath.Join(t.TempDir(), "journal-2026-11.jsonl")); err != nil {
		t.Fatal(err)
	}
	step()
	strip := func(s string) string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.HasPrefix(l, "cursors:") || !strings.Contains(l, "journal-2026-11") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if got, want := strip(dump(t, e.ix)), strip(before); got != want {
		t.Errorf("after the move\n%s\n---\n%s", got, want)
	}
	if rs := e.rowsOf(`SELECT missing FROM cursors WHERE source_file = 'feedback/journal-2026-11.jsonl'`); len(rs) != 1 || rs[0]["missing"] != int64(1) {
		t.Errorf("moved month's cursor %v", rs)
	}
	if e.count("credits") != 1 || e.count("stops") != 1 {
		t.Errorf("rows of the moved month lost: %d credits, %d stops", e.count("credits"), e.count("stops"))
	}
}

func TestJournalRetirementsAndKeys(t *testing.T) {
	e := newEnv(t)
	e.appendJ(feedback.Event{TS: jT0, Kind: "retire", Verdict: "wrong"})                                                // no id: no row
	e.appendJ(feedback.Event{TS: jT0.Add(time.Second), Kind: "hide", IDs: []string{"bad\nid", "ok"}, Verdict: "wrong"}) // a bad id: a NULL row, the event happened
	e.appendJ(feedback.Event{TS: jT0.Add(2 * time.Second), Kind: "credit", Verdict: "none"})                            // credits keep their single NULL row
	e.appendJ(feedback.Event{TS: jT0.Add(3 * time.Second), Kind: "shows", Turn: "t", Path: "prompt", Items: []feedback.Item{{ID: "m"}}})
	e.appendJ(feedback.Event{TS: jT0.Add(4 * time.Second), Kind: "settle", Settled: []feedback.Settled{
		{Key: "s1|t|the user said hello world|m", ID: "m", Outcome: "shown"},
		{Key: "s1|a b c|1|m", ID: "m", Outcome: "shown"},
		{Key: "free text", ID: "m", Outcome: "shown"},
		{Key: "s1|t|123|m", ID: "m", Outcome: "shown"},
		{Key: "s1||123|m", ID: "m", Outcome: "shown"},
	}})
	e.appendJ(feedback.Event{TS: jT0.Add(5 * time.Second), Kind: "import", Source: "telemetry"}) // empty import: one bare row
	e.ingest()
	var got []string
	for _, r := range e.rowsOf(`SELECT kind, ifnull(memory_id, 'NULL') AS id FROM retirements ORDER BY "offset", pos`) {
		got = append(got, fmt.Sprint(r["kind"], "/", r["id"]))
	}
	if want := []string{"hide/NULL", "hide/ok"}; !slices.Equal(got, want) {
		t.Errorf("retirements %v, want %v", got, want)
	}
	if n := e.count("credits"); n != 1 {
		t.Errorf("%d credits", n)
	}
	var keys []string
	for _, r := range e.rowsOf(`SELECT ifnull(key, 'NULL') AS k FROM settles ORDER BY pos`) {
		keys = append(keys, r["k"].(string))
	}
	if want := []string{"NULL", "NULL", "NULL", "s1|t|123|m", "s1||123|m"}; !slices.Equal(keys, want) {
		t.Errorf("settle keys %v, want %v", keys, want)
	}
	if rs := e.rowsOf(`SELECT key FROM shows`); rs[0]["key"] != fmt.Sprintf("s1|t|%d|m", jT0.Add(3*time.Second).UnixNano()) {
		t.Errorf("show key %v", rs)
	}
	e.checkRows("imports", "pos", []map[string]any{
		{"pos": 0, "ts": ms(jT0.Add(5 * time.Second)), "source": "telemetry", "key": nil, "memory_id": nil, "at": nil, "outcome": nil, "rank": nil},
	})
}
