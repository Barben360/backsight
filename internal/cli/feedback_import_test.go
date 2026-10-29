package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

const (
	histA = "0a000000-0000-4000-8000-00000000000a"
	histB = "0b000000-0000-4000-8000-00000000000b"
	histC = "0c000000-0000-4000-8000-00000000000c"
	histD = "0d000000-0000-4000-8000-00000000000d"
)

var histStart = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC) // before logStart, where the live journal begins

// writeTelemetry appends events to home's telemetry, each in the month
// file of its "ts".
func writeTelemetry(t *testing.T, home string, events ...map[string]any) {
	t.Helper()
	dir := pathsOf(home).TelemetryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		ts := e["ts"].(time.Time)
		e["ts"] = ts.UTC().Format("2006-01-02T15:04:05.000Z")
		raw, _ := json.Marshal(e)
		f, err := os.OpenFile(filepath.Join(dir, "events-"+ts.UTC().Format("2006-01")+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(append(raw, '\n'))
		_ = f.Close()
	}
}

// writeHistoryTranscript writes a transcript under home's Claude Code projects
// directory.
func writeHistoryTranscript(t *testing.T, home, session string, records ...map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-home-u-billing")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range records {
		r["sessionId"] = session
		r["timestamp"] = r["timestamp"].(time.Time).Format(time.RFC3339Nano)
		raw, _ := json.Marshal(r)
		b.Write(append(raw, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func trRec(typ, uuid string, at time.Time, content any) map[string]any {
	return map[string]any{"type": typ, "uuid": uuid, "timestamp": at, "message": map[string]any{"role": typ, "content": content}}
}

// history writes a little earlier history: session s-tel in telemetry
// (turn 1 injects A and the reply cites it; turn 2 injects A and B,
// searches C, and its reply cites memory; its transcript repeats the
// search), session s-old only in a transcript (a search that returns D,
// in the format without ranks, and a reply that cites it), which a
// resumed session's transcript repeats.
func history(t *testing.T, home string) {
	t.Helper()
	at := func(m int) time.Time { return histStart.Add(time.Duration(m) * time.Minute) }
	writeTelemetry(t, home,
		map[string]any{"ts": at(0), "event": "prompt", "session": "s-tel", "n": 1, "decision": "recall", "ids": []string{histA}},
		map[string]any{"ts": at(1), "event": "reply", "session": "s-tel", "n": 1, "credit": "exact"},
		map[string]any{"ts": at(10), "event": "prompt", "session": "s-tel", "n": 2, "decision": "recall", "ids": []string{histA, histB}},
		map[string]any{"ts": at(11), "event": "tool_use", "session": "s-tel", "n": 2, "name": "mcp__plugin_backsight_memory__memory_search", "ids": 1, "memory_ids": []string{histC}},
		map[string]any{"ts": at(12), "event": "reply", "session": "s-tel", "n": 2, "credit": "free"},
	)
	search := "[billing]\n- PostgreSQL 16 stays (2026-01-01, 4d ago, " + histD + ", 0.81)\n"
	// s-tel's transcript holds the search telemetry recorded: counted once.
	writeHistoryTranscript(t, home, "s-tel",
		trRec("user", "v1", at(10), "the second prompt"),
		trRec("assistant", "b1", at(11), []any{map[string]any{"type": "tool_use", "id": "t9", "name": "mcp__plugin_backsight_memory__memory_search", "input": map[string]any{"query": "billing"}}}),
		trRec("user", "w1", at(11), []any{map[string]any{"type": "tool_result", "tool_use_id": "t9", "content": "[billing]\n- #1 a fact (2026-01-01, 4d ago, " + histC + ", 0.70)\n"}}),
	)
	old := func() []map[string]any {
		return []map[string]any{
			trRec("user", "u1", at(20), "how is billing stored?"),
			trRec("assistant", "a1", at(21), []any{map[string]any{"type": "tool_use", "id": "t1", "name": "mcp__plugin_backsight_memory__memory_search", "input": map[string]any{"query": "billing"}}}),
			trRec("user", "r1", at(22), []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": []any{map[string]any{"type": "text", "text": search}}}}),
			trRec("assistant", "a2", at(23), []any{map[string]any{"type": "text", "text": "> 🧠 **From memory (billing)** — it stays on PostgreSQL 16."}}),
		}
	}
	writeHistoryTranscript(t, home, "s-old", old()...)
	// A resumed session's transcript repeats the records it resumed.
	writeHistoryTranscript(t, home, "s-resumed", append(old(), trRec("user", "u9", at(30), "and then?"))...)
}

// fakeMemoryServer answers GET .../memories/<id> with status[id], 200
// for any other id.
func fakeMemoryServer(t *testing.T, home string, status map[string]int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := path.Base(r.URL.Path)
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/memories/") {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		if code, ok := status[id]; ok {
			http.Error(w, "status", code)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","text":"a fact","type":"world"}`))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pointAt(t, home, port)
}

type importEnvelope struct {
	OK    bool          `json:"ok"`
	Error *output.Error `json:"error"`
	Data  struct {
		Plan    bool `json:"plan"`
		Sources map[string]struct {
			Sessions int `json:"sessions"`
			Shows    int `json:"shows"`
			Credits  int `json:"credits"`
			Dropped  int `json:"ids_dropped"`
		} `json:"sources"`
		Already   int    `json:"already_imported"`
		Unchecked int    `json:"ids_unchecked"`
		Why       string `json:"unchecked_reason"`
		Events    int    `json:"events"`
		Complete  bool   `json:"complete"`
	} `json:"data"`
}

func runImport(t *testing.T, args ...string) importEnvelope {
	t.Helper()
	code, out, errOut := run(t, append([]string{"feedback", "import", "--json"}, args...)...)
	var env importEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK {
		t.Fatalf("exit %d: %v\n%s%s", code, err, out, errOut)
	}
	return env
}

// importEvents is the import events of home's journal with shows (not
// the marks of a complete import).
func importEvents(t *testing.T, home string) []feedback.Event {
	t.Helper()
	events, _, err := feedback.Read(pathsOf(home).FeedbackDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []feedback.Event
	for _, e := range events {
		if e.Kind == feedback.KindImport && e.Outcome != feedback.ImportComplete {
			out = append(out, e)
		}
	}
	return out
}

// --plan counts per source and writes nothing; --since narrows to the
// shows from that date on.
func TestImportPlanWritesNothing(t *testing.T) {
	home := devInstall(t, false)
	fakeMemoryServer(t, home, nil)
	history(t, home)
	before := filesUnder(t, filepath.Join(home, "backsight"))

	env := runImport(t, "--plan")
	tel, tr := env.Data.Sources["telemetry"], env.Data.Sources["transcripts"]
	if !env.Data.Plan || tel.Sessions != 1 || tel.Shows != 4 || tel.Credits != 1 || tr.Sessions != 1 || tr.Shows != 1 || tr.Credits != 0 || env.Data.Unchecked != 0 || env.Data.Complete {
		t.Errorf("plan %+v", env.Data)
	}
	if after := filesUnder(t, filepath.Join(home, "backsight")); len(after) != len(before) {
		t.Errorf("--plan wrote files: %d before, %d after", len(before), len(after))
	} else {
		for f, v := range before {
			if after[f] != v {
				t.Errorf("--plan changed %s", f)
			}
		}
	}
	code, out, _ := run(t, "feedback", "import", "--plan")
	if code != output.OK || !strings.Contains(out, "telemetry") || !strings.Contains(out, "4 shows") || !strings.Contains(out, "nothing written") {
		t.Errorf("text plan: exit %d\n%s", code, out)
	}

	since := runImport(t, "--plan", "--since", histStart.Add(24*time.Hour).Format("2006-01-02"))
	if n := since.Data.Sources["telemetry"].Shows + since.Data.Sources["transcripts"].Shows; n != 0 {
		t.Errorf("--since the next day: %d shows", n)
	}
	if code, _, _ := run(t, "feedback", "import", "--plan", "--since", "2026-01-05T10:00Z"); code != output.Usage {
		t.Errorf("--since with a time: exit %d, want usage", code)
	}
}

// Import adds the earlier shows and credits once, apart from the live
// counts; the shows the live journal holds are not imported again; a
// rebuild without imports gives the live ledger back.
func TestImportIsIdempotentAndSeparable(t *testing.T) {
	home := devInstall(t, false)
	fakeMemoryServer(t, home, nil)
	dir := feedbackJournal(t, home) // session s1-abcdef12, from logStart
	history(t, home)
	// The live session in telemetry too: its show before the journal
	// began is earlier history, the one after it is journaled already.
	writeTelemetry(t, home,
		map[string]any{"ts": logStart.Add(-time.Hour), "event": "prompt", "session": "s1-abcdef12", "n": 1, "ids": []string{logM2}},
		map[string]any{"ts": logStart.Add(time.Minute), "event": "prompt", "session": "s1-abcdef12", "n": 2, "ids": []string{logM1, logM2}},
	)
	ledgerBefore, _ := os.ReadFile(filepath.Join(dir, "ledger.json"))
	live, _ := feedback.LoadLedger(dir)

	first := runImport(t)
	if first.Data.Events == 0 || !first.Data.Complete || first.Data.Sources["telemetry"].Shows != 5 || first.Data.Sources["transcripts"].Shows != 1 {
		t.Fatalf("first run %+v", first.Data)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "ledger.json")); string(raw) != string(ledgerBefore) {
		t.Error("the import rewrote ledger.json")
	}
	evs := importEvents(t, home)
	keys := map[string]int{}
	for _, e := range evs {
		for _, s := range e.Settled {
			keys[s.Key]++
			if s.Outcome != feedback.OutcomeShown && s.Outcome != feedback.OutcomeCredit {
				t.Errorf("imported outcome %q", s.Outcome)
			}
		}
		if e.Source == "" || e.Session == "" || e.Params == nil {
			t.Errorf("import event %+v", e)
		}
	}
	if keys["s1-abcdef12|1|prompt|"+logM2] != 1 || keys["s1-abcdef12|2|prompt|"+logM1] != 0 || keys["s-old|r1|search|"+histD] != 1 {
		t.Errorf("keys %v", keys)
	}
	raw, _ := json.Marshal(evs)
	for _, text := range []string{"PostgreSQL", "billing stored", "From memory"} {
		if strings.Contains(string(raw), text) {
			t.Errorf("the journal holds text %q", text)
		}
	}

	cur, err := feedback.Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := cur.Memories[logM2]; m.Shown != live.Memories[logM2].Shown+1 || m.Imported.Shown != 1 || m.Misses != live.Memories[logM2].Misses {
		t.Errorf("m2 %+v, live %+v", m, live.Memories[logM2])
	}
	if a := cur.Memories[histA]; a.Shown != 2 || a.Credits != 1 || a.Imported.Credits != 1 || a.Misses != 0 {
		t.Errorf("A %+v", a)
	}
	// A transcript-only session: shows, never a credit.
	if d := cur.Memories[histD]; d.Shown != 1 || d.Credits != 0 || !d.LastShown.Equal(histStart.Add(22*time.Minute)) {
		t.Errorf("D %+v", d)
	}

	again := runImport(t)
	if again.Data.Events != 0 || again.Data.Already != 7 || len(importEvents(t, home)) != len(evs) {
		t.Errorf("second run %+v", again.Data)
	}

	if code, out, _ := run(t, "feedback", "rebuild", "--no-import", "--json"); code != output.OK {
		t.Fatalf("rebuild --no-import: exit %d\n%s", code, out)
	}
	all, _, _ := feedback.Read(dir, time.Time{})
	without, _ := feedback.LoadLedger(dir)
	p := feedbackParams(config.Default(pathsOf(home), home))
	want := feedback.Fold(feedback.DropImports(all), p)
	if len(without.Memories) != len(want.Memories) || len(without.Memories) != len(live.Memories) {
		t.Errorf("without imports %d memories, fold %d, live %d", len(without.Memories), len(want.Memories), len(live.Memories))
	}
	for id, m := range want.Memories {
		if w := without.Memories[id]; w.Shown != m.Shown || w.Credits != m.Credits || w.Misses != m.Misses || w.Imported != (feedback.Imported{}) || w.Shown != live.Memories[id].Shown {
			t.Errorf("%s: %+v, fold %+v, live %+v", id, w, m, live.Memories[id])
		}
	}
	if code, _, _ := run(t, "feedback", "rebuild", "--json"); code != output.OK {
		t.Fatal("rebuild")
	}
	if l, _ := feedback.LoadLedger(dir); l.Memories[histA].Imported.Shown != 2 {
		t.Errorf("a rebuild with imports: %+v", l.Memories[histA])
	}
}

// An id the server answers 404 for is dropped; one it fails on is kept
// and counted unchecked, as is every id while the server is unreachable.
func TestImportDropsUnknownIDs(t *testing.T) {
	home := devInstall(t, false)
	fakeMemoryServer(t, home, map[string]int{histB: http.StatusNotFound, histC: http.StatusInternalServerError})
	history(t, home)
	plan := runImport(t, "--plan")
	tel := plan.Data.Sources["telemetry"]
	if tel.Dropped != 1 || tel.Shows != 3 || plan.Data.Unchecked != 1 || !strings.Contains(plan.Data.Why, "500") {
		t.Errorf("plan %+v", plan.Data)
	}
	runImport(t)
	ids := map[string]bool{}
	for _, e := range importEvents(t, home) {
		for _, s := range e.Settled {
			ids[s.ID] = true
		}
	}
	if ids[histB] || !ids[histA] || !ids[histC] || !ids[histD] {
		t.Errorf("imported %v: B dropped, A, C and D kept", ids)
	}

	// The server down: nothing dropped, every id kept and said unchecked.
	other := devInstall(t, false)
	history(t, other)
	down := runImport(t, "--plan")
	if down.Data.Sources["telemetry"].Dropped != 0 || down.Data.Sources["telemetry"].Shows != 4 || down.Data.Unchecked != 4 {
		t.Errorf("server down %+v", down.Data)
	}
	if _, out, _ := run(t, "feedback", "import", "--plan"); !strings.Contains(out, "4 memory ids not checked") || !strings.Contains(out, "kept") {
		t.Errorf("text:\n%s", out)
	}

	// Another server answers at the joined address: it holds none of these
	// memories, so it is not asked and nothing is dropped.
	third := devInstall(t, false)
	fakeMemoryServer(t, third, map[string]int{histA: http.StatusNotFound, histB: http.StatusNotFound, histC: http.StatusNotFound, histD: http.StatusNotFound})
	recordMismatch(t, third)
	history(t, third)
	mm := runImport(t, "--plan")
	if mm.Data.Sources["telemetry"].Dropped != 0 || mm.Data.Sources["transcripts"].Dropped != 0 || mm.Data.Unchecked != 4 {
		t.Errorf("another server %+v", mm.Data)
	}
}

// The cheap pass over a transcript finds the word across two reads.
func TestContainsWordAcrossReads(t *testing.T) {
	word := []byte("memory_search")
	pad := strings.Repeat("x", 1<<16+len(word)-5)
	if !containsWord(strings.NewReader(pad+string(word)+"tail"), word) {
		t.Error("word across two reads not found")
	}
	if !containsWord(iotest.OneByteReader(strings.NewReader("ab"+string(word))), word) {
		t.Error("word read one byte at a time not found")
	}
	if containsWord(strings.NewReader(pad+"memory_searc"), word) {
		t.Error("found a word that is not there")
	}
}

// importMarks counts the marks of a complete import in home's journal.
func importMarks(t *testing.T, home string) int {
	t.Helper()
	events, _, _ := feedback.Read(pathsOf(home).FeedbackDir(), time.Time{})
	n := 0
	for _, e := range events {
		if e.Kind == feedback.KindImport && e.Outcome == feedback.ImportComplete {
			n++
		}
	}
	return n
}

func tickOf(t *testing.T) tickResult {
	t.Helper()
	_, out, errOut := run(t, "jobs", "tick", "--json")
	var env struct {
		Data tickResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %s %s", out, errOut)
	}
	return env.Data
}

// After the upgrade, the tick imports earlier history, marks it complete,
// and leaves it alone from then on.
func TestTickImportsEarlierHistoryOnce(t *testing.T) {
	home := devInstall(t, true) // the server down: ids kept, unchecked
	history(t, home)
	if r := tickOf(t); r.History != "earlier history imported: 5 shows and 1 credits" {
		t.Errorf("first tick: %q", r.History)
	}
	l, err := feedback.Current(pathsOf(home).FeedbackDir())
	if err != nil || l.HistoryImported.IsZero() || l.Memories[histA].Imported.Shown != 2 || importMarks(t, home) != 1 {
		t.Errorf("after the first tick: %v %+v, %d marks", err, l.Memories[histA], importMarks(t, home))
	}
	events := len(importEvents(t, home))
	if r := tickOf(t); r.History != "" || len(importEvents(t, home)) != events || importMarks(t, home) != 1 {
		t.Errorf("second tick: %q, %d events (was %d), %d marks", r.History, len(importEvents(t, home)), events, importMarks(t, home))
	}
	if _, out, _ := run(t, "jobs", "tick"); strings.Contains(out, "earlier history") {
		t.Errorf("a done import still reported: %s", out)
	}
}

// A tick that runs out of its budget keeps what it journaled and leaves
// no mark; the next tick imports the rest and marks it complete.
func TestTickImportCutByItsBudgetGoesOn(t *testing.T) {
	home := devInstall(t, true)
	history(t, home)
	oldBudget := importTickBudget
	importTickBudget = 300 * time.Millisecond
	units := 0
	afterImportUnit = func() {
		if units++; units == 1 {
			time.Sleep(400 * time.Millisecond) // past the budget, after the telemetry
		}
	}
	t.Cleanup(func() { importTickBudget, afterImportUnit = oldBudget, nil })
	if r := tickOf(t); r.History != "earlier history: 4 shows and 1 credits imported, the rest at the next tick" {
		t.Errorf("cut tick: %q", r.History)
	}
	if n := len(importEvents(t, home)); n != 1 || importMarks(t, home) != 0 {
		t.Fatalf("after the cut: %d events, %d marks", n, importMarks(t, home))
	}
	importTickBudget = oldBudget
	if r := tickOf(t); r.History != "earlier history imported: 1 shows and 0 credits" {
		t.Errorf("next tick: %q", r.History)
	}
	l, _ := feedback.Current(pathsOf(home).FeedbackDir())
	if importMarks(t, home) != 1 || l.Memories[histA].Imported.Shown != 2 || l.Memories[histD].Imported.Shown != 1 {
		t.Errorf("after the next tick: %d marks, A %+v, D %+v", importMarks(t, home), l.Memories[histA], l.Memories[histD])
	}
}

// Feedback off: the tick imports nothing, and the command refuses.
func TestImportNeedsFeedbackOn(t *testing.T) {
	home := devInstall(t, true)
	history(t, home)
	configure(t, home, "[feedback]\nenabled = false")
	if r := tickOf(t); r.History != "" {
		t.Errorf("tick with feedback off: %q", r.History)
	}
	code, out, _ := run(t, "feedback", "import", "--json")
	if code != output.NeedsUser || !strings.Contains(out, "feedback is off") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(pathsOf(home).FeedbackDir()); err == nil {
		t.Error("feedback off: the feedback directory was written")
	}
}

// One import runs at a time: a manual import beside the tick's would
// journal the same shows twice. While a run holds the import lock, a
// second does nothing: the command says so (exit 20), the tick says
// nothing, --plan (which writes nothing) still runs; the first goes on,
// and each key is journaled once.
func TestOneImportRunsAtATime(t *testing.T) {
	home := devInstall(t, true)
	history(t, home)
	d := pathsOf(home)
	lock, err := state.TryAcquire(filepath.Join(d.LocksDir(), "feedback-import.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "feedback", "import", "--json")
	if code != output.Blocked || !strings.Contains(out, "already running (jobs tick)") {
		t.Errorf("manual import beside a running one: exit %d\n%s", code, out)
	}
	if r := tickOf(t); r.History != "" {
		t.Errorf("tick beside a running import: %q", r.History)
	}
	if code, out, _ := run(t, "feedback", "import", "--plan"); code != output.OK {
		t.Errorf("--plan beside a running import: exit %d\n%s", code, out)
	}
	if n := len(importEvents(t, home)); n != 0 || importMarks(t, home) != 0 {
		t.Errorf("%d events, %d marks journaled beside a running import", n, importMarks(t, home))
	}
	_ = lock.Release()

	var second error
	tried := false
	afterImportUnit = func() {
		if !tried { // inside the first run, its telemetry journaled
			tried = true
			if _, second = importHistory(context.Background(), d, home, time.Time{}, false, "feedback import"); second == nil {
				second = errors.New("ran beside the first")
			}
		}
	}
	t.Cleanup(func() { afterImportUnit = nil })
	if r := tickOf(t); r.History != "earlier history imported: 5 shows and 1 credits" {
		t.Errorf("first run: %q", r.History)
	}
	if !errors.As(second, new(importRunning)) {
		t.Errorf("second run beside the first: %v", second)
	}
	keys := map[string]int{}
	for _, e := range importEvents(t, home) {
		for _, s := range e.Settled {
			keys[s.Key]++
		}
	}
	for k, n := range keys {
		if n != 1 {
			t.Errorf("%s journaled %d times", k, n)
		}
	}
	if len(keys) != 5 || importMarks(t, home) != 1 {
		t.Errorf("%d keys, %d marks: want 5 and 1", len(keys), importMarks(t, home))
	}
}

// A run from --since leaves the earlier history to the tick: it writes no
// mark, says so, and the next tick imports what it left.
func TestImportSinceLeavesTheRestToTheTick(t *testing.T) {
	home := devInstall(t, true)
	history(t, home)
	next := histStart.Add(24 * time.Hour).Format("2006-01-02")
	if env := runImport(t, "--since", next); env.Data.Complete || importMarks(t, home) != 0 {
		t.Errorf("--since marked the import complete: %+v, %d marks", env.Data, importMarks(t, home))
	}
	if code, out, _ := run(t, "feedback", "import", "--since", next); code != output.OK || !strings.Contains(out, "a run without --since imports the earlier ones") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
	if r := tickOf(t); r.History != "earlier history imported: 5 shows and 1 credits" || importMarks(t, home) != 1 {
		t.Errorf("tick after --since: %q, %d marks", r.History, importMarks(t, home))
	}
}

// Telemetry stamps are cut to the millisecond, and the prompt hook writes
// its telemetry just after its first journal line: a show stamped in the
// millisecond the session's journal began is live, one before it is not.
func TestLiveCutoffCoversTheTelemetryMillisecond(t *testing.T) {
	ms := time.Date(2026, 10, 29, 4, 16, 28, 154_000_000, time.UTC)
	k := journalKnown([]feedback.Event{{Kind: "turn", Session: "s", TS: ms.Add(800 * time.Microsecond)}})
	if !k.live(feedback.ImportShow{Session: "s", At: ms}) {
		t.Error("the first prompt's telemetry, stamped in the journal's first millisecond, is imported again")
	}
	if k.live(feedback.ImportShow{Session: "s", At: ms.Add(-time.Millisecond)}) {
		t.Error("a show the millisecond before the journal began counts as live")
	}
}
