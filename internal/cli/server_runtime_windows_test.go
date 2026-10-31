//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The shim is current.exe: present when it is a non-empty .exe file, with
// no mode bit to read.
func TestShimPresentOnWindows(t *testing.T) {
	d := t.TempDir()
	shim := filepath.Join(d, "current.exe")
	if !shimMissing(shim) {
		t.Error("an absent shim is present")
	}
	if err := os.WriteFile(shim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !shimMissing(shim) {
		t.Error("an empty shim is present")
	}
	if err := os.WriteFile(shim, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if shimMissing(shim) {
		t.Error("a non-empty current.exe is missing")
	}
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	if !shimMissing(shim) {
		t.Error("a directory named current.exe is a shim")
	}
}
