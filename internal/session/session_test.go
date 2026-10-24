package session

import (
	"errors"
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
	l, err := state.TryAcquire(filepath.Join(s.Locks, "session-abc.lock"), "other hook")
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
	_ = os.WriteFile(filepath.Join(s.Locks, "session-abc.lock"), []byte(`{"pid":999999999,"command":"dead","since":"2026-10-24T09:00:00Z"}`), 0o600)
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
