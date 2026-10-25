package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
)

// pathLinkAt makes <bin>/backsight a link to target, as install.sh made
// it before decision 0067's block review (target: a versioned binary).
func pathLinkAt(t *testing.T, d paths.Dirs, target string) string {
	t.Helper()
	link := filepath.Join(d.Bin, "backsight")
	for _, dir := range []string{d.Bin, filepath.Dir(target)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(target); err != nil {
		if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func readlink(t *testing.T, p string) string {
	t.Helper()
	got, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return got
}

// client install points an older PATH link (to a versioned binary) at the
// shim: the PATH command then runs what update apply installs.
func TestClientInstallPointsAnOlderPathLinkAtTheShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH entry is a copy on Windows")
	}
	home := devInstall(t, true)
	withClaude(t)
	d := pathsOf(home)
	old := filepath.Join(d.Data, "bin", "0.0.9", "backsight")
	link := pathLinkAt(t, d, old)
	shim := filepath.Join(d.Data, "bin", "current")

	code, out, _ := run(t, "client", "install", "--plan", "--json")
	if code != output.OK || !strings.Contains(out, `"name": "path"`) || !strings.Contains(out, link+" → "+shim) || readlink(t, link) != old {
		t.Fatalf("plan: exit %d, link → %s\n%s", code, readlink(t, link), out)
	}
	code, out, _ = run(t, "client", "install", "--json")
	var env struct {
		Data clientInstallResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	stepped := false
	for _, s := range env.Data.Steps {
		stepped = stepped || (s.Name == "path" && s.Status == "done")
	}
	if code != output.OK || !stepped || readlink(t, link) != shim {
		t.Fatalf("exit %d, link → %s\n%s", code, readlink(t, link), out)
	}
	// Then nothing to do.
	code, out, _ = run(t, "client", "install", "--json")
	if code != output.OK || strings.Contains(out, `"name": "path"`) || readlink(t, link) != shim {
		t.Errorf("again: exit %d\n%s", code, out)
	}
}

// What is not install.sh's link to one of this install's binaries stays:
// a link elsewhere (a developer's build), a file, or none at all.
func TestClientInstallLeavesOtherPathEntriesAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH entry is a copy on Windows")
	}
	home := devInstall(t, true)
	withClaude(t)
	d := pathsOf(home)
	link := filepath.Join(d.Bin, "backsight")

	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatal(out)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("client install created a PATH entry install.sh did not make")
	}
	elsewhere := filepath.Join(t.TempDir(), "backsight")
	pathLinkAt(t, d, elsewhere)
	if code, out, _ := run(t, "client", "install"); code != output.OK || readlink(t, link) != elsewhere {
		t.Errorf("a link elsewhere was changed: exit %d → %s\n%s", code, readlink(t, link), out)
	}
	_ = os.Remove(link)
	if err := os.WriteFile(link, []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatal(out)
	}
	if raw, err := os.ReadFile(link); err != nil || string(raw) != "mine" {
		t.Errorf("a file on PATH was changed: %q %v", raw, err)
	}
}

// Pruning keeps the binary the PATH command runs, besides the shim's
// current and previous targets (install.sh's binary, before a client
// install could point its link at the shim).
func TestPruneNeverRemovesThePathTarget(t *testing.T) {
	data, bin := t.TempDir(), t.TempDir()
	dir := func(name string) string { return filepath.Join(data, "bin", name) }
	start := time.Now().Add(-time.Hour)
	for i, v := range []string{"0.0.1", "0.0.2", "0.0.3", "0.0.4", "0.0.5", "0.0.6", "0.0.7"} {
		if err := os.MkdirAll(dir(v), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir(v), "backsight"), nil, 0o755)
		at := start.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(dir(v), at, at)
	}
	if err := os.Symlink(filepath.Join(dir("0.0.1"), "backsight"), filepath.Join(bin, "backsight")); err != nil {
		t.Fatal(err)
	}
	pruneBinaries(data, bin, filepath.Join(dir("0.0.7"), "backsight"), filepath.Join(dir("0.0.6"), "backsight"))
	for _, v := range []string{"0.0.1", "0.0.3", "0.0.4", "0.0.5", "0.0.6", "0.0.7"} {
		if _, err := os.Stat(dir(v)); err != nil {
			t.Errorf("%s pruned", v)
		}
	}
	if _, err := os.Stat(dir("0.0.2")); err == nil {
		t.Error("0.0.2, the oldest unused, was kept")
	}
}

// update apply points an older PATH link at the shim itself: without
// Claude Code wired here, no client install runs to do it.
func TestUpdateApplyPointsAnOlderPathLinkAtTheShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH entry is a copy on Windows")
	}
	home, _, _ := installedAt(t, "0.1.0")
	d := pathsOf(home)
	link := pathLinkAt(t, d, shimOf(t, d))
	// Claude Code not wired: no plugin recorded.
	raw, _ := os.ReadFile(d.StateFile())
	var st map[string]any
	_ = json.Unmarshal(raw, &st)
	delete(st, "plugin_version")
	raw, _ = json.Marshal(st)
	_ = os.WriteFile(d.StateFile(), raw, 0o600)
	src := t.TempDir()
	signedRelease(t, src, "0.2.0", trustTestKey(t), relOpts{})
	code, out, errOut := run(t, "update", "apply", "--source", src, "--json")
	if code != output.OK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := readlink(t, link); got != filepath.Join(d.Data, "bin", "current") {
		t.Errorf("PATH link → %s", got)
	}
	if !strings.Contains(out, `"plugin": "skipped`) {
		t.Errorf("Claude Code was wired after all:\n%s", out)
	}
}
