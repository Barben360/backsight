package state

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireWritesOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "locks", "maintenance.lock")
	l, err := TryAcquire(p, "backup create")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	var o Owner
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if o.PID != os.Getpid() || o.Command != "backup create" || o.Since.IsZero() {
		t.Errorf("%+v", o)
	}
}

func TestSecondAcquireIsHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, err := TryAcquire(p, "update apply")
	if err != nil {
		t.Fatal(err)
	}
	_, err = TryAcquire(p, "backup create")
	var held *HeldError
	if !errors.As(err, &held) || held.Owner.Command != "update apply" || held.Stale {
		t.Fatalf("want a live HeldError naming the owner, got %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := TryAcquire(p, "backup create")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = l2.Release()
}

func writeOwner(t *testing.T, p string, o Owner) {
	t.Helper()
	raw, _ := json.Marshal(o)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// A dead owner's lock is reported as stale and left in place (SPEC §11.4).
func TestStaleLockReportedNotStolen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "maintenance.lock")
	stale := []byte(`{"pid":` + itoa(deadPID(t)) + `,"command":"restore","since":"2026-10-24T08:00:00Z"}`)
	_ = os.WriteFile(p, stale, 0o600)
	_, err := TryAcquire(p, "backup create")
	var held *HeldError
	if !errors.As(err, &held) || !held.Stale || held.Owner.Command != "restore" {
		t.Fatalf("want a stale HeldError, got %v", err)
	}
	if raw, _ := os.ReadFile(p); string(raw) != string(stale) {
		t.Error("a stale lock must not be stolen or rewritten")
	}
}

func TestReleaseLeavesForeignLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, _ := TryAcquire(p, "a")
	foreign := []byte(`{"pid":1,"command":"other","since":"2026-10-24T08:00:00Z"}`)
	_ = os.WriteFile(p, foreign, 0o600) // replaced behind our back
	_ = l.Release()
	if raw, err := os.ReadFile(p); err != nil || string(raw) != string(foreign) {
		t.Error("Release must only remove a lock it still owns")
	}
}

func TestOnlyOneWinner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := TryAcquire(p, "race"); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d winners", wins)
	}
}

// A lock being written right now cannot be parsed yet: held, not stale.
func TestFreshUnreadableLockIsHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	_ = os.WriteFile(p, nil, 0o600)
	var held *HeldError
	if _, err := TryAcquire(p, "a"); !errors.As(err, &held) || held.Stale {
		t.Errorf("want held and not stale, got %v", err)
	}
}

// A crash between create and write leaves an empty lock: stale once old.
func TestOldUnreadableLockIsStale(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	_ = os.WriteFile(p, []byte("garbage"), 0o600)
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(p, old, old)
	var held *HeldError
	if _, err := TryAcquire(p, "a"); !errors.As(err, &held) || !held.Stale {
		t.Errorf("want a stale report, got %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("a stale lock is reported, never removed by TryAcquire")
	}
}

func TestBreakStale(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.lock")
	writeOwner(t, p, Owner{PID: deadPID(t), Command: "jobs tick", Since: time.Now().UTC()})
	_, err := TryAcquire(p, "a")
	var held *HeldError
	if !errors.As(err, &held) || !held.Stale {
		t.Fatalf("setup: %v", err)
	}
	if err := BreakStale(held); err != nil {
		t.Fatal(err)
	}
	l, err := TryAcquire(p, "a")
	if err != nil {
		t.Fatalf("after BreakStale the lock must be free: %v", err)
	}
	_ = l.Release()
}

func TestBreakStaleRefusesLiveOrChangedLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, _ := TryAcquire(p, "owner")
	_, err := TryAcquire(p, "a")
	var held *HeldError
	if !errors.As(err, &held) || held.Stale {
		t.Fatalf("setup: %v", err)
	}
	if err := BreakStale(held); err == nil {
		t.Error("a live lock must not be broken")
	}
	_ = l.Release()

	writeOwner(t, p, Owner{PID: deadPID(t), Command: "old", Since: time.Now().UTC()})
	_, err = TryAcquire(p, "a")
	if !errors.As(err, &held) || !held.Stale {
		t.Fatalf("setup: %v", err)
	}
	writeOwner(t, p, Owner{PID: os.Getpid(), Command: "new owner", Since: time.Now().UTC()})
	if err := BreakStale(held); err == nil {
		t.Error("a lock that changed since it was reported must not be broken")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("the new owner's lock was removed")
	}
}

func TestReleaseKeepsLockWithSamePIDButOtherSince(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	l, _ := TryAcquire(p, "a")
	writeOwner(t, p, Owner{PID: os.Getpid(), Command: "a", Since: time.Now().UTC().Add(time.Hour)})
	_ = l.Release()
	if _, err := os.Stat(p); err != nil {
		t.Error("a later lock of the same process was released")
	}
}

func TestHeldErrorMessages(t *testing.T) {
	e := &HeldError{Path: "/s/x.lock", Owner: Owner{PID: 42, Command: "backup create", Since: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)}}
	if m := e.Error(); !strings.Contains(m, "backup create") || !strings.Contains(m, "42") || strings.Contains(m, "gone") {
		t.Errorf("held: %q", m)
	}
	e.Stale = true
	if m := e.Error(); !strings.Contains(m, "gone") {
		t.Errorf("stale: %q", m)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// Acquire takes over a dead owner's lock and says so; a live owner's lock
// stays held and untouched.
func TestAcquireTakesOverOnlyAStaleLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job-backfill.lock")
	_ = os.WriteFile(p, []byte(`{"pid":`+itoa(deadPID(t))+`,"command":"retain backfill","since":"2026-10-24T08:00:00Z"}`), 0o600)
	l, broke, err := Acquire(p, "jobs tick")
	if err != nil || !broke {
		t.Fatalf("broke %v, err %v", broke, err)
	}
	if raw, _ := os.ReadFile(p); !strings.Contains(string(raw), `"jobs tick"`) {
		t.Errorf("lock now %s", raw)
	}
	_, broke, err = Acquire(p, "retain backfill")
	var held *HeldError
	if !errors.As(err, &held) || held.Stale || broke {
		t.Errorf("a live lock: broke %v, err %v", broke, err)
	}
	_ = l.Release()
	if l, broke, err := Acquire(p, "x"); err != nil || broke {
		t.Errorf("a free lock: broke %v, err %v", broke, err)
	} else {
		_ = l.Release()
	}
}
