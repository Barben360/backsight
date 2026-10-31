//go:build windows

package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows has no execute bit: uv is there when uv.exe is a non-empty file.
func TestUVInstalledOnWindows(t *testing.T) {
	n := newNative(t, &fakeTools{})
	n.GOOS = "windows"
	if filepath.Base(n.UV()) != "uv.exe" {
		t.Fatalf("UV() = %s, want uv.exe", n.UV())
	}
	if n.uvInstalled() {
		t.Fatal("uv installed in an empty root")
	}
	touch(t, n.UV(), 0o644) // no x bit to read
	if !n.uvInstalled() {
		t.Error("uv.exe present but not installed")
	}
	if err := os.WriteFile(n.UV(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if n.uvInstalled() {
		t.Error("an empty uv.exe counts as installed")
	}
}
