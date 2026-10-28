package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	if _, err := feedback.Settle(dir, feedbackParams(config.Default(pathsOf(home), home)), logStart.Add(3*time.Hour)); err != nil {
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
