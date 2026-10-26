package state

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

// The restore-in-progress marker (decision 0044 and 0067 block 1 review):
// written, read back, cleared; an unreadable one still counts as present.
func TestRestoreMarkerRoundTrip(t *testing.T) {
	d := paths.Dirs{State: filepath.Join(t.TempDir(), "state")}
	if m, err := ReadRestoreMarker(d); m != nil || err != nil || RestoreIncomplete(d) {
		t.Fatalf("no marker: %+v %v", m, err)
	}
	in := RestoreMarker{Command: "restore", Safety: "/b/backsight-safety.tar.gz.age", Archive: "/b/backsight-20261025.tar.gz.age",
		StartedAt: time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)}
	if err := WriteRestoreMarker(d, in); err != nil {
		t.Fatal(err)
	}
	if d.RestoreMarker() != filepath.Join(d.State, "restore-incomplete.json") {
		t.Errorf("path %s", d.RestoreMarker())
	}
	m, err := ReadRestoreMarker(d)
	if err != nil || m == nil || !reflect.DeepEqual(*m, in) || !RestoreIncomplete(d) {
		t.Fatalf("read back %+v %v", m, err)
	}
	raw, _ := os.ReadFile(d.RestoreMarker())
	for _, k := range []string{`"command"`, `"safety"`, `"archive"`, `"started_at"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("key %s missing: %s", k, raw)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(d.RestoreMarker()); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", fi.Mode().Perm())
		}
	}
	if err := ClearRestoreMarker(d); err != nil || RestoreIncomplete(d) {
		t.Fatalf("clear: %v", err)
	}
	if err := ClearRestoreMarker(d); err != nil {
		t.Errorf("clearing an absent marker: %v", err)
	}

	_ = os.WriteFile(d.RestoreMarker(), []byte("{torn"), 0o600)
	if m, err := ReadRestoreMarker(d); err == nil || m == nil || !RestoreIncomplete(d) {
		t.Errorf("a torn marker must still hold: %+v %v", m, err)
	}
}

// Another bank's restore epoch is not this bank's.
func TestEpochOfAnotherBankIsZero(t *testing.T) {
	s := State{RestoreEpoch: &RestoreEpoch{Bank: "other", Epoch: 4}}
	if s.EpochOf("claude-memory") != 0 || s.EpochOf("other") != 4 {
		t.Errorf("%d %d", s.EpochOf("claude-memory"), s.EpochOf("other"))
	}
}
