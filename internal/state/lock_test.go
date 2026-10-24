package state

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
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

func TestUnreadableLockIsHeldNotStolen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	_ = os.WriteFile(p, []byte("garbage"), 0o600)
	var held *HeldError
	if _, err := TryAcquire(p, "a"); !errors.As(err, &held) {
		t.Errorf("a lock that cannot be parsed must be treated as held, got %v", err)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
