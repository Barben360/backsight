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
		PluginVersion: "0.1.0", ShimTarget: "/d/bin/0.1.0/backsight", ReplacedFrom: "ref-stack",
		FirstSessionNoticeShown: true,
		Backfill:                &Backfill{Done: 10, Total: 90, StartedAt: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)}}
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
