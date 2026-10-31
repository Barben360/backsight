package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// On Unix EnsurePrivate leaves everything as it is: no directory is
// created, no mode changed.
func TestEnsurePrivateDoesNothingOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has its own test")
	}
	h := t.TempDir()
	d := Dirs{filepath.Join(h, "data"), filepath.Join(h, "config"), filepath.Join(h, "state"), filepath.Join(h, "bin")}
	if err := os.Mkdir(d.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsurePrivate(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(d.Config); st.Mode().Perm() != 0o755 {
		t.Errorf("config mode %v changed", st.Mode().Perm())
	}
	if _, err := os.Stat(d.Data); err == nil {
		t.Error("data created")
	}
}
