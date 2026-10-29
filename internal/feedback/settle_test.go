package feedback

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

var (
	t0         = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	testParams = Params{ConfidentRank: 2, MissHalfLife: 90 * 24 * time.Hour, CreditHalfLife: 365 * 24 * time.Hour, TurnTimeout: TurnTimeout}
)

// at is t0 plus m minutes.
func at(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

func evTurn(sess, turn string, m int) Event {
	return Event{Kind: "turn", Session: sess, Turn: turn, TS: at(m)}
}

func evShows(sess, turn, agent, path string, m int, items ...Item) Event {
	return Event{Kind: "shows", Session: sess, Turn: turn, Agent: agent, Path: path, TS: at(m), Items: items}
}

func evCredit(sess, turn, agent, verdict string, m int, ids ...string) Event {
	return Event{Kind: "credit", Session: sess, Turn: turn, Agent: agent, Verdict: verdict, TS: at(m), IDs: ids}
}

func evStop(sess, turn, agent, outcome string, m int) Event {
	return Event{Kind: "stop", Session: sess, Turn: turn, Agent: agent, Outcome: outcome, TS: at(m)}
}

// key is the settle key of id shown in sess's turn at minute m.
func key(sess, turn string, m int, id string) string {
	return sess + "|" + turn + "|" + strconv.FormatInt(at(m).UnixNano(), 10) + "|" + id
}

func outcomes(settled []Settled) map[string]string {
	out := map[string]string{}
	for _, s := range settled {
		out[s.Key] = s.Outcome
	}
	return out
}

// A credit goes to the latest show of its id not yet credited: the
// earlier show is decided by its own turn.
func TestCreditAttachesToTheLatestUncreditedShow(t *testing.T) {
	events := []Event{
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "search", 1, Item{ID: "m1", Rank: 1}),
		evCredit("s1", "p1", "", "none", 2),
		evTurn("s1", "p2", 10),
		evTurn("s1", "p3", 20),
		evShows("s1", "p3", "", "search", 21, Item{ID: "m1", Rank: 1}),
		evCredit("s1", "p3", "", "used", 22, "m1"),
		evTurn("s1", "p4", 30),
	}
	got, late := Decide(events, testParams, at(120))
	want := map[string]string{key("s1", "p1", 1, "m1"): "confident_miss", key("s1", "p3", 21, "m1"): "credit"}
	if o := outcomes(got); len(got) != 2 || o[key("s1", "p1", 1, "m1")] != want[key("s1", "p1", 1, "m1")] || o[key("s1", "p3", 21, "m1")] != "credit" || late != 0 {
		t.Errorf("got %+v late %d, want %v", got, late, want)
	}
}

// Each show earns at most one credit: a second credit with no uncredited
// show left is late and counts nowhere.
func TestCreditsNeverExceedShows(t *testing.T) {
	events := []Event{
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "m1", Rank: 1}),
		evCredit("s1", "p1", "", "used", 2, "m1"),
		evTurn("s1", "p2", 10),
		evCredit("s1", "p2", "", "used", 11, "m1"),
		evCredit("s2", "q1", "", "used", 12, "m1"), // another session's credit never reaches s1's shows
		evTurn("s1", "p3", 20),
	}
	got, late := Decide(events, testParams, at(120))
	if len(got) != 1 || got[0].Outcome != "credit" || got[0].ID != "m1" || !got[0].At.Equal(at(1)) || late != 2 {
		t.Errorf("got %+v late %d, want one credit and 2 late", got, late)
	}
}

func TestMissesNeedADeclaredTurnAndATopRank(t *testing.T) {
	events := []Event{
		// Declared first turn: only a top-ranked prompt, search or gate
		// show misses; the briefing before it belongs to it.
		evShows("s1", "", "", "briefing", -1, Item{ID: "brief", Rank: 1}),
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "top", Rank: 1}, Item{ID: "low", Rank: 3}),
		evShows("s1", "p1", "", "read", 2, Item{ID: "read"}),
		evShows("s1", "p1", "", "failure", 2, Item{ID: "fail", Rank: 1}),
		evShows("s1", "p1", "", "gate", 3, Item{ID: "gate", Rank: 2}),
		evShows("s1", "p1", "", "search", 3, Item{ID: "search", Rank: 1}),
		evCredit("s1", "p1", "", "none", 4),
		evStop("s1", "p1", "", "declared", 5),
		// Undeclared, blocked then undeclared, interrupted: unknown.
		evTurn("s1", "p2", 10),
		evShows("s1", "p2", "", "prompt", 11, Item{ID: "undeclared", Rank: 1}),
		evStop("s1", "p2", "", "undeclared", 12),
		evTurn("s1", "p3", 20),
		evShows("s1", "p3", "", "search", 21, Item{ID: "blocked", Rank: 1}),
		evStop("s1", "p3", "", "blocked", 22),
		evStop("s1", "p3", "", "undeclared", 23),
		evTurn("s1", "p4", 30),
		evShows("s1", "p4", "", "gate", 31, Item{ID: "interrupted", Rank: 1}),
		evTurn("s1", "p5", 40),
		// A subagent's declaration does not declare the main agent's turn.
		evShows("s1", "p5", "", "prompt", 41, Item{ID: "main", Rank: 1}),
		evShows("s1", "p5", "sub1", "search", 41, Item{ID: "sub", Rank: 1}),
		evCredit("s1", "p5", "sub1", "none", 42),
		evTurn("s1", "p6", 50),
		// A credit after its show settled: no credit, and it is late.
		evTurn("s2", "q1", 0),
		evShows("s2", "q1", "", "prompt", 1, Item{ID: "settled", Rank: 1}),
		evCredit("s2", "q1", "", "none", 2),
		evTurn("s2", "q2", 10),
		{Kind: "settle", TS: at(60), Settled: []Settled{{Key: key("s2", "q1", 1, "settled"), ID: "settled", At: at(1), Outcome: "confident_miss"}}},
		evCredit("s2", "q2", "", "used", 70, "settled"),
	}
	got, late := Decide(events, testParams, at(200))
	want := map[string]string{
		key("s1", "", -1, "brief"):         "shown",
		key("s1", "p1", 1, "top"):          "confident_miss",
		key("s1", "p1", 1, "low"):          "shown",
		key("s1", "p1", 2, "read"):         "shown",
		key("s1", "p1", 2, "fail"):         "shown",
		key("s1", "p1", 3, "gate"):         "confident_miss",
		key("s1", "p1", 3, "search"):       "confident_miss",
		key("s1", "p2", 11, "undeclared"):  "unknown",
		key("s1", "p3", 21, "blocked"):     "unknown",
		key("s1", "p4", 31, "interrupted"): "unknown",
		key("s1", "p5", 41, "main"):        "unknown",
		key("s1", "p5", 41, "sub"):         "confident_miss",
	}
	o := outcomes(got)
	if len(got) != len(want) || late != 1 {
		t.Errorf("got %d decisions, late %d; want %d and 1: %+v", len(got), late, len(want), got)
	}
	for k, w := range want {
		if o[k] != w {
			t.Errorf("%s: got %q, want %q", k, o[k], w)
		}
	}
}

func TestOpenTurnWaits(t *testing.T) {
	events := []Event{
		evTurn("over", "p1", 0),
		evShows("over", "p1", "", "prompt", 1, Item{ID: "m1", Rank: 1}),
		evTurn("over", "p2", 5),
		evTurn("open", "q1", 0),
		evShows("open", "q1", "", "prompt", 1, Item{ID: "m1", Rank: 1}),
		evTurn("stopped", "r1", 0),
		evShows("stopped", "r1", "", "prompt", 1, Item{ID: "m1", Rank: 1}),
		evStop("stopped", "r1", "", "undeclared", 2),
		// A subagent's stop does not end the main agent's turn.
		evTurn("sub", "u1", 0),
		evShows("sub", "u1", "", "prompt", 1, Item{ID: "m1", Rank: 1}),
		evStop("sub", "u1", "a7", "declared", 2),
	}
	sessions := func(m int) map[string]bool {
		got, _ := Decide(events, testParams, at(m))
		out := map[string]bool{}
		for _, s := range got {
			out[strings.SplitN(s.Key, "|", 2)[0]] = true
		}
		return out
	}
	if got := sessions(11); len(got) != 0 {
		t.Errorf("10 min old: decided %v", got)
	}
	if got := sessions(32); len(got) != 2 || !got["over"] || !got["stopped"] {
		t.Errorf("31 min old: decided %v, want over and stopped", got)
	}
	if got := sessions(121); len(got) != 4 {
		t.Errorf("2 h old: decided %v, want all four", got)
	}
}

// writeJournal appends events to dir's journal, each in the month file of
// its own ts.
func writeJournal(t *testing.T, dir string, events ...Event) {
	t.Helper()
	for _, e := range events {
		j := NewJournal(dir, Common{})
		j.now = func() time.Time { return e.TS }
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// settleKeys counts how often each show key appears in the journal's
// settle events.
func settleKeys(t *testing.T, dir string) map[string]int {
	t.Helper()
	events, _, err := Read(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, e := range events {
		for _, s := range e.Settled {
			n[s.Key]++
		}
	}
	return n
}

func sampleSession() []Event {
	return []Event{
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "m1", Rank: 1}, Item{ID: "m2", Rank: 2}),
		evCredit("s1", "p1", "", "used", 2, "m1"),
		evTurn("s1", "p2", 10),
		evShows("s1", "p2", "", "search", 11, Item{ID: "m3", Rank: 4}),
		evStop("s1", "p2", "", "undeclared", 12),
	}
}

func TestSettleIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	now := at(120)
	first, err := Settle(dir, testParams, now)
	if err != nil || first.Shows != 3 || first.Credits != 1 || first.Misses != 1 || first.Unknown != 1 {
		t.Fatalf("first settle: %+v %v", first, err)
	}
	l1, err := LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Settle(dir, testParams, now.Add(15*time.Minute))
	if err != nil || second != (SettleResult{}) {
		t.Errorf("second settle: %+v %v", second, err)
	}
	l2, _ := LoadLedger(dir)
	assertSameEntries(t, l1, l2, 0)
	for k, n := range settleKeys(t, dir) {
		if n != 1 {
			t.Errorf("%s settled %d times", k, n)
		}
	}
	if e := l2.Memories["m1"]; e.Shown != 1 || e.Credits != 1 || !e.LastCredited.Equal(at(1)) || e.Misses != 0 {
		t.Errorf("m1: %+v", e)
	}
	if e := l2.Memories["m2"]; e.Shown != 1 || e.Misses != 1 || e.Credits != 0 {
		t.Errorf("m2: %+v", e)
	}
	if !l2.Watermark.Equal(now.Add(15*time.Minute)) || l2.Schema != LedgerSchema {
		t.Errorf("watermark %v schema %d", l2.Watermark, l2.Schema)
	}

	// Two settles at once: one holds the lock, the other changes nothing.
	dir = filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var held *state.HeldError
			if _, err := Settle(dir, testParams, now); err != nil && !errors.As(err, &held) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := Settle(dir, testParams, now); err != nil {
		t.Fatal(err)
	}
	keys := settleKeys(t, dir)
	for k, n := range keys {
		if n != 1 {
			t.Errorf("concurrent: %s settled %d times", k, n)
		}
	}
	l3, _ := LoadLedger(dir)
	if len(keys) != 3 {
		t.Errorf("concurrent: %d keys settled, want 3", len(keys))
	}
	assertSameEntries(t, l1, l3, 0)
}

// A session resumed after its turns settled: its new shows settle, the old
// ones never again, and a credit goes to the new show of its id.
func TestResumedSessionSettlesOnlyNewShows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir,
		evShows("s1", "", "", "briefing", -1, Item{ID: "m1", Rank: 1}),
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "m1", Rank: 1}),
		evCredit("s1", "p1", "", "none", 2),
		evStop("s1", "p1", "", "declared", 3),
	)
	if res, err := Settle(dir, testParams, at(180)); err != nil || res.Shows != 2 || res.Misses != 1 {
		t.Fatalf("first settle: %+v %v", res, err)
	}
	// Resumed a day later: a new briefing, belonging to the first turn
	// after it, and a credit for m1.
	writeJournal(t, dir,
		evShows("s1", "", "", "briefing", 1440, Item{ID: "m1", Rank: 1}),
		evTurn("s1", "p9", 1441),
		evShows("s1", "p9", "", "prompt", 1442, Item{ID: "m1", Rank: 1}),
		evCredit("s1", "p9", "", "used", 1443, "m1"),
		evStop("s1", "p9", "", "declared", 1444),
	)
	res, err := Settle(dir, testParams, at(1440+180))
	if err != nil || res.Shows != 2 || res.Credits != 1 || res.Misses != 0 || res.Unknown != 0 || res.Late != 0 {
		t.Fatalf("after resume: %+v %v", res, err)
	}
	keys := settleKeys(t, dir)
	if len(keys) != 4 {
		t.Errorf("%d keys settled, want 4: %v", len(keys), keys)
	}
	for k, n := range keys {
		if n != 1 {
			t.Errorf("%s settled %d times", k, n)
		}
	}
	l, _ := LoadLedger(dir)
	if e := l.Memories["m1"]; e.Shown != 4 || e.Credits != 1 || e.Misses != 1 || !e.LastCredited.Equal(at(1442)) {
		t.Errorf("m1: %+v", e)
	}
}

func TestLoadLedgerRefusesAnUnknownSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	_ = os.MkdirAll(dir, 0o700)
	if l, err := LoadLedger(dir); err != nil || len(l.Memories) != 0 {
		t.Errorf("missing ledger: %+v %v", l, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(`{"schema":99,"memories":{"m1":{"shown":3}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := LoadLedger(dir); err == nil || len(l.Memories) != 0 {
		t.Errorf("unknown schema: %+v %v", l, err)
	}
	// Settle leaves a newer binary's ledger alone and journals nothing.
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, at(120)); err == nil {
		t.Error("settle over an unknown ledger schema succeeded")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "ledger.json")); string(raw) != `{"schema":99,"memories":{"m1":{"shown":3}}}` {
		t.Errorf("ledger rewritten: %s", raw)
	}
	if len(settleKeys(t, dir)) != 0 {
		t.Error("a settle was journaled")
	}
}

// A memory forgotten after it was shown and before its show settled: the
// show is settled as forgotten, counted nowhere, never decided again, and
// a rebuild agrees. A show after the forget counts.
func TestForgetBeforeSettleKeepsTheEntryGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir,
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "m1", Rank: 1}, Item{ID: "m2", Rank: 2}),
		evCredit("s1", "p1", "", "used", 2, "m1"),
		Event{Kind: "forget", TS: at(5), IDs: []string{"m1"}},
		evStop("s1", "p1", "", "declared", 6),
	)
	res, err := Settle(dir, testParams, at(120))
	if err != nil || res.Shows != 1 || res.Credits != 0 || res.Misses != 1 {
		t.Fatalf("settle: %+v %v", res, err)
	}
	live, _ := LoadLedger(dir)
	if _, ok := live.Memories["m1"]; ok || live.Memories["m2"].Shown != 1 {
		t.Errorf("live after a forget before settle: %+v", live.Memories)
	}
	if again, err := Settle(dir, testParams, at(240)); err != nil || again != (SettleResult{}) {
		t.Errorf("second settle: %+v %v", again, err)
	}
	if k := settleKeys(t, dir); k[key("s1", "p1", 1, "m1")] != 1 {
		t.Errorf("the forgotten show is not marked settled once: %v", k)
	}
	rebuilt, _ := Rebuild(dir, testParams, false, true)
	assertSameEntries(t, live, rebuilt, 0)

	// Shown again after the forget: that show counts.
	writeJournal(t, dir,
		evTurn("s1", "p2", 300),
		evShows("s1", "p2", "", "search", 301, Item{ID: "m1", Rank: 3}),
		evStop("s1", "p2", "", "undeclared", 302),
	)
	if res, err := Settle(dir, testParams, at(420)); err != nil || res.Shows != 1 {
		t.Fatalf("settle after the new show: %+v %v", res, err)
	}
	live, _ = LoadLedger(dir)
	if live.Memories["m1"].Shown != 1 {
		t.Errorf("m1 shown after its forget: %+v", live.Memories["m1"])
	}
	rebuilt, _ = Rebuild(dir, testParams, false, true)
	assertSameEntries(t, live, rebuilt, 0)
}

// The facts a Sources: answer listed are journaled as shows only so that
// the next verdict can name them: never settled, they add no show and no
// miss, and the live ledger still equals a rebuild.
func TestSourcesShowsAreNeverSettled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir,
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "obs", Rank: 1}),
		evShows("s1", "p1", "", PathSources, 2, Item{ID: "src1"}, Item{ID: "src2"}),
		evCredit("s1", "p1", "", "outdated", 3, "src1"),
		evStop("s1", "p1", "", "declared", 4),
	)
	res, err := Settle(dir, testParams, at(180))
	if err != nil {
		t.Fatal(err)
	}
	if res.Shows != 1 || res.Misses != 1 {
		t.Errorf("settled %+v, want the prompt show alone (a miss)", res)
	}
	live, err := LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"src1", "src2"} {
		if m, ok := live.Memories[id]; ok {
			t.Errorf("%s, only listed as a source, has an entry: %+v", id, m)
		}
	}
	if m := live.Memories["obs"]; m.Shown != 1 {
		t.Errorf("obs %+v", m)
	}
	rebuilt, err := Rebuild(dir, testParams, false, true)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, live, rebuilt, 0)
}
