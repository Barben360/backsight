package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// The snapshot of the files an update may change survives the process: a
// present file comes back byte for byte (an empty one too), an absent one
// as absent.
func TestUpdateSnapshotRoundTrip(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	files := map[string][]byte{"/x/state.json": []byte(`{"a":1}`), "/x/config.toml": nil, "/x/empty": {}}
	if err := WriteUpdateSnapshot(d, files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.UpdateSnapshot()); err != nil || filepath.Dir(d.UpdateSnapshot()) != d.State {
		t.Fatalf("%v", err)
	}
	testtool.RequirePrivate(t, d.UpdateSnapshot())
	got, err := ReadUpdateSnapshot(d)
	if err != nil || len(got) != 3 || string(got["/x/state.json"]) != `{"a":1}` || got["/x/config.toml"] != nil || got["/x/empty"] == nil {
		t.Fatalf("%q %v", got, err)
	}
	if err := ClearUpdateMarker(d); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadUpdateSnapshot(d); err != nil || got != nil {
		t.Errorf("the snapshot outlived the marker: %q %v", got, err)
	}
}

// UpdateIncomplete: the marker exists, readable or not.
func TestUpdateIncomplete(t *testing.T) {
	d := paths.Dirs{State: t.TempDir()}
	if UpdateIncomplete(d) {
		t.Error("no marker")
	}
	_ = os.WriteFile(d.UpdateMarker(), []byte("{not json"), 0o600)
	if !UpdateIncomplete(d) {
		t.Error("a corrupt marker is a marker")
	}
	if m, err := ReadUpdateMarker(d); m == nil || err == nil {
		t.Errorf("corrupt marker read as %+v %v", m, err)
	}
}
