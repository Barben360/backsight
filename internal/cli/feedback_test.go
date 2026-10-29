package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

const (
	logM1 = "0b4c2a52-1111-4111-8111-111111111111"
	logM2 = "7f3e9d10-2222-4222-8222-222222222222"
)

var logStart = time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)

// feedbackJournal writes a session of two turns into home's journal and
// settles it 3 hours later: m1 shown first at rank 1 and credited, m2
// beside it at rank 2 (a confident miss), then m1 again at rank 3 in a
// turn that never ends (unknown).
func feedbackJournal(t *testing.T, home string) string {
	t.Helper()
	dir := pathsOf(home).FeedbackDir()
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1-abcdef12"})
	for _, e := range []feedback.Event{
		{Kind: "turn", Turn: "p1", N: 1, TS: logStart},
		{Kind: "shows", Turn: "p1", Path: "prompt", TS: logStart.Add(time.Minute), Items: []feedback.Item{{ID: logM1, Rank: 1}, {ID: logM2, Rank: 2}}},
		{Kind: "credit", Turn: "p1", Verdict: "used", IDs: []string{logM1}, TS: logStart.Add(2 * time.Minute)},
		{Kind: "stop", Turn: "p1", Outcome: "declared", StopCheck: "always", TS: logStart.Add(3 * time.Minute)},
		{Kind: "turn", Turn: "p2", N: 2, TS: logStart.Add(40 * time.Minute)},
		{Kind: "shows", Turn: "p2", Path: "search", TS: logStart.Add(41 * time.Minute), Items: []feedback.Item{{ID: logM1, Rank: 3}}},
	} {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := feedback.Settle(dir, feedbackParams(config.Default(pathsOf(home), home)), func() time.Time { return logStart.Add(3 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	return dir
}

// filesUnder is every file under dir with its size and modification time.
func filesUnder(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err == nil && !de.IsDir() {
			fi, _ := de.Info()
			files[p] = fi.ModTime().String() + " " + itoa(int(fi.Size()))
		}
		return err
	})
	return files
}

// lineIndex is the index of the first line of out holding every part.
func lineIndex(lines []string, parts ...string) int {
	for i, l := range lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return i
		}
	}
	return -1
}

func TestFeedbackLogShowsAMemorysTimeline(t *testing.T) {
	home := isolate(t)
	t.Setenv("BACKSIGHT_SESSION", "")
	dir := feedbackJournal(t, home)
	old := now
	now = func() time.Time { return logStart.Add(time.Minute).AddDate(1, 0, 0) } // one credit half-life after the credited show
	t.Cleanup(func() { now = old })
	before := filesUnder(t, filepath.Dir(dir))

	code, out, errOut := run(t, "feedback", "log", "--id", logM1[:8]) // a short id names the memory
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	order := []int{
		lineIndex(lines, "2026-01-10 09:01", "s1-abcde", "turn 1", "shows", "prompt", "#1"),
		lineIndex(lines, "2026-01-10 09:02", "turn 1", "credit", "used"),
		lineIndex(lines, "2026-01-10 09:41", "turn 2", "shows", "search", "#3"),
		lineIndex(lines, "2026-01-10 12:00", "turn 1", "settle", "credit"),
		lineIndex(lines, "2026-01-10 12:00", "turn 2", "settle", "unknown"),
		lineIndex(lines, "shown 2", "credits 0.50", "misses 0.00"),
	}
	for i, at := range order {
		if at < 0 || i > 0 && at <= order[i-1] {
			t.Fatalf("line %d missing or out of order (%v):\n%s", i, order, out)
		}
	}
	if order[len(order)-1] != len(lines)-1 {
		t.Errorf("the ledger entry is not the last line:\n%s", out)
	}
	if strings.Contains(out, logM2[:8]) || strings.Contains(out, "confident_miss") || strings.Contains(out, "stop") {
		t.Errorf("another memory's events in the timeline:\n%s", out)
	}

	// The session's timeline holds every event of the session.
	_, out, _ = run(t, "feedback", "log", "--session", "s1-abcdef12")
	if lineIndex(strings.Split(out, "\n"), "turn 1", "stop", "declared") < 0 || lineIndex(strings.Split(out, "\n"), "settle", logM2[:8], "confident_miss") < 0 {
		t.Errorf("session timeline:\n%s", out)
	}
	// --since drops the earlier events; the settle of an earlier show is
	// after it.
	_, out, _ = run(t, "feedback", "log", "--id", logM1, "--since", "2026-01-10T09:30Z")
	if lines := strings.Split(out, "\n"); lineIndex(lines, "turn 1", "shows") >= 0 || lineIndex(lines, "credit", "used") >= 0 || lineIndex(lines, "turn 2", "shows", "#3") < 0 {
		t.Errorf("--since:\n%s", out)
	}

	code, out, _ = run(t, "feedback", "log", "--id", logM1, "--json")
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			ID     string `json:"id"`
			Events []struct {
				Kind    string `json:"kind"`
				N       int    `json:"turn_number"`
				Outcome string `json:"outcome"`
			} `json:"events"`
			Entry *struct {
				Shown   int     `json:"shown"`
				Credits float64 `json:"credits"`
			} `json:"entry"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK || !env.OK {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	if env.Data.ID != logM1 || len(env.Data.Events) != 5 || env.Data.Events[0].N != 1 || env.Data.Events[3].Outcome != "credit" ||
		env.Data.Entry == nil || env.Data.Entry.Shown != 2 || env.Data.Entry.Credits < 0.4999 || env.Data.Entry.Credits > 0.5001 {
		t.Errorf("envelope:\n%s", out)
	}

	if after := filesUnder(t, filepath.Dir(dir)); !reflect.DeepEqual(before, after) {
		t.Errorf("feedback log wrote:\nbefore %v\nafter  %v", before, after)
	}
	if code, _, _ := run(t, "feedback", "log"); code != output.Usage {
		t.Errorf("no memory and no session: exit %d", code)
	}
}

func TestFeedbackRebuildPlanWritesNothing(t *testing.T) {
	home := isolate(t)
	dir := feedbackJournal(t, home)
	// A memory the ledger does not hold yet: a settle event it missed.
	j := feedback.NewJournal(dir, feedback.Common{})
	if err := j.Append(feedback.Event{Kind: "settle", TS: logStart.Add(4 * time.Hour), Settled: []feedback.Settled{{Key: "s2|p1|1|m9", ID: "m9", At: logStart.Add(3 * time.Hour), Outcome: "shown"}}}); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "ledger.json")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	_ = os.Chtimes(ledger, past, past)
	raw, _ := os.ReadFile(ledger)
	// --plan reads only: it runs while a tick holds the ledger lock.
	lock, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "feedback-ledger.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	code, out, errOut := run(t, "feedback", "rebuild", "--plan")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"memories", "3", "2", "shows", "4", "1 new, 0 dropped, 0 changed", "nothing written"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if fi, err := os.Stat(ledger); err != nil || !fi.ModTime().Equal(past) {
		t.Errorf("ledger touched: %v %v", fi.ModTime(), err)
	}
	if again, _ := os.ReadFile(ledger); string(again) != string(raw) {
		t.Error("ledger changed")
	}
	code, out, _ = run(t, "feedback", "rebuild", "--plan", "--json")
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Plan    bool `json:"plan"`
			Rebuilt struct {
				Memories int `json:"memories"`
				Shown    int `json:"shown"`
			} `json:"rebuilt"`
			Current struct {
				Memories int `json:"memories"`
			} `json:"current"`
			New int `json:"new"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK || !env.OK || !env.Data.Plan ||
		env.Data.Rebuilt.Memories != 3 || env.Data.Rebuilt.Shown != 4 || env.Data.Current.Memories != 2 || env.Data.New != 1 {
		t.Errorf("exit %d %v:\n%s", code, err, out)
	}
}

func TestFeedbackRebuildWritesTheFold(t *testing.T) {
	home := isolate(t)
	dir := feedbackJournal(t, home)
	ledger := filepath.Join(dir, "ledger.json")
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "feedback", "rebuild")
	if code != output.OK || !strings.Contains(out, "2 new") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	got, err := feedback.LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, _, _ := feedback.Read(dir, time.Time{})
	want := feedback.Fold(events, feedbackParams(config.Default(pathsOf(home), home)))
	// It records where the journal ended, for Current to read on from there.
	if fi, err := os.Stat(filepath.Join(dir, got.JournalAt.File)); err != nil || got.JournalAt.File == "" || fi.Size() != got.JournalAt.Offset {
		t.Errorf("journal_at %+v: %v", got.JournalAt, err)
	}
	want.JournalAt = got.JournalAt
	if !reflect.DeepEqual(got, want) || len(got.Memories) != 2 {
		t.Errorf("rebuilt %+v\nwant     %+v", got, want)
	}

	// A tick settling holds the ledger lock: the rebuild waits for it.
	lock, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "feedback-ledger.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	_ = os.Remove(ledger)
	if code, out, _ := run(t, "feedback", "rebuild", "--json"); code != output.Blocked || !strings.Contains(out, `"ok": false`) {
		t.Errorf("lock held: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(ledger); err == nil {
		t.Error("a blocked rebuild wrote the ledger")
	}
}

// --session takes the first 8 characters of a session id or more, when
// exactly one session of the journal starts with them.
func TestFeedbackLogTakesASessionPrefix(t *testing.T) {
	home := isolate(t)
	t.Setenv("BACKSIGHT_SESSION", "")
	dir := feedbackJournal(t, home)
	_, out, _ := run(t, "feedback", "log", "--session", "s1-abcde")
	if lineIndex(strings.Split(out, "\n"), "turn 1", "stop", "declared") < 0 {
		t.Errorf("session prefix:\n%s", out)
	}
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1-abcdef99"})
	if err := j.Append(feedback.Event{Kind: "turn", Turn: "q1", N: 1, TS: logStart}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run(t, "feedback", "log", "--session", "s1-abcde"); code != output.Usage || !strings.Contains(out+errOut, "s1-abcde starts several sessions") {
		t.Errorf("ambiguous prefix: exit %d\n%s%s", code, out, errOut)
	}
	if _, out, _ := run(t, "feedback", "log", "--session", "s1-abcdef1"); lineIndex(strings.Split(out, "\n"), "turn 1", "stop", "declared") < 0 {
		t.Errorf("a longer prefix:\n%s", out)
	}
	// under 8 characters: no prefix, a session of its own
	if _, out, _ := run(t, "feedback", "log", "--session", "s1-abcd"); strings.Contains(out, "declared") {
		t.Errorf("a 7-character prefix matched:\n%s", out)
	}
}

const (
	repM1 = "1a000000-0000-4000-8000-000000000001" // credited once, missed twice
	repM2 = "2b000000-0000-4000-8000-000000000002" // credited once
	repM3 = "3c000000-0000-4000-8000-000000000003" // missed once
	repB  = "4d000000-0000-4000-8000-000000000004" // never settled but in recalls
	repC  = "5e000000-0000-4000-8000-000000000005"
	repD  = "6f000000-0000-4000-8000-000000000006"
)

// replayJournal journals one session where each turn shows one memory at
// rank 1 (m3 at rank 2) and declares the turn, crediting it or not: m1
// credited once and missed twice, m2 credited, m3 and ten others missed
// once (15 prompt
// shows of one memory each, which no boost reorders); and a second
// session, never declared (its shows settle as shown only), whose
// recalls hold scores without the boost. Feedback replay runs a day later.
func replayJournal(t *testing.T, home string, recalls ...feedback.Event) string {
	t.Helper()
	dir := pathsOf(home).FeedbackDir()
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1-abcdef12"})
	turns := []struct {
		id   string
		used bool
		rank int
	}{{repM1, true, 1}, {repM1, false, 1}, {repM1, false, 1}, {repM2, true, 1}, {repM3, false, 2}}
	for i := range 10 {
		turns = append(turns, struct {
			id   string
			used bool
			rank int
		}{fmt.Sprintf("f%07d-0000-4000-8000-000000000000", i), false, 1})
	}
	for i, tu := range turns {
		m, turn := logStart.Add(time.Duration(i)*10*time.Minute), "p"+itoa(i+1)
		credit := feedback.Event{Kind: "credit", Turn: turn, Verdict: "none", TS: m.Add(2 * time.Minute)}
		if tu.used {
			credit.Verdict, credit.IDs = "used", []string{tu.id}
		}
		for _, e := range []feedback.Event{
			{Kind: "turn", Turn: turn, N: i + 1, TS: m},
			{Kind: "shows", Turn: turn, Path: "prompt", TS: m.Add(time.Minute), Items: []feedback.Item{{ID: tu.id, Rank: tu.rank, ScoreWithoutBoost: ptr(0.7)}}},
			credit,
			{Kind: "stop", Turn: turn, Outcome: "declared", StopCheck: "always", TS: m.Add(3 * time.Minute)},
		} {
			if err := j.Append(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	j2 := feedback.NewJournal(dir, feedback.Common{Session: "s2-abcdef12"})
	for _, e := range recalls {
		if err := j2.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	old := now
	now = func() time.Time { return logStart.AddDate(0, 0, 1) }
	t.Cleanup(func() { now = old })
	return dir
}

type replayEnvelope struct {
	OK   bool `json:"ok"`
	Data struct {
		Set            map[string]float64 `json:"set"`
		Memories       int                `json:"memories"`
		TurnedNegative []string           `json:"turned_negative"`
		TurnedPositive []string           `json:"turned_positive"`
		Movers         []struct {
			ID     string  `json:"id"`
			Before float64 `json:"before"`
			After  float64 `json:"after"`
		} `json:"movers"`
		Recalls struct {
			Compared      int `json:"compared"`
			SetChanged    int `json:"set_changed"`
			OrderChanged  int `json:"order_changed"`
			NotComparable int `json:"not_comparable"`
		} `json:"recalls"`
	} `json:"data"`
}

func replayJSON(t *testing.T, args ...string) replayEnvelope {
	t.Helper()
	code, out, errOut := run(t, append([]string{"feedback", "replay", "--json"}, args...)...)
	var env replayEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK || !env.OK {
		t.Fatalf("replay %v: exit %d, %v\n%s%s", args, code, err, out, errOut)
	}
	return env
}

// Against the current parameters, the current parameters change nothing;
// a confident rank of 1 drops m3's miss at rank 2; a smaller prior of misses (β) turns m1 (one credit, two misses)
// negative, and the movers come by how far their boost moved, at most 10.
func TestReplayCountsSignChangesAndMovers(t *testing.T) {
	home := isolate(t)
	replayJournal(t, home)
	for _, args := range [][]string{nil, {"--set", "prior_misses=9"}} {
		env := replayJSON(t, args...)
		if env.Data.Memories != 13 || len(env.Data.TurnedNegative)+len(env.Data.TurnedPositive)+len(env.Data.Movers) != 0 {
			t.Errorf("replay %v changed something: %+v", args, env.Data)
		}
	}

	// A show is re-decided: m3, shown at rank 2, is no longer a miss.
	if d := replayJSON(t, "--set", "confident_rank=1").Data; len(d.Movers) != 1 || d.Movers[0].ID != repM3 || d.Movers[0].Before >= 0 || d.Movers[0].After != 0 {
		t.Errorf("confident rank 1: %+v", d)
	}

	env := replayJSON(t, "--set", "prior_misses=1.5")
	d := env.Data
	if d.Set["prior_misses"] != 1.5 || strings.Join(d.TurnedNegative, ",") != repM1 || len(d.TurnedPositive) != 0 {
		t.Errorf("sign changes: %+v", d)
	}
	if len(d.Movers) != 10 || d.Movers[0].ID != repM1 || d.Movers[0].Before <= 0 || d.Movers[0].After >= 0 {
		t.Fatalf("movers: %+v", d.Movers)
	}
	if math.Abs(d.Movers[0].Before-0.0017) > 0.0001 || math.Abs(d.Movers[0].After+0.0133) > 0.0001 {
		t.Errorf("m1 moves %v → %v, want about +0.0017 → -0.0133", d.Movers[0].Before, d.Movers[0].After)
	}
	for i, m := range d.Movers {
		if m.ID == repM2 {
			t.Errorf("m2, which hardly moves, is among the 10 biggest: %+v", d.Movers)
		}
		if i > 0 && math.Abs(m.After-m.Before) > math.Abs(d.Movers[i-1].After-d.Movers[i-1].Before)+1e-12 {
			t.Errorf("movers out of order at %d: %+v", i, d.Movers)
		}
	}

	code, out, errOut := run(t, "feedback", "replay", "--set", "prior_misses=1.5")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lines := strings.Split(out, "\n")
	for _, want := range [][]string{{"Replay of this machine's journal", "prior_misses=1.5", "nothing written"}, {"13 memories", "1 turning negative", "0 turning positive"}, {repM1[:8], "+0.0017", "-0.013"}} {
		if lineIndex(lines, want...) < 0 {
			t.Errorf("no line with %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, repM1[8:]) {
		t.Errorf("a full id in the text:\n%s", out)
	}
}

// Each journaled recall, prompt and gate show is ordered again under both
// replayed ledgers: a larger weight (k) lifts m2 over b, which flips a
// recall's and a gate's order and brings m2 into a prompt's top 3
// (recall.prompt_hits). A recall without scores (an earlier version) is
// not comparable, one with a score of 0 is; a failure show is not
// counted.
func TestReplayCountsChangedRecalls(t *testing.T) {
	home := isolate(t)
	at := logStart.Add(6 * time.Hour)
	scored := func(pairs ...any) []feedback.Item {
		var items []feedback.Item
		for i := 0; i < len(pairs); i += 2 {
			items = append(items, feedback.Item{ID: pairs[i].(string), Rank: i/2 + 1, ScoreWithoutBoost: ptr(pairs[i+1].(float64))})
		}
		return items
	}
	replayJournal(t, home,
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: at, Items: scored(repB, 0.80, repM2, 0.785)},
		feedback.Event{Kind: "shows", Path: "prompt", TS: at, Items: scored(repB, 0.80, repC, 0.798, repD, 0.796, repM2, 0.785)},
		feedback.Event{Kind: "shows", Path: "gate", TS: at, Items: scored(repB, 0.80, repM2, 0.785)},
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: at, Items: []feedback.Item{{ID: repB, Rank: 1, Reranker: 0.8}, {ID: repM2, Rank: 2, Reranker: 0.785}}},
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: at, Items: scored(repM2, 0.90, repB, 0.70)},
		feedback.Event{Kind: "shows", Path: "failure", TS: at, Items: scored(repB, 0.80, repM2, 0.785)},
		feedback.Event{Kind: feedback.KindRecall, Path: "search", TS: at, Items: scored(repM2, 0.01, repC, 0.0)}, // a score of 0 is a score
	)
	if r := replayJSON(t).Data.Recalls; r.Compared != 20 || r.SetChanged != 0 || r.OrderChanged != 0 || r.NotComparable != 1 {
		t.Errorf("current parameters: %+v", r)
	}
	if r := replayJSON(t, "--set", "boost_weight=0.5").Data.Recalls; r.Compared != 20 || r.SetChanged != 1 || r.OrderChanged != 2 || r.NotComparable != 1 {
		t.Errorf("k 0.5: %+v, want 20 compared, 1 set changed (the prompt), 2 orders (the recall and the gate), 1 not comparable", r)
	}
	// A weight between: m2 (+0.0086) passes neither b nor d.
	if r := replayJSON(t, "--set", "boost_weight=0.15").Data.Recalls; r.SetChanged != 0 || r.OrderChanged != 0 {
		t.Errorf("k 0.15: %+v", r)
	}
	_, out, _ := run(t, "feedback", "replay", "--set", "boost_weight=0.5")
	if lineIndex(strings.Split(out, "\n"), "20 compared", "1 would change", "2 their order", "1 journaled by an earlier version") < 0 {
		t.Errorf("recall line:\n%s", out)
	}
}

// --set takes the replay's keys only, each a value config.toml accepts:
// anything else is a usage error that says why.
func TestReplayRefusesUnknownKeys(t *testing.T) {
	home := isolate(t)
	replayJournal(t, home)
	for _, c := range []struct{ set, why string }{
		{"stop_check=off", "stop_check"},
		{"boost=0.2", "boost"},
		{"boost_weight", "key=value"},
		{"boost_weight=much", "much"},
		{"boost_weight=NaN", "NaN"},
		{"confident_rank=1.5", "1.5"},
		{"boost_weight=-0.1", "feedback.boost_weight"},
		{"prior_credits=0", "feedback.prior_credits"},
		{"confident_rank=0", "feedback.confident_rank"},
		{"miss_half_life_days=-1", "feedback.miss_half_life_days"},
	} {
		code, out, errOut := run(t, "feedback", "replay", "--set", c.set)
		if code != output.Usage || !strings.Contains(out+errOut, c.why) {
			t.Errorf("--set %s: exit %d, want %d naming %q\n%s%s", c.set, code, output.Usage, c.why, out, errOut)
		}
	}
	for _, set := range []string{"boost_weight=0", "prior_credits=1", "confident_rank=3", "miss_half_life_days=0", "credit_half_life_days=30"} {
		if code, out, errOut := run(t, "feedback", "replay", "--set", set); code != output.OK {
			t.Errorf("--set %s: exit %d\n%s%s", set, code, out, errOut)
		}
	}
	// A config.toml refused on a key --set does not name is the config's
	// error, as for any command: not a --set one.
	_ = os.MkdirAll(filepath.Dir(pathsOf(home).ConfigFile()), 0o700)
	if err := os.WriteFile(pathsOf(home).ConfigFile(), []byte("[feedback]\nstale_shows = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "feedback", "replay", "--set", "boost_weight=0.2")
	if code != output.Blocked || !strings.Contains(out+errOut, "feedback.stale_shows") || strings.Contains(out+errOut, "--set") {
		t.Errorf("a broken config.toml: exit %d, want %d without --set\n%s%s", code, output.Blocked, out, errOut)
	}
}

func ptr(v float64) *float64 { return &v }

// Replay reads only: no file changes, and it runs while a tick holds the
// ledger lock.
func TestReplayIsReadOnly(t *testing.T) {
	home := isolate(t)
	dir := replayJournal(t, home)
	if _, err := feedback.Settle(dir, feedbackParams(config.Default(pathsOf(home), home)), func() time.Time { return logStart.Add(3 * time.Hour) }); err != nil {
		t.Fatal(err)
	}
	lock, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "feedback-ledger.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	before := filesUnder(t, filepath.Dir(dir))
	if env := replayJSON(t, "--set", "confident_rank=1", "--set", "credit_half_life_days=7"); len(env.Data.Movers) == 0 {
		t.Errorf("a 7-day credit half-life moved nothing: %+v", env.Data)
	}
	if code, out, errOut := run(t, "feedback", "replay"); code != output.OK {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if after := filesUnder(t, filepath.Dir(dir)); !reflect.DeepEqual(before, after) {
		t.Errorf("feedback replay wrote:\nbefore %v\nafter  %v", before, after)
	}
}

// With the boost off, a memory whose boost was negative moves to 0, not
// to a negative zero: neither the text nor the JSON prints "-0".
func TestReplayPrintsNoNegativeZero(t *testing.T) {
	home := isolate(t)
	replayJournal(t, home)
	_, out, _ := run(t, "feedback", "replay", "--set", "boost_weight=0")
	if !strings.Contains(out, "+0.0000") || strings.Contains(out, "-0.0000") {
		t.Errorf("text with a negative zero, or no move to 0:\n%s", out)
	}
	_, raw, _ := run(t, "feedback", "replay", "--set", "boost_weight=0", "--json")
	if !regexp.MustCompile(`"after":\s*0\s*[,}]`).MatchString(raw) || regexp.MustCompile(`:\s*-0\s*[,}]`).MatchString(raw) {
		t.Errorf("JSON with a negative zero, or no move to 0:\n%s", raw)
	}
}

// An entry holding only a restore time (a restore of a memory this
// machine never counted) is not a memory: rebuild's totals, its new and
// dropped counts and replay's count leave it out.
func TestARestoreAloneIsNotAMemory(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	p := feedback.Params{CreditHalfLife: 30 * 24 * time.Hour, MissHalfLife: 30 * 24 * time.Hour, PriorCredits: 1, PriorMisses: 9, Weight: 0.1}
	shown := feedback.Entry{Shown: 2, LastShown: at}
	withRestore := feedback.Ledger{Memories: map[string]feedback.Entry{"m1": shown, "m2": {UnhiddenAt: at}}}
	without := feedback.Ledger{Memories: map[string]feedback.Entry{"m1": shown}}
	if tot := totals(withRestore, p, at); tot.Memories != 1 || tot.Shown != 2 {
		t.Errorf("totals %+v", tot)
	}
	if res := compareLedgers(withRestore, without, p, at); res.New != 0 || res.Dropped != 0 || res.Rebuilt.Memories != 1 {
		t.Errorf("rebuilt holds the restore: %+v", res)
	}
	if res := compareLedgers(without, withRestore, p, at); res.New != 0 || res.Dropped != 0 || res.Current.Memories != 1 {
		t.Errorf("current holds the restore: %+v", res)
	}
	if res := compareReplays(nil, withRestore, withRestore, p, p, at, 3); res.Memories != 1 {
		t.Errorf("replay counts %d memories", res.Memories)
	}
}
