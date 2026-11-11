package ledgerdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
)

// settleStart is when the tests' sessions begin; settleAt, three hours
// later, is when they settle.
var (
	settleStart = nov(100)
	settleAt    = settleStart.Add(3 * time.Hour)
)

// settleSessions journals two sessions: in s1, m1 (rank 1) and m2 (rank
// 2) shown in a declared turn, m1 credited (a credit, a confident miss);
// in s2, m3 shown in a turn never declared (unknown). Settled at
// settleAt: three shows, one credit, one miss, one unknown.
func settleSessions(t *testing.T, o Options) {
	t.Helper()
	appendLines(t, o,
		feedback.Event{Kind: "turn", TS: settleStart, Turn: "p1", N: 1},
		feedback.Event{Kind: "shows", TS: settleStart.Add(time.Minute), Turn: "p1", Path: "prompt", Items: []feedback.Item{{ID: "m1", Rank: 1}, {ID: "m2", Rank: 2}}},
		feedback.Event{Kind: "credit", TS: settleStart.Add(2 * time.Minute), Turn: "p1", Verdict: "used", IDs: []string{"m1"}},
		feedback.Event{Kind: "stop", TS: settleStart.Add(3 * time.Minute), Turn: "p1", Outcome: "declared"},
		feedback.Event{Kind: "turn", TS: settleStart.Add(5 * time.Minute), Session: "s2", Turn: "q1", N: 1},
		feedback.Event{Kind: "shows", TS: settleStart.Add(6 * time.Minute), Session: "s2", Turn: "q1", Path: "search", Items: []feedback.Item{{ID: "m3", Rank: 1}}},
	)
}

// settledOnce is what settleSessions gives, each show counted once.
var settledOnce = feedback.SettleResult{Shows: 3, Credits: 1, Misses: 1, Unknown: 1}

// countedOnce: o's ledger.db holds each show of settleSessions once,
// by hand: m1 shown and credited, m2 shown and missed, m3 shown.
func countedOnce(t *testing.T, what string, o Options) {
	t.Helper()
	l := folded(t, o)
	for id, want := range map[string]struct {
		shown           int
		credits, misses float64
	}{"m1": {1, 1, 0}, "m2": {1, 0, 1}, "m3": {1, 0, 0}} {
		e := l.Memories[id]
		if e.Shown != want.shown || e.Credits != want.credits || e.Misses != want.misses {
			t.Errorf("%s: %s shown %d credits %v misses %v, want %d %v %v", what, id, e.Shown, e.Credits, e.Misses, want.shown, want.credits, want.misses)
		}
	}
}

// settleEvents are the settle lines of o's journal, in file order.
func settleEvents(t *testing.T, o Options) []feedback.Event {
	t.Helper()
	events, _, _, err := feedback.ReadAll(feedbackDir(o))
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(events, func(e feedback.Event) bool { return e.Kind != "settle" })
}

func keysOf(e feedback.Event) []string {
	keys := make([]string, len(e.Settled))
	for i, s := range e.Settled {
		keys[i] = s.Key
	}
	slices.Sort(keys)
	return keys
}

// twoSettlesShareTheirKeys: the journal holds two settle lines deciding
// the same shows, the proof two deciders raced.
func twoSettlesShareTheirKeys(t *testing.T, o Options) {
	t.Helper()
	settles := settleEvents(t, o)
	if len(settles) != 2 {
		t.Fatalf("%d settle lines, want 2 (both deciders journaled)", len(settles))
	}
	a, b := keysOf(settles[0]), keysOf(settles[1])
	t.Logf("settle lines at %v (seq %d) and %v (seq %d), keys %v and %v", settles[0].TS, settles[0].Seq, settles[1].TS, settles[1].Seq, a, b)
	if len(a) != 3 || !slices.Equal(a, b) {
		t.Errorf("the two settles hold keys %v and %v, want the same three", a, b)
	}
}

// exportMatches: ledger.json holds what ledger.db folded.
func exportMatches(t *testing.T, what string, o Options) {
	t.Helper()
	sameLedger(t, what+": ledger.json", loadExport(t, o), folded(t, o))
}

func at(ts time.Time) func() time.Time { return func() time.Time { return ts } }

func mustSettle(t *testing.T, o Options, now time.Time) feedback.SettleResult {
	t.Helper()
	res, err := Settle(context.Background(), o, testParams, at(now))
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	return res
}

func TestSettleDecidesFoldsAndExports(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	if res := mustSettle(t, o, settleAt); res != settledOnce {
		t.Errorf("Settle: %+v, want %+v", res, settledOnce)
	}
	settles := settleEvents(t, o)
	if len(settles) != 1 || !settles[0].TS.Equal(settleAt) || settles[0].Binary != "v-test" || settles[0].Params[missHalfLifeDays] != 30 {
		t.Fatalf("settle lines %+v, want one at %v carrying the binary and the params", settles, settleAt)
	}
	countedOnce(t, "after the settle", o)
	matchesOracle(t, "after the settle", o, testParams)
	exportMatches(t, "after the settle", o)

	if res := mustSettle(t, o, settleAt.Add(time.Hour)); res != (feedback.SettleResult{}) {
		t.Errorf("a second settle: %+v, want nothing", res)
	}
	if n := len(settleEvents(t, o)); n != 1 {
		t.Errorf("%d settle lines after a settle with nothing to decide, want 1", n)
	}
	countedOnce(t, "after the second settle", o)
}

// missHalfLifeDays is the key of the miss half-life in a settle's params.
const missHalfLifeDays = "miss_half_life_days"

func TestSettleCountsForgottenShowsNowhere(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	appendLines(t, o, feedback.Event{Kind: "forget", TS: settleStart.Add(10 * time.Minute), IDs: []string{"m3"}})
	want := feedback.SettleResult{Shows: 2, Credits: 1, Misses: 1}
	if res := mustSettle(t, o, settleAt); res != want {
		t.Errorf("Settle: %+v, want %+v (m3 forgotten)", res, want)
	}
	if _, ok := folded(t, o).Memories["m3"]; ok {
		t.Error("a forgotten memory has a row")
	}
}

func TestSettleDuringAnUpdateDoesNothing(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	if err := state.WriteUpdateMarker(o.Dirs, state.UpdateMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	res, err := Settle(context.Background(), o, testParams, at(settleAt))
	if err != nil || res != (feedback.SettleResult{}) {
		t.Errorf("Settle during an update: %+v, %v; want nothing, no error", res, err)
	}
	if n := len(settleEvents(t, o)); n != 0 {
		t.Errorf("%d settle lines journaled during an update", n)
	}
	absent(t, mainPath(o))
}

func TestSettleRetriesAFailedExport(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	jsonPath := filepath.Join(feedbackDir(o), "ledger.json")
	if err := os.MkdirAll(jsonPath, 0o700); err != nil { // a directory: the export cannot replace it
		t.Fatal(err)
	}
	res, err := Settle(context.Background(), o, testParams, at(settleAt))
	if !errors.Is(err, ErrExport) || res != settledOnce {
		t.Fatalf("Settle with an export that fails: %+v, %v; want %+v and ErrExport", res, err, settledOnce)
	}
	countedOnce(t, "after the failed export", o)
	if err := os.Remove(jsonPath); err != nil {
		t.Fatal(err)
	}
	// Nothing new to fold: the settle still exports, as the last one could not.
	if res := mustSettle(t, o, settleAt.Add(time.Hour)); res != (feedback.SettleResult{}) {
		t.Errorf("the next settle: %+v, want nothing", res)
	}
	exportMatches(t, "after the retry", o)
}

// A ledger.json that does not parse, is gone or has another schema is
// exported again at the next settle, even with nothing new folded.
func TestSettleRepairsACorruptLedgerJSON(t *testing.T) {
	for name, damage := range map[string]func(path string) error{
		"garbage":        func(path string) error { return os.WriteFile(path, []byte("{not json"), 0o600) },
		"deleted":        os.Remove,
		"another schema": func(path string) error { return os.WriteFile(path, []byte(`{"schema":2,"memories":{}}`), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			o := tempOpts(t)
			settleSessions(t, o)
			mustSettle(t, o, settleAt)
			if err := damage(filepath.Join(feedbackDir(o), "ledger.json")); err != nil {
				t.Fatal(err)
			}
			gen := metaInt(t, o, "generation")
			if exported := metaInt(t, o, "exported_generation"); exported != gen {
				t.Fatalf("exported_generation %d, generation %d: the export is behind before the damage", exported, gen)
			}
			if res := mustSettle(t, o, settleAt.Add(time.Hour)); res != (feedback.SettleResult{}) {
				t.Errorf("the settle after the damage: %+v, want nothing", res)
			}
			if got := metaInt(t, o, "generation"); got != gen {
				t.Fatalf("generation %d, want %d: the settle folded something", got, gen)
			}
			exportMatches(t, "after the repair", o)
		})
	}
}

// TestSoundLedgerJSONIsNotRewritten: a settle with nothing new leaves a
// sound ledger.json as it is, bytes and modification time.
func TestSoundLedgerJSONIsNotRewritten(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	mustSettle(t, o, settleAt)
	path := filepath.Join(feedbackDir(o), "ledger.json")
	past := clock.Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	before := readBytes(t, path)
	if res := mustSettle(t, o, settleAt.Add(time.Hour)); res != (feedback.SettleResult{}) {
		t.Errorf("the second settle: %+v, want nothing", res)
	}
	if got := readBytes(t, path); !bytes.Equal(got, before) {
		t.Error("ledger.json's bytes changed")
	}
	if fi, err := os.Stat(path); err != nil || !fi.ModTime().Equal(past) {
		t.Errorf("ledger.json rewritten: mtime %v (%v), want %v", fi.ModTime(), err, past)
	}
}

// A ledger.json that cannot be read for another reason than its content
// (a permission) is written once per generation, not at every settle.
func TestUnreadableLedgerJSONIsWrittenOncePerGeneration(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: a file mode that refuses its owner's read is a Unix permission")
	o := tempOpts(t)
	settleSessions(t, o)
	mustSettle(t, o, settleAt)
	path := filepath.Join(feedbackDir(o), "ledger.json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this user reads a file of mode 0 (root)")
	}
	mustSettle(t, o, settleAt.Add(time.Hour))
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0 {
		t.Fatalf("a settle with nothing new rewrote an unreadable ledger.json: %v %v", fi.Mode(), err)
	}
	// A new generation is exported, over it.
	appendLines(t, o, hideOf(settleAt.Add(90*time.Minute), "m3"))
	mustSettle(t, o, settleAt.Add(2*time.Hour))
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the next generation was not exported: %v %v", fi.Mode(), err)
	}
	exportMatches(t, "after the new generation", o)
}

// A fold that sets a corrupt ledger.db aside says so in its result.
func TestSettleReportsASetAside(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	if res := mustSettle(t, o, settleAt); res.Rebuilt != "" {
		t.Errorf("a settle over a healthy file: Rebuilt %q", res.Rebuilt)
	}
	corruptPage(t, mustOpenWriter(t, o), "meta")
	if res := mustSettle(t, o, settleAt.Add(time.Hour)); res.Rebuilt != ReasonSetAside {
		t.Errorf("the settle that set the file aside: %+v, want Rebuilt %q", res, ReasonSetAside)
	}
	countedOnce(t, "after the set-aside", o)
	exportMatches(t, "after the set-aside", o)
}

func TestCrashBetweenAppendAndFold(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	crash := errors.New("crash before the fold commits")
	setSeam(t, &beforeFoldCommit, nil)
	// After the first build (OpenWriter's own fold), before the append.
	setSeam(t, &afterDecide, func() { beforeFoldCommit = func() error { return crash } })
	if res, err := Settle(context.Background(), o, testParams, at(settleAt)); !errors.Is(err, crash) || res != (feedback.SettleResult{}) {
		t.Fatalf("Settle with a crash before its fold commits: %+v, %v; want nothing counted and the crash", res, err)
	}
	if n := len(settleEvents(t, o)); n != 1 {
		t.Fatalf("%d settle lines after the crash, want the 1 journaled before it", n)
	}
	if l := folded(t, o); len(l.Memories) != 0 {
		t.Fatalf("the crashed fold left rows: %+v", l.Memories)
	}

	afterDecide, beforeFoldCommit = nil, nil
	if res := mustSettle(t, o, settleAt.Add(time.Hour)); res != (feedback.SettleResult{}) {
		t.Errorf("the settle after the crash decided %+v again, want nothing", res)
	}
	if n := len(settleEvents(t, o)); n != 1 {
		t.Errorf("%d settle lines after the next settle, want 1: its keys were journaled again", n)
	}
	countedOnce(t, "after the next settle", o)
	matchesOracle(t, "after the next settle", o, testParams)
	exportMatches(t, "after the next settle", o)
}

// barrier is a seam two deciders meet at: each waits for the other, with
// no timer (a decider that never comes, held back by the first, hangs
// until go test's -timeout).
func barrier() func() {
	var n atomic.Int32
	all := make(chan struct{})
	return func() {
		if n.Add(1) == 2 {
			close(all)
		}
		<-all
	}
}

func TestTwoDecidersCountEachShowOnce(t *testing.T) {
	o := tempOpts(t)
	settleSessions(t, o)
	mustCloseWriter(t, mustOpenWriter(t, o)) // built: the race is the decide, not the first build
	setSeam(t, &afterDecide, barrier())
	var wg sync.WaitGroup
	var res [2]feedback.SettleResult
	var errs [2]error
	for i := range 2 {
		wg.Go(func() { res[i], errs[i] = Settle(context.Background(), o, testParams, at(settleAt)) })
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || res[i] != settledOnce {
			t.Errorf("decider %d: %+v, %v; want %+v", i, res[i], errs[i], settledOnce)
		}
	}
	twoSettlesShareTheirKeys(t, o)
	countedOnce(t, "after two deciders", o)
	matchesOracle(t, "after two deciders", o, testParams)
	exportMatches(t, "after two deciders", o)
}

// deciderEnv runs TestDecidingProcess as one decider of
// TestTwoDecidingProcesses: "<state dir>|<barrier dir>|<name>".
const deciderEnv = "BACKSIGHT_TEST_LEDGER_DECIDER"

func TestTwoDecidingProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two processes")
	}
	o := tempOpts(t)
	settleSessions(t, o)
	mustCloseWriter(t, mustOpenWriter(t, o))
	gate := t.TempDir()
	var wg sync.WaitGroup
	outs := make([]bytes.Buffer, 2)
	errs := make([]error, 2)
	args := []string{"-test.run=^TestDecidingProcess$", "-test.v"}
	if d, ok := t.Deadline(); ok {
		// The deciders' barrier has no timer: a decider left waiting ends
		// with this test's own -timeout instead of outliving it.
		args = append(args, "-test.timeout="+time.Until(d).String()) //nolint:forbidigo // hands the child go test's own remaining timeout
	}
	for i := range 2 {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), deciderEnv+"="+o.Dirs.State+"|"+gate+"|"+fmt.Sprint(i))
		cmd.Stdout, cmd.Stderr = &outs[i], &outs[i]
		wg.Go(func() { errs[i] = cmd.Run() })
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("decider %d: %v\n%s", i, errs[i], outs[i].String())
		}
		if got := deciderResult(t, outs[i].String()); got != settledOnce {
			t.Errorf("decider %d settled %+v, want %+v", i, got, settledOnce)
		}
	}
	twoSettlesShareTheirKeys(t, o)
	countedOnce(t, "after two deciding processes", o)
	matchesOracle(t, "after two deciding processes", o, testParams)
	exportMatches(t, "after two deciding processes", o)
}

// deciderResult is the SettleResult a decider process printed.
func deciderResult(t *testing.T, out string) feedback.SettleResult {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "DECIDED "); ok {
			var res feedback.SettleResult
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				t.Fatal(err)
			}
			return res
		}
	}
	t.Fatalf("the decider printed no result:\n%s", out)
	return feedback.SettleResult{}
}

// TestDecidingProcess is one decider of TestTwoDecidingProcesses, in a
// process of its own: it settles once both deciders decided (each drops
// a file in the barrier folder and waits for the other's).
func TestDecidingProcess(t *testing.T) {
	spec := os.Getenv(deciderEnv)
	if spec == "" {
		t.Skip("one decider of TestTwoDecidingProcesses")
	}
	parts := strings.Split(spec, "|")
	if len(parts) != 3 {
		t.Fatalf("%s=%q", deciderEnv, spec)
	}
	o := Options{Dirs: paths.Dirs{State: parts[0]}, Getenv: func(string) string { return "" }, BinaryVersion: "v-test", Now: func() time.Time { return clock }}
	gate, name := parts[1], parts[2]
	setSeam(t, &afterDecide, func() {
		if err := os.WriteFile(filepath.Join(gate, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// No deadline: a decider that never comes hangs this one until its
		// -test.timeout, the parent's remaining time.
		for {
			if names, _ := os.ReadDir(gate); len(names) == 2 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	res, err := Settle(context.Background(), o, testParams, at(settleAt))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	fmt.Printf("DECIDED %s\n", b)
}
