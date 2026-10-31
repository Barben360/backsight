package claudecfg

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/testtool"
)

func runner(t *testing.T) (Runner, string) {
	t.Helper()
	dir := t.TempDir()
	n := 0
	return Runner{PreImages: filepath.Join(dir, "state", "pre-images"), Journal: filepath.Join(dir, "state", "journal.jsonl"),
		Command: "client config apply", Now: func() time.Time {
			n++
			return time.Date(2026, 10, 24, 12, 0, n, 0, time.UTC)
		}}, dir
}

func retention(days int) func([]byte) ([]byte, []Change, error) {
	return func(doc []byte) ([]byte, []Change, error) {
		out, ch, err := Set(doc, []string{"cleanupPeriodDays"}, days)
		return out, []Change{ch}, err
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func journal(t *testing.T, r Runner) []map[string]any {
	t.Helper()
	f, err := os.Open(r.Journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []map[string]any
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var m map[string]any
		_ = json.Unmarshal(sc.Bytes(), &m)
		lines = append(lines, m)
	}
	return lines
}

func TestApplyBacksUpThenWritesOnce(t *testing.T) {
	testtool.SkipWindows(t, "Task 3: mode bits")
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	orig := "{\n  \"model\": \"opus\"\n}\n"
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, res, err := r.Apply([]Edit{{Target: target, Apply: retention(3650)}})
	if err != nil || ts == "" || len(res) != 1 || !res[0].Written || len(res[0].Changes) != 1 {
		t.Fatalf("ts %q res %+v err %v", ts, res, err)
	}
	if got := read(t, target); got != "{\n  \"model\": \"opus\",\n  \"cleanupPeriodDays\": 3650\n}\n" {
		t.Errorf("file:\n%s", got)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want the file's own 0644", fi.Mode().Perm())
	}
	m, err := LoadManifest(r.PreImages, ts)
	if err != nil || len(m.Files) != 1 || m.Files[0].PreImage == nil || m.Files[0].ShaBefore == m.Files[0].ShaAfter || m.Command != "client config apply" {
		t.Fatalf("manifest %+v err %v", m, err)
	}
	pre := filepath.Join(r.PreImages, ts, *m.Files[0].PreImage)
	if read(t, pre) != orig {
		t.Error("pre-image is not the original bytes")
	}
	if fi, _ := os.Stat(pre); fi.Mode().Perm() != 0o600 {
		t.Errorf("pre-image mode %v", fi.Mode().Perm())
	}
	j := journal(t, r)
	if len(j) != 1 || j[0]["step"] != "claude-config" || j[0]["action"] != "edit" || j[0]["target"] != target || j[0]["pre_image"] != pre {
		t.Errorf("journal %v", j)
	}
	ts2, res2, err := r.Apply([]Edit{{Target: target, Apply: retention(3650)}})
	if err != nil || ts2 != "" || res2[0].Written || len(res2[0].Changes) != 0 {
		t.Errorf("second apply: ts %q res %+v err %v", ts2, res2, err)
	}
	if runs, _ := Runs(r.PreImages); len(runs) != 1 {
		t.Errorf("a no-op run left a directory: %v", runs)
	}
}

func TestPlanWritesNothing(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	res, err := Plan([]Edit{{Target: target, Apply: retention(3650)}})
	if err != nil || len(res) != 1 || len(res[0].Changes) != 1 || *res[0].Changes[0].New != "3650" || res[0].Changes[0].Old != nil {
		t.Fatalf("res %+v err %v", res, err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Error("plan created the file")
	}
	if runs, _ := Runs(r.PreImages); len(runs) != 0 {
		t.Error("plan created a run")
	}
}

// Claude Code rewrites its files often: a change between our read and our
// write is kept and our edit re-applied on top; the pre-image holds the
// bytes actually replaced.
func TestApplyReappliesOverAConcurrentChange(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, ".claude.json")
	_ = os.WriteFile(target, []byte(`{"numStartups": 1}`), 0o600)
	calls := 0
	apply := func(doc []byte) ([]byte, []Change, error) {
		calls++
		if calls == 1 {
			_ = os.WriteFile(target, []byte(`{"numStartups": 2}`), 0o600)
		}
		return retention(30)(doc)
	}
	ts, _, err := r.Apply([]Edit{{Target: target, Apply: apply}})
	if err != nil || calls != 2 {
		t.Fatalf("err %v calls %d", err, calls)
	}
	if got := read(t, target); got != `{"numStartups": 2, "cleanupPeriodDays": 30}` {
		t.Errorf("file %s", got)
	}
	m, _ := LoadManifest(r.PreImages, ts)
	if read(t, filepath.Join(r.PreImages, ts, *m.Files[0].PreImage)) != `{"numStartups": 2}` {
		t.Error("pre-image is not the replaced version")
	}
}

func TestApplyGivesUpAfterThreeConflicts(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, ".claude.json")
	_ = os.WriteFile(target, []byte(`{"n": 0}`), 0o600)
	calls := 0
	apply := func(doc []byte) ([]byte, []Change, error) {
		calls++
		_ = os.WriteFile(target, []byte(fmt.Sprintf(`{"n": %d}`, calls)), 0o600)
		return retention(30)(doc)
	}
	_, _, err := r.Apply([]Edit{{Target: target, Apply: apply}})
	if !errors.Is(err, ErrBusy) || calls != 3 {
		t.Fatalf("err %v calls %d", err, calls)
	}
	if got := read(t, target); got != `{"n": 3}` {
		t.Errorf("the concurrent writer's content was overwritten: %s", got)
	}
	if len(journal(t, r)) != 0 {
		t.Error("journal records a write that did not happen")
	}
}

func TestApplyRefusesInvalidJSONAndLeavesItAlone(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(target, []byte(`{"a": 1,}`), 0o600)
	if _, _, err := r.Apply([]Edit{{Target: target, Apply: retention(3650)}}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if read(t, target) != `{"a": 1,}` {
		t.Error("invalid file was modified")
	}
	if runs, _ := Runs(r.PreImages); len(runs) != 0 {
		t.Error("a refused edit created a run")
	}
}

func TestApplyFollowsSymlinks(t *testing.T) {
	r, dir := runner(t)
	real := filepath.Join(dir, "dotfiles", "settings.json")
	_ = os.MkdirAll(filepath.Dir(real), 0o700)
	_ = os.WriteFile(real, []byte(`{}`), 0o644)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	if _, _, err := r.Apply([]Edit{{Target: link, Apply: retention(3650)}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if read(t, real) == `{}` {
		t.Error("the link's target was not edited")
	}
}

func TestRestoreUndoesAndIsItselfUndoable(t *testing.T) {
	r, dir := runner(t)
	settings := filepath.Join(dir, "settings.json")
	created := filepath.Join(dir, "new.json")
	orig := "{\n  \"model\": \"opus\"\n}\n"
	_ = os.WriteFile(settings, []byte(orig), 0o600)
	ts, _, err := r.Apply([]Edit{{Target: settings, Apply: retention(3650)}, {Target: created, Apply: retention(1)}})
	if err != nil {
		t.Fatal(err)
	}
	edited := read(t, settings)
	r.Command = "client config restore"
	ts2, res, err := r.Restore("", false)
	if err != nil || len(res) != 2 || res[0].Status != "restored" || res[1].Status != "deleted" {
		t.Fatalf("restore: %+v err %v", res, err)
	}
	if read(t, settings) != orig {
		t.Error("settings not restored")
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file the run created is still there")
	}
	if ts2 == ts || ts2 == "" {
		t.Fatalf("restore did not make its own run: %q", ts2)
	}
	if _, res, err := r.Restore(ts2, false); err != nil || res[0].Status != "restored" || res[1].Status != "restored" {
		t.Fatalf("undo of restore: %+v err %v", res, err)
	}
	if read(t, settings) != edited || read(t, created) == "" {
		t.Error("undoing the restore did not bring the edit back")
	}
	if _, res, err := r.Restore(ts, false); err != nil || res[0].Status != "restored" {
		t.Errorf("restoring an older run by ts: %+v %v", res, err)
	}
}

func TestRestoreRefusesFilesChangedSinceUnlessForced(t *testing.T) {
	r, dir := runner(t)
	settings := filepath.Join(dir, "settings.json")
	other := filepath.Join(dir, "other.json")
	_ = os.WriteFile(settings, []byte(`{}`), 0o600)
	_ = os.WriteFile(other, []byte(`{}`), 0o600)
	ts, _, _ := r.Apply([]Edit{{Target: settings, Apply: retention(3650)}, {Target: other, Apply: retention(1)}})
	_ = os.WriteFile(settings, []byte(`{"user": "edit"}`), 0o600)
	otherEdited := read(t, other)
	_, res, err := r.Restore(ts, false)
	if !errors.Is(err, ErrChangedSince) || res[0].Status != "refused" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if read(t, settings) != `{"user": "edit"}` || read(t, other) != otherEdited {
		t.Error("a refused restore wrote something")
	}
	if _, _, err := r.Restore(ts, true); err != nil {
		t.Fatal(err)
	}
	if read(t, settings) != `{}` || read(t, other) != `{}` {
		t.Error("--force did not restore")
	}
}

func TestOnlyTheLastTwentyRunsAreKept(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	for i := 1; i <= 23; i++ {
		if _, _, err := r.Apply([]Edit{{Target: target, Apply: retention(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := Runs(r.PreImages)
	if len(runs) != 20 {
		t.Fatalf("%d runs kept", len(runs))
	}
	m, _ := LoadManifest(r.PreImages, runs[0])
	if pre := read(t, filepath.Join(r.PreImages, runs[0], *m.Files[0].PreImage)); pre != "{\n  \"cleanupPeriodDays\": 3\n}\n" {
		t.Errorf("oldest kept run should hold the value before run 4, got %q", pre)
	}
}

func TestPinnedRunsSurvivePruning(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	first, _, err := r.Apply([]Edit{{Target: target, Apply: retention(1)}})
	if err != nil || Pin(r.PreImages, first) != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 25; i++ {
		if _, _, err := r.Apply([]Edit{{Target: target, Apply: retention(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := Runs(r.PreImages)
	if len(runs) != 21 || runs[0] != first {
		t.Fatalf("%d runs kept, oldest %s (pinned %s)", len(runs), runs[0], first)
	}
	Unpin(r.PreImages, first)
	if _, _, err := r.Apply([]Edit{{Target: target, Apply: retention(26)}}); err != nil {
		t.Fatal(err)
	}
	if runs, _ = Runs(r.PreImages); len(runs) != 20 || runs[0] == first {
		t.Errorf("an unpinned run must be pruned again: %d runs, oldest %s", len(runs), runs[0])
	}
}

func TestRestoreWithoutRuns(t *testing.T) {
	r, _ := runner(t)
	if _, _, err := r.Restore("", false); err == nil {
		t.Error("restore with no run must fail")
	}
}

// A run that ends without writing (every attempt conflicted) leaves no run
// behind: the default restore still finds the last real run, and failed
// runs never push real ones out (M1 review).
func TestBusyRunsLeaveNoRunAndPruneNothing(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(target, []byte(`{}`), 0o600)
	good, _, err := r.Apply([]Edit{{Target: target, Apply: retention(3650)}})
	if err != nil {
		t.Fatal(err)
	}
	busy := func(doc []byte) ([]byte, []Change, error) {
		_ = os.WriteFile(target, append(doc, ' '), 0o600)
		return retention(1)(doc)
	}
	for i := 0; i < 21; i++ {
		if _, _, err := r.Apply([]Edit{{Target: target, Apply: busy}}); !errors.Is(err, ErrBusy) {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	ents, _ := os.ReadDir(r.PreImages)
	if len(ents) != 1 || ents[0].Name() != good {
		t.Fatalf("pre-images %v, want only %s", ents, good)
	}
	if _, _, err := r.Restore("", true); err != nil {
		t.Errorf("default restore after busy runs: %v", err)
	}
}

// Restoring the oldest kept run must not prune it halfway (M1 review).
func TestRestoringTheOldestRunKeepsItWhole(t *testing.T) {
	r, dir := runner(t)
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	oldest, _, _ := r.Apply([]Edit{{Target: a, Apply: retention(1)}, {Target: b, Apply: retention(1)}})
	for i := 2; i <= 20; i++ {
		_, _, _ = r.Apply([]Edit{{Target: filepath.Join(dir, "c.json"), Apply: retention(i)}})
	}
	_, res, err := r.Restore(oldest, false)
	if err != nil || len(res) != 2 || res[0].Status != "deleted" || res[1].Status != "deleted" {
		t.Fatalf("res %+v err %v", res, err)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s not restored", p)
		}
	}
}

// Without --force a restore writes only over the exact bytes its run left:
// a change landing between the check and the write is refused, not
// overwritten (M1 review).
func TestRestoreNeverOverwritesALateChange(t *testing.T) {
	r, dir := runner(t)
	target := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(target, []byte(`{}`), 0o600)
	ts, _, _ := r.Apply([]Edit{{Target: target, Apply: retention(3650)}})
	beforePut = func() { _ = os.WriteFile(target, []byte(`{"late": true}`), 0o600) }
	t.Cleanup(func() { beforePut = func() {} })
	if _, _, err := r.Restore(ts, false); !errors.Is(err, ErrChangedSince) {
		t.Fatalf("err %v", err)
	}
	if read(t, target) != `{"late": true}` {
		t.Error("the late change was overwritten")
	}
}

func TestRestoreSkipsAFileAlreadyBackAndJournalsTheRestore(t *testing.T) {
	r, dir := runner(t)
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	_ = os.WriteFile(a, []byte(`{}`), 0o600)
	_ = os.WriteFile(b, []byte(`{}`), 0o600)
	ts, _, _ := r.Apply([]Edit{{Target: a, Apply: retention(1)}, {Target: b, Apply: retention(1)}})
	_ = os.WriteFile(a, []byte(`{}`), 0o600) // the user already put a back by hand
	r.Command = "client config restore"
	_, res, err := r.Restore(ts, false)
	if err != nil || res[0].Status != "unchanged" || res[1].Status != "restored" {
		t.Fatalf("res %+v err %v", res, err)
	}
	// Two edits, then the restore: started, the one file it put back,
	// finished.
	j := journal(t, r)
	if len(j) != 5 {
		t.Fatalf("journal %v", j)
	}
	if put := j[3]; put["action"] != "restore" || put["target"] != b || put["command"] != "client config restore" || put["status"] != "done" || put["ts"] == "" {
		t.Errorf("restore line %v", put)
	}
	if j[2]["status"] != "started" || j[2]["restores"] != ts || j[4]["status"] != "finished" || j[4]["restores"] != ts || j[4]["run"] == "" {
		t.Errorf("start and finish lines %v, %v", j[2], j[4])
	}
}

func TestARestoreThatStopsHalfwayStaysPendingUntilRerun(t *testing.T) {
	r, dir := runner(t)
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	_ = os.WriteFile(a, []byte(`{}`), 0o600)
	_ = os.WriteFile(b, []byte(`{}`), 0o600)
	ts, _, _ := r.Apply([]Edit{{Target: a, Apply: retention(1)}, {Target: b, Apply: retention(1)}})
	// b changes between the check and its write: a is restored, b is not.
	puts := 0
	beforePut = func() {
		if puts++; puts == 2 {
			_ = os.WriteFile(b, []byte(`{"late": true}`), 0o600)
		}
	}
	t.Cleanup(func() { beforePut = func() {} })
	r.Command = "client config restore"
	if _, _, err := r.Restore(ts, false); !errors.Is(err, ErrChangedSince) {
		t.Fatalf("err %v", err)
	}
	lines, err := ReadJournal(r.Journal)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingRestores(lines)
	if len(pending) != 1 || pending[0].Restores != ts || pending[0].Command != "client config restore" {
		t.Fatalf("pending %+v in %+v", pending, lines)
	}
	beforePut = func() {}
	if _, _, err := r.Restore(ts, true); err != nil {
		t.Fatal(err)
	}
	lines, _ = ReadJournal(r.Journal)
	if p := PendingRestores(lines); len(p) != 0 {
		t.Errorf("a finished rerun leaves %+v pending", p)
	}
}

func TestARefusedOrEmptyRestoreJournalsNothingPending(t *testing.T) {
	r, dir := runner(t)
	a := filepath.Join(dir, "a.json")
	_ = os.WriteFile(a, []byte(`{}`), 0o600)
	ts, _, _ := r.Apply([]Edit{{Target: a, Apply: retention(1)}})
	_ = os.WriteFile(a, []byte(`{"user": 1}`), 0o600)
	if _, _, err := r.Restore(ts, false); !errors.Is(err, ErrChangedSince) {
		t.Fatalf("err %v", err)
	}
	_ = os.WriteFile(a, []byte(`{}`), 0o600) // back as before the run: nothing to do
	if _, _, err := r.Restore(ts, false); err != nil {
		t.Fatal(err)
	}
	lines, _ := ReadJournal(r.Journal)
	if p := PendingRestores(lines); len(p) != 0 || len(lines) != 1 {
		t.Errorf("pending %+v, journal %+v", p, lines)
	}
}

func TestReadJournalSkipsBrokenLinesAndAMissingFile(t *testing.T) {
	dir := t.TempDir()
	if lines, err := ReadJournal(filepath.Join(dir, "none.jsonl")); err != nil || lines != nil {
		t.Errorf("missing: %v %v", lines, err)
	}
	p := filepath.Join(dir, "journal.jsonl")
	_ = os.WriteFile(p, []byte("{\"step\":\"server\",\"status\":\"done\"}\nnot json\n{\"step\":\"claude-config\",\"action\":\"edit\",\"target\":\"/s.json\",\"command\":\"client install\"}\n"), 0o600)
	lines, err := ReadJournal(p)
	if err != nil || len(lines) != 2 || lines[1].Target != "/s.json" || lines[1].Command != "client install" || lines[0].Step != "server" {
		t.Errorf("lines %+v err %v", lines, err)
	}
}

func TestRunsWithTheSameTimestampSortInOrder(t *testing.T) {
	r, dir := runner(t)
	r.Now = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }
	target := filepath.Join(dir, "settings.json")
	var last string
	for i := 1; i <= 12; i++ {
		last, _, _ = r.Apply([]Edit{{Target: target, Apply: retention(i)}})
	}
	runs, _ := Runs(r.PreImages)
	if runs[len(runs)-1] != last {
		t.Errorf("last run %s sorts before others: %v", last, runs)
	}
}

// A rerun that finds every file already back (the user finished it by
// hand) writes nothing, but closes the restore left pending (0067 block 4
// review rules).
func TestARerunWithNothingLeftToWriteFinishesThePendingRestore(t *testing.T) {
	r, dir := runner(t)
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	_ = os.WriteFile(a, []byte(`{}`), 0o600)
	_ = os.WriteFile(b, []byte(`{}`), 0o600)
	ts, _, _ := r.Apply([]Edit{{Target: a, Apply: retention(1)}, {Target: b, Apply: retention(1)}})
	puts := 0
	beforePut = func() {
		if puts++; puts == 2 {
			_ = os.WriteFile(b, []byte(`{"late": true}`), 0o600)
		}
	}
	t.Cleanup(func() { beforePut = func() {} })
	r.Command = "client config restore"
	if _, _, err := r.Restore(ts, false); !errors.Is(err, ErrChangedSince) {
		t.Fatalf("err %v", err)
	}
	beforePut = func() {}
	_ = os.WriteFile(b, []byte(`{}`), 0o600) // put back by hand
	run, res, err := r.Restore(ts, false)
	if err != nil || run != "" || res[0].Status != "unchanged" || res[1].Status != "unchanged" {
		t.Fatalf("run %q res %+v err %v", run, res, err)
	}
	lines, _ := ReadJournal(r.Journal)
	if p := PendingRestores(lines); len(p) != 0 {
		t.Errorf("still pending: %+v", p)
	}
	last := lines[len(lines)-1]
	if last.Status != "finished" || last.Restores != ts || last.Run != "" {
		t.Errorf("last line %+v", last)
	}
	// Once finished, a further no-op rerun adds nothing.
	n := len(lines)
	if _, _, err := r.Restore(ts, false); err != nil {
		t.Fatal(err)
	}
	if lines, _ = ReadJournal(r.Journal); len(lines) != n {
		t.Errorf("journal grew to %d lines: %+v", len(lines), lines[n:])
	}
}

func TestPendingRestoresKeepsOnlyTheUnfinishedRuns(t *testing.T) {
	line := func(status, run string) JournalLine {
		return JournalLine{Step: "claude-config", Action: "restore", Status: status, Restores: run}
	}
	lines := []JournalLine{
		line("started", "A"), line("started", "B"),
		{Step: "claude-config", Action: "edit", Target: "/s.json", Status: "done"},
		{Step: "server", Action: "restore", Status: "finished", Restores: "B"}, // another step's line
		line("finished", "A"),
	}
	p := PendingRestores(lines)
	if len(p) != 1 || p[0].Restores != "B" {
		t.Errorf("pending %+v", p)
	}
	// A restart of B after A finished is still B's, once.
	if p = PendingRestores(append(lines, line("started", "B"), line("finished", "B"))); len(p) != 0 {
		t.Errorf("pending %+v", p)
	}
}
