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
	j := journal(t, r)
	last := j[len(j)-1]
	if len(j) != 3 || last["action"] != "restore" || last["target"] != b || last["command"] != "client config restore" || last["status"] != "done" || last["ts"] == "" {
		t.Errorf("journal %v", j)
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
