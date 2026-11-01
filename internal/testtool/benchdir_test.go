package testtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchTempDir is under BenchDirEnv when it is set, a new directory each
// call, gone once the test ends; unset, it is a test temp directory.
func TestBenchTempDirUsesTheBenchDisk(t *testing.T) {
	root := t.TempDir()
	var a, b string
	t.Run("set", func(t *testing.T) {
		t.Setenv(BenchDirEnv, root)
		a, b = BenchTempDir(t), BenchTempDir(t)
		for _, d := range []string{a, b} {
			if filepath.Dir(d) != root {
				t.Errorf("%s is not under %s", d, root)
			}
			if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
				t.Errorf("%s: %v", d, err)
			}
		}
		if a == b {
			t.Errorf("two calls gave one directory, %s", a)
		}
	})
	for _, d := range []string{a, b} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s outlived its test: %v", d, err)
		}
	}
	t.Setenv(BenchDirEnv, "")
	d := BenchTempDir(t)
	if strings.HasPrefix(d, root) {
		t.Errorf("unset, %s is still under %s", d, root)
	}
	// t.TempDir's directories share one parent per test.
	if want := filepath.Dir(t.TempDir()); filepath.Dir(d) != want {
		t.Errorf("unset, %s is not a test temp directory (under %s)", d, want)
	}
}
