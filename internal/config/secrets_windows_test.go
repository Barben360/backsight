//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/testtool"
)

// Windows has no mode bits: a secrets.toml the Users group can read is
// refused, naming the group and the fix.
func TestLoadSecretsRefusesAFileOtherAccountsCanReadOnWindows(t *testing.T) {
	p := writeSecrets(t, secretsTOML, 0o600)
	if _, err := LoadSecrets(p); err != nil {
		t.Fatalf("a private file refused: %v", err)
	}
	testtool.ShareWithOthers(t, p)
	_, err := LoadSecrets(p)
	if err == nil || !strings.Contains(err.Error(), "Users") || !strings.Contains(err.Error(), "icacls") || strings.Contains(err.Error(), "fakevalue") {
		t.Errorf("a file Users can read: %v", err)
	}
}

// The first secrets.toml creates a private config directory.
func TestUpdateSecretsMakesThePrivateConfigDirOnWindows(t *testing.T) {
	root, err := os.MkdirTemp(`C:\`, "bs-test-config-") // inherits read access for Users
	if err != nil {
		t.Skipf("cannot create a directory under C:\\: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	d := filepath.Join(root, "config")
	p := filepath.Join(d, "secrets.toml")
	if who, _ := fsutil.OtherReaders(root); len(who) == 0 {
		t.Fatal("the fixture's parent is private already: the test proves nothing")
	}
	if err := UpdateSecrets(p, func(s *Secrets) error { return s.Set("api_key", "fakevalue9") }); err != nil {
		t.Fatal(err)
	}
	for _, x := range []string{d, p} {
		if who, err := fsutil.OtherReaders(x); err != nil || len(who) != 0 {
			t.Errorf("%s readers %v, %v", x, who, err)
		}
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}

// A config directory that was there already and grants Users read: the
// written file is private anyway, and LoadSecrets reads it back.
func TestUpdateSecretsIntoALooseExistingDirOnWindows(t *testing.T) {
	root, err := os.MkdirTemp(`C:\`, "bs-test-config-")
	if err != nil {
		t.Skipf("cannot create a directory under C:\\: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if who, _ := fsutil.OtherReaders(root); len(who) == 0 {
		t.Fatal("the fixture directory is private already: the test proves nothing")
	}
	p := filepath.Join(root, "secrets.toml")
	if err := UpdateSecrets(p, func(s *Secrets) error { return s.Set("api_key", "fakevalue9") }); err != nil {
		t.Fatal(err)
	}
	if who, err := fsutil.OtherReaders(p); err != nil || len(who) != 0 {
		t.Errorf("readers of the written file: %v, %v", who, err)
	}
	s, err := LoadSecrets(p)
	if err != nil || s.APIKey != "fakevalue9" {
		t.Errorf("LoadSecrets: %v", err)
	}
}
