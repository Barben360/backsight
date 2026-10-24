package state

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
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
