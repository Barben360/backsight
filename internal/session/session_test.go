package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

func store(t *testing.T) Store {
	dir := t.TempDir()
	return Store{Dir: filepath.Join(dir, "sessions"), Locks: filepath.Join(dir, "locks")}
}

func TestUpdateThenLoad(t *testing.T) {
	s := store(t)
	err := s.Update("abc-123", func(st *State) error {
		st.Project, st.Prompts = "billing", 2
		st.Injected = append(st.Injected, Injection{ID: "m1", Text: "t", Project: "billing", Score: 0.7, Scope: "project", Prompt: 2, Rule: "first"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load("abc-123")
	if err != nil || st.Project != "billing" || st.Prompts != 2 || len(st.Injected) != 1 || st.Injected[0].Rule != "first" {
		t.Fatalf("%+v %v", st, err)
	}
	if fi, _ := os.Stat(filepath.Join(s.Dir, "abc-123.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if st, err := s.Load("never-seen"); err != nil || st.Prompts != 0 {
		t.Errorf("a new session is empty: %+v %v", st, err)
	}
}

// Hooks never wait: a held lock gives up at once (SPEC §6.1).
func TestUpdateGivesUpOnAHeldLock(t *testing.T) {
	s := store(t)
	l, err := state.TryAcquire(filepath.Join(s.Locks, "state-abc.lock"), "other hook")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	start := time.Now()
	err = s.Update("abc", func(st *State) error { st.Prompts = 1; return nil })
	if !errors.Is(err, ErrBusy) || time.Since(start) > 100*time.Millisecond {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
	if st, _ := s.Load("abc"); st.Prompts != 0 {
		t.Error("written despite the lock")
	}
}

func TestUpdateBreaksAStaleLock(t *testing.T) {
	s := store(t)
	_ = os.MkdirAll(s.Locks, 0o700)
	_ = os.WriteFile(filepath.Join(s.Locks, "state-abc.lock"), []byte(`{"pid":999999999,"command":"dead","since":"2026-10-24T09:00:00Z"}`), 0o600)
	if err := s.Update("abc", func(st *State) error { st.Prompts = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Load("abc"); st.Prompts != 1 {
		t.Error("not written after breaking the stale lock")
	}
}

// A corrupt state restarts paused: a pause is never lost (flushes treat an
// unreadable state as paused too).
func TestCorruptStateRestartsPaused(t *testing.T) {
	s := store(t)
	_ = os.MkdirAll(s.Dir, 0o700)
	_ = os.WriteFile(filepath.Join(s.Dir, "abc.json"), []byte("{not json"), 0o600)
	if p, _ := Paused(s.Dir, "abc"); !p {
		t.Error("an unreadable state must count as paused")
	}
	if err := s.Update("abc", func(st *State) error { st.Prompts++; return nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Load("abc"); !st.Paused || st.Prompts != 1 {
		t.Errorf("%+v", st)
	}
	if p, _ := Paused(s.Dir, "unknown"); p {
		t.Error("an unknown session is not paused")
	}
}

func TestAFailingUpdateWritesNothing(t *testing.T) {
	s := store(t)
	_ = s.Update("abc", func(st *State) error { st.Prompts = 1; return nil })
	if err := s.Update("abc", func(st *State) error { st.Prompts = 9; return errors.New("no") }); err == nil {
		t.Fatal("error lost")
	}
	if st, _ := s.Load("abc"); st.Prompts != 1 {
		t.Errorf("written despite the error: %+v", st)
	}
}

func TestSessionIDsCannotEscapeTheDirectory(t *testing.T) {
	s := store(t)
	for _, id := range []string{"", "../x", "a/b", ".hidden", "a\x00b"} {
		if err := s.Update(id, func(*State) error { return nil }); err == nil {
			t.Errorf("id %q accepted", id)
		}
		if _, err := s.Load(id); err == nil {
			t.Errorf("load %q accepted", id)
		}
	}
}

func TestLatestIsTheMostRecentlyWritten(t *testing.T) {
	s := store(t)
	_ = s.Update("old", func(*State) error { return nil })
	_ = s.Update("new", func(*State) error { return nil })
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(s.Dir, "old.json"), past, past)
	_ = os.WriteFile(filepath.Join(s.Dir, ".hidden.json"), []byte("{}"), 0o600) // not a session id
	_ = os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("x"), 0o600)
	if id, err := s.Latest(); err != nil || id != "new" {
		t.Errorf("%q %v", id, err)
	}
	if id, err := (Store{Dir: filepath.Join(t.TempDir(), "none")}).Latest(); err != nil || id != "" {
		t.Errorf("no sessions: %q %v", id, err)
	}
}

// A state that cannot be read for now (permissions) is never overwritten.
func TestAnUnreadableStateIsNotOverwritten(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	s := store(t)
	_ = s.Update("abc", func(st *State) error { st.Prompts = 3; return nil })
	p := filepath.Join(s.Dir, "abc.json")
	_ = os.Chmod(p, 0o000)
	defer func() { _ = os.Chmod(p, 0o600) }()
	if err := s.Update("abc", func(st *State) error { st.Prompts = 9; return nil }); err == nil {
		t.Error("an unreadable state was replaced")
	}
	_ = os.Chmod(p, 0o600)
	if st, _ := s.Load("abc"); st.Prompts != 3 {
		t.Errorf("%+v", st)
	}
}

func TestPauseSpans(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	var st State
	if !st.SetPaused(true, t0) || st.SetPaused(true, t0.Add(time.Minute)) {
		t.Fatal("pause, then a second pause, must change once")
	}
	if !InSpans(st.PausedSpans, t0.Add(time.Hour)) {
		t.Error("an open span covers what comes after the pause")
	}
	st.SetPaused(false, t0.Add(10*time.Minute))
	st.SetPaused(true, t0.Add(20*time.Minute))
	st.SetPaused(false, t0.Add(30*time.Minute))
	for _, c := range []struct {
		min  int
		want bool
	}{{-1, false}, {0, true}, {9, true}, {10, false}, {15, false}, {20, true}, {30, false}, {40, false}} {
		if got := InSpans(st.PausedSpans, t0.Add(time.Duration(c.min)*time.Minute)); got != c.want {
			t.Errorf("minute %d: %v, want %v", c.min, got, c.want)
		}
	}
	if st.Paused || len(st.PausedSpans) != 2 {
		t.Errorf("%+v", st)
	}
}

// The texts of shown memories are kept for the end-of-turn check, at most
// 50: the ones just shown stay, older ones make room.
func TestKeepShownTextsIsCapped(t *testing.T) {
	var st State
	st.KeepShownTexts(map[string]string{"m1": "first", "m2": "second"})
	st.KeepShownTexts(map[string]string{"m1": "first, updated"})
	if len(st.ShownTexts) != 2 || st.ShownTexts["m1"] != "first, updated" || st.ShownTexts["m2"] != "second" {
		t.Fatalf("%v", st.ShownTexts)
	}
	for i := range 60 {
		st.KeepShownTexts(map[string]string{fmt.Sprintf("old-%d", i): "x"})
	}
	batch := map[string]string{}
	for i := range 10 {
		batch[fmt.Sprintf("new-%d", i)] = "y"
	}
	st.KeepShownTexts(batch)
	if len(st.ShownTexts) != maxShownTexts || maxShownTexts != 50 {
		t.Fatalf("%d texts kept", len(st.ShownTexts))
	}
	for id := range batch {
		if st.ShownTexts[id] != "y" {
			t.Errorf("%s, just shown, was dropped", id)
		}
	}
	big := map[string]string{}
	for i := range 70 {
		big[fmt.Sprintf("big-%d", i)] = "z"
	}
	st.KeepShownTexts(big)
	if len(st.ShownTexts) != maxShownTexts {
		t.Errorf("a batch over the cap: %d texts kept", len(st.ShownTexts))
	}
}
