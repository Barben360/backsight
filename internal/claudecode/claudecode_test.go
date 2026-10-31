package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLocateDefault(t *testing.T) {
	l := Locate("/home/u", env(nil))
	fs := filepath.FromSlash
	want := Location{Dir: fs("/home/u/.claude"), Settings: fs("/home/u/.claude/settings.json"), State: fs("/home/u/.claude.json"),
		ClaudeMD: fs("/home/u/.claude/CLAUDE.md"), Projects: fs("/home/u/.claude/projects")}
	if l != want {
		t.Errorf("got %+v\nwant %+v", l, want)
	}
}

// With CLAUDE_CONFIG_DIR, Claude Code 2.1.310 keeps .claude.json inside it.
func TestLocateConfigDirOverride(t *testing.T) {
	l := Locate("/home/u", env(map[string]string{"CLAUDE_CONFIG_DIR": "/x/cc"}))
	fs := filepath.FromSlash
	want := Location{Dir: "/x/cc", Settings: fs("/x/cc/settings.json"), State: fs("/x/cc/.claude.json"),
		ClaudeMD: fs("/x/cc/CLAUDE.md"), Projects: fs("/x/cc/projects")}
	if l != want {
		t.Errorf("got %+v\nwant %+v", l, want)
	}
}

func TestManagedDir(t *testing.T) {
	for goos, want := range map[string]string{"linux": "/etc/claude-code", "darwin": "/Library/Application Support/ClaudeCode",
		"windows": `C:\Program Files\ClaudeCode`} {
		if got := ManagedDir(goos); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

func TestReadRetention(t *testing.T) {
	cases := []struct {
		name, user, managed, dropIn string
		want                        Retention
	}{
		{"nothing set", "", "", "", Retention{EffectiveDays: 30, Source: "default"}},
		{"user file without the key", `{"model":"opus"}`, "", "", Retention{EffectiveDays: 30, Source: "default"}},
		{"user", `{"cleanupPeriodDays": 3650}`, "", "", Retention{EffectiveDays: 3650, Source: "user", UserDays: ptr(3650)}},
		{"managed wins", `{"cleanupPeriodDays": 3650}`, `{"cleanupPeriodDays": 14}`, "",
			Retention{EffectiveDays: 14, Source: "managed", UserDays: ptr(3650), ManagedDays: ptr(14)}},
		{"drop-in overrides managed file", "", `{"cleanupPeriodDays": 14}`, `{"cleanupPeriodDays": 90}`,
			Retention{EffectiveDays: 90, Source: "managed", ManagedDays: ptr(90)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			loc := Locate(filepath.Join(root, "home"), env(nil))
			managed := filepath.Join(root, "etc")
			if c.user != "" {
				put(t, loc.Settings, c.user)
			}
			if c.managed != "" {
				put(t, filepath.Join(managed, "managed-settings.json"), c.managed)
			}
			if c.dropIn != "" {
				put(t, filepath.Join(managed, "managed-settings.d", "50-retention.json"), c.dropIn)
			}
			got, err := ReadRetention(loc, managed)
			if err != nil {
				t.Fatal(err)
			}
			if !sameRetention(got, c.want) {
				t.Errorf("got %s\nwant %s", show(got), show(c.want))
			}
		})
	}
}

func TestReadRetentionCorruptSettingsNamesTheFile(t *testing.T) {
	loc := Locate(t.TempDir(), env(nil))
	put(t, loc.Settings, `{"cleanupPeriodDays": `)
	if _, err := ReadRetention(loc, t.TempDir()); err == nil || !strings.Contains(err.Error(), loc.Settings) {
		t.Errorf("want an error naming %s, got %v", loc.Settings, err)
	}
}

func TestScanTranscripts(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	put(t, filepath.Join(dir, "-home-u-a", "s1.jsonl"), "12345")
	put(t, filepath.Join(dir, "-home-u-b", "s2.jsonl"), "123")
	put(t, filepath.Join(dir, "-home-u-b", "s2", "subagents", "agent-x.jsonl"), "1234567") // not a session transcript
	put(t, filepath.Join(dir, "-home-u-b", "notes.txt"), "ignored")
	if err := os.Chtimes(filepath.Join(dir, "-home-u-b", "s2.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	got, err := ScanTranscripts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || got.Bytes != 8 || !got.Oldest.Equal(old) {
		t.Errorf("got %+v", got)
	}
}

func TestScanTranscriptsMissingDir(t *testing.T) {
	got, err := ScanTranscripts(filepath.Join(t.TempDir(), "projects"))
	if err != nil || got.Count != 0 || !got.Oldest.IsZero() {
		t.Errorf("got %+v, %v", got, err)
	}
}

func ptr(n int) *int { return &n }

func sameRetention(a, b Retention) bool {
	eq := func(x, y *int) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	return a.EffectiveDays == b.EffectiveDays && a.Source == b.Source && eq(a.UserDays, b.UserDays) && eq(a.ManagedDays, b.ManagedDays)
}

func show(r Retention) string {
	v := func(p *int) string {
		if p == nil {
			return "nil"
		}
		return strconv.Itoa(*p)
	}
	return fmt.Sprintf("%s %d user=%s managed=%s", r.Source, r.EffectiveDays, v(r.UserDays), v(r.ManagedDays))
}

// Drop-ins apply in name order, not in the order the files were created.
func TestDropInsApplyInNameOrder(t *testing.T) {
	root := t.TempDir()
	loc := Locate(filepath.Join(root, "home"), env(nil))
	managed := filepath.Join(root, "etc")
	put(t, filepath.Join(managed, "managed-settings.d", "90-z.json"), `{"cleanupPeriodDays": 90}`)
	put(t, filepath.Join(managed, "managed-settings.d", "10-a.json"), `{"cleanupPeriodDays": 10}`)
	got, err := ReadRetention(loc, managed)
	if err != nil || got.EffectiveDays != 90 {
		t.Errorf("want 90 from 90-z.json, got %+v %v", got, err)
	}
}

func TestCorruptManagedSettingsNamesTheFile(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "etc")
	p := filepath.Join(managed, "managed-settings.json")
	put(t, p, `{"cleanupPeriodDays": `)
	if _, err := ReadRetention(Locate(filepath.Join(root, "home"), env(nil)), managed); err == nil || !strings.Contains(err.Error(), p) {
		t.Errorf("want an error naming %s, got %v", p, err)
	}
}

// 0 is a value someone wrote, not "unset".
func TestZeroRetentionIsAValue(t *testing.T) {
	loc := Locate(t.TempDir(), env(nil))
	put(t, loc.Settings, `{"cleanupPeriodDays": 0}`)
	got, err := ReadRetention(loc, t.TempDir())
	if err != nil || got.Source != "user" || got.EffectiveDays != 0 || got.UserDays == nil {
		t.Errorf("%+v %v", got, err)
	}
}

// What the config editor accepts, retention reading accepts too: an empty
// file is {}, a BOM is skipped, a non-integer value counts as unset (M1
// review).
func TestReadRetentionToleratesWhatTheEditorAccepts(t *testing.T) {
	for _, c := range []struct {
		doc  string
		want int
	}{{"", 30}, {"  \n", 30}, {"\xef\xbb\xbf{\"cleanupPeriodDays\": 90}", 90}, {`{"cleanupPeriodDays": "90"}`, 30}, {`{"cleanupPeriodDays": 9.5}`, 30}, {`{"cleanupPeriodDays": null}`, 30}} {
		home := t.TempDir()
		loc := Locate(home, func(string) string { return "" })
		_ = os.MkdirAll(loc.Dir, 0o700)
		_ = os.WriteFile(loc.Settings, []byte(c.doc), 0o600)
		r, err := ReadRetention(loc, filepath.Join(home, "etc"))
		if err != nil || r.EffectiveDays != c.want {
			t.Errorf("%q: %+v %v", c.doc, r, err)
		}
	}
}
