package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

func TestLoadMissingIsEmptyCurrentSchema(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != SchemaVersion || len(s.AppliedMigrations) != 0 {
		t.Errorf("%+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "state.json")
	in := State{SchemaVersion: SchemaVersion, AppliedMigrations: []string{"0001"}, BankConfigHash: "abc",
		PluginVersion: "0.1.0", ShimTarget: "/d/bin/0.1.0/backsight", ReplacedFrom: "reference-stack",
		FirstSessionNoticeShown: true,
		Backfill:                &Backfill{Done: 10, Total: 90, StartedAt: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)},
		UpdateCheck: &UpdateCheck{CheckedAt: time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC), Source: "github:Barben360/backsight",
			Installed: "0.1.0", Latest: "0.2.0", Summary: "✨ Add update check", Newer: 1, Error: "unreachable"}}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("got %+v\nwant %+v", out, in)
	}
	raw, _ := os.ReadFile(p)
	for _, key := range []string{`"schema_version"`, `"applied_migrations"`, `"bank_config_hash"`, `"plugin_version"`, `"shim_target"`, `"backfill"`, `"done"`, `"total"`, `"started_at"`, `"replaced_from"`, `"first_session_notice_shown"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("Appendix A.3 key %s missing from %s", key, raw)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", fi.Mode().Perm())
		}
	}
}

func TestLoadRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(p, []byte(`{"schema_version": 99}`), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "newer backsight") {
		t.Errorf("got %v", err)
	}
}

func TestLoadRefusesCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(p, []byte(`{"schema_version": 1,`), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Errorf("want an error naming the file, got %v", err)
	}
}

func TestNilSlicesSavedAsEmptyArrays(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := Save(p, State{SchemaVersion: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"applied_migrations": []`) {
		t.Errorf("%s", raw)
	}
}

// A newer minor version may add fields; saving must not drop them.
func TestUnknownFieldsSurviveSave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(p, []byte(`{"schema_version": 1, "plugin_version": "0.1.0", "future": {"a": [1, 2]}}`), 0o600)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s.PluginVersion = "0.2.0"
	if err := Save(p, s); err != nil {
		t.Fatal(err)
	}
	var back map[string]json.RawMessage
	raw, _ := os.ReadFile(p)
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	var future struct{ A []int }
	if string(back["plugin_version"]) != `"0.2.0"` || json.Unmarshal(back["future"], &future) != nil || len(future.A) != 2 {
		t.Errorf("known field not updated or unknown field lost: %s", raw)
	}
}

func TestUpdateUnderLock(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	if err := Update(d, "test", func(s *State) error { s.PluginVersion = "0.1.0"; return nil }); err != nil {
		t.Fatal(err)
	}
	s, _ := Load(d.StateFile())
	if s.PluginVersion != "0.1.0" {
		t.Errorf("not persisted: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(d.LocksDir(), "state.lock")); err == nil {
		t.Error("the lock must be released after the update")
	}

	l, _ := TryAcquire(filepath.Join(d.LocksDir(), "state.lock"), "other")
	defer func() { _ = l.Release() }()
	var held *HeldError
	if err := Update(d, "test", func(s *State) error { s.PluginVersion = "lost"; return nil }); !errors.As(err, &held) {
		t.Errorf("want HeldError while another process holds the lock, got %v", err)
	}
	if s, _ := Load(d.StateFile()); s.PluginVersion != "0.1.0" {
		t.Errorf("state changed without the lock: %+v", s)
	}
}

func TestUpdateCallbackErrorWritesNothing(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	boom := errors.New("boom")
	if err := Update(d, "test", func(s *State) error { s.PluginVersion = "x"; return boom }); !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
	if _, err := os.Stat(d.StateFile()); err == nil {
		t.Error("state.json written despite the callback error")
	}
}

// UpdateWithin waits for a held state lock up to its bound, then gives up
// with the HeldError; fn's own error is never retried.
func TestUpdateWithinWaitsForTheLock(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	l, err := TryAcquire(filepath.Join(d.LocksDir(), "state.lock"), "other")
	if err != nil {
		t.Fatal(err)
	}
	go func(l *Lock) { time.Sleep(150 * time.Millisecond); _ = l.Release() }(l) // its own l: the test reuses the variable
	if err := UpdateWithin(d, "test", 5*time.Second, func(s *State) error { s.PluginVersion = "0.2.0"; return nil }); err != nil {
		t.Fatalf("not retried until the lock was free: %v", err)
	}
	if s, _ := Load(d.StateFile()); s.PluginVersion != "0.2.0" {
		t.Errorf("not written: %+v", s)
	}

	// The lock is free again once UpdateWithin returned: taking it must
	// succeed, or the bound below would be tested against no holder.
	l2, err := TryAcquire(filepath.Join(d.LocksDir(), "state.lock"), "other")
	if err != nil {
		t.Fatalf("the lock was not released after the update: %v", err)
	}
	defer func() { _ = l2.Release() }()
	start := time.Now()
	var held *HeldError
	if err := UpdateWithin(d, "test", 200*time.Millisecond, func(s *State) error { s.PluginVersion = "lost"; return nil }); !errors.As(err, &held) || time.Since(start) < 200*time.Millisecond {
		t.Errorf("a lock held past the bound: %v after %s", err, time.Since(start))
	}
	_ = l2.Release()
	calls := 0
	boom := errors.New("boom")
	if err := UpdateWithin(d, "test", time.Second, func(*State) error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Errorf("fn's error: %v, %d calls", err, calls)
	}
}

// SaveWithin writes the raw state only under the state lock.
func TestSaveWithinTakesTheLock(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	l, err := TryAcquire(filepath.Join(d.LocksDir(), "state.lock"), "other")
	if err != nil {
		t.Fatal(err)
	}
	var held *HeldError
	if err := SaveWithin(d, "test", 100*time.Millisecond, []byte(`{"plugin_version":"x"}`)); !errors.As(err, &held) {
		t.Errorf("written under someone else's lock: %v", err)
	}
	if _, err := os.Stat(d.StateFile()); err == nil {
		t.Error("state.json written without the lock")
	}
	go func() { time.Sleep(100 * time.Millisecond); _ = l.Release() }()
	if err := SaveWithin(d, "test", 5*time.Second, []byte(`{"plugin_version":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(d.StateFile()); string(raw) != `{"plugin_version":"x"}` {
		t.Errorf("%s", raw)
	}
	if _, err := os.Stat(filepath.Join(d.LocksDir(), "state.lock")); err == nil {
		t.Error("lock left")
	}
}

// The look for recreated purged documents survives a save and a load
// whole, beside fields a newer minor version wrote.
func TestPurgeGuardRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	at := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	g := &PurgeGuard{Bank: "claude-memory", Newest: at, FullAt: at, Tombstones: 4, CheckedAt: at,
		Waiting: []Recreated{{Document: "conversation:s1", Session: "s1", Project: "billing", Why: "consent", NeedsUser: true}},
		Purged:  []string{"conversation:s3"}, PurgedAt: at, Repeats: []Repurge{{Document: "conversation:s3", Count: 2, Since: at}}}
	raw, _ := json.Marshal(map[string]any{"schema_version": SchemaVersion, "purge_guard": g, "from_a_newer_minor": true})
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil || !reflect.DeepEqual(st.PurgeGuard, g) {
		t.Fatalf("%+v %v", st.PurgeGuard, err)
	}
	if err := Save(p, st); err != nil {
		t.Fatal(err)
	}
	back, _ := os.ReadFile(p)
	if again, _ := Load(p); !reflect.DeepEqual(again.PurgeGuard, g) || !strings.Contains(string(back), "from_a_newer_minor") {
		t.Errorf("%s", back)
	}
}
