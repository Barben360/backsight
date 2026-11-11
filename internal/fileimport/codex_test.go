package fileimport

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

const codexFixtures = "../testdata/codex/0.167.0/memories"

// The thread ids of the recorded stores' summaries.
const (
	v2NFaA = "01a1c747-a22f-7a40-9fbe-74c9c7c099cc" // memories_v2/rollout_summaries/2026-11-10T21-05-22-NFaA-fake_slug.md
)

var codexMtime = time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)

// codexHome is a Codex home holding the recorded stores of variant (v1,
// v2, dual), with what the recording lists but did not copy (.git/,
// .codex-baseline/), every file dated codexMtime.
func codexHome(t *testing.T, variant string) string {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join(codexFixtures, variant)
	for _, store := range CodexStores {
		from := filepath.Join(src, store)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(from, p)
			to := filepath.Join(home, store, rel)
			if d.IsDir() {
				return os.MkdirAll(to, 0o755)
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(to, raw, 0o644); err != nil {
				return err
			}
			return os.Chtimes(to, codexMtime, codexMtime)
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{".git/HEAD", ".codex-baseline/.git/codex-baseline.lock"} {
			writeFile(t, filepath.Join(home, store, f), "x")
		}
	}
	return home
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rolloutFor puts a rollout of thread under home's sessions/, as Codex
// names it.
func rolloutFor(t *testing.T, home, thread, ext string) {
	t.Helper()
	writeFile(t, filepath.Join(home, "sessions", "2026", "11", "10", "rollout-2026-11-10T21-05-23-"+thread+ext), "{}\n")
}

func fileOf(t *testing.T, s Scan, rel string) File {
	t.Helper()
	for _, f := range s.Files {
		if f.Rel == rel {
			return f
		}
	}
	t.Fatalf("%s not sent; files %v, skipped %v", rel, rels(s.Files), s.Skipped)
	return File{}
}

func TestScanCodexV2(t *testing.T) {
	home := codexHome(t, "v2")
	rolloutFor(t, home, v2NFaA, ".jsonl") // the first thread's conversation is still on disk
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"memories_v2/memory_summary.md",
		"memories_v2/rollout_summaries/2026-11-10T21-05-31-3lru-fake_slug.md",
		"memories_v2/rollout_summaries/2026-11-10T21-05-40-fhUb-fake_slug.md",
	}
	if !reflect.DeepEqual(rels(s.Files), want) {
		t.Fatalf("sent %v, want %v; skipped %v", rels(s.Files), want, s.Skipped)
	}
	if s.Preset != PresetCodex || !reflect.DeepEqual(s.Stores, []string{"memories_v2"}) {
		t.Errorf("preset %q stores %v", s.Preset, s.Stores)
	}
	// The consolidated file: the import's project, dated by its mtime.
	if f := fileOf(t, s, want[0]); f.Project != "" || !f.DateFromMtime || !f.Date.Equal(codexMtime) {
		t.Errorf("memory_summary.md = project %q date %v (mtime %v)", f.Project, f.Date, f.DateFromMtime)
	}
	// A summary: the project of its cwd (/home/tester/work), its updated_at.
	if f := fileOf(t, s, want[1]); f.Project != "work" || f.DateFromMtime || !f.Date.Equal(time.Date(2026, 11, 10, 19, 5, 33, 0, time.UTC)) {
		t.Errorf("summary = project %q date %v (mtime %v)", f.Project, f.Date, f.DateFromMtime)
	}
	wantSkipped := map[string][]string{
		SkipRolloutOnDisk: {"memories_v2/rollout_summaries/2026-11-10T21-05-22-NFaA-fake_slug.md"},
		SkipCodexWorking:  {"memories_v2/extensions/ad_hoc/instructions.md"},
	}
	if !reflect.DeepEqual(s.Skipped, wantSkipped) {
		t.Errorf("skipped = %v, want %v", s.Skipped, wantSkipped)
	}
	if !reflect.DeepEqual(s.SkippedDirs, map[string][]string{SkipHidden: {"memories_v2/.codex-baseline", "memories_v2/.git"}}) {
		t.Errorf("skipped folders = %v", s.SkippedDirs)
	}
}

func TestScanCodexV1(t *testing.T) {
	home := codexHome(t, "v1")
	writeFile(t, filepath.Join(home, "memories", "skills", "deploy", "SKILL.md"), "a skill")
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"memories/MEMORY.md",
		"memories/memory_summary.md",
		"memories/rollout_summaries/2026-11-10T21-06-22-1qW9-fake_slug.md",
		"memories/rollout_summaries/2026-11-10T21-06-30-kwGL-fake_slug.md",
		"memories/rollout_summaries/2026-11-10T21-06-39-9BVv-fake_slug.md",
	}
	if !reflect.DeepEqual(rels(s.Files), want) {
		t.Fatalf("sent %v, want %v; skipped %v", rels(s.Files), want, s.Skipped)
	}
	if f := fileOf(t, s, "memories/MEMORY.md"); f.Project != "" || !f.Date.Equal(codexMtime) {
		t.Errorf("MEMORY.md = project %q date %v", f.Project, f.Date)
	}
	if got := s.Skipped[SkipCodexWorking]; !reflect.DeepEqual(got, []string{"memories/extensions/ad_hoc/instructions.md", "memories/raw_memories.md"}) {
		t.Errorf("working files skipped = %v", got)
	}
	if got := s.SkippedDirs[SkipCodexSkills]; !reflect.DeepEqual(got, []string{"memories/skills"}) {
		t.Errorf("skills skipped = %v (all folders %v)", got, s.SkippedDirs)
	}
}

func TestScanCodexDualSendsEachFileOnce(t *testing.T) {
	home := codexHome(t, "dual")
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"memories/MEMORY.md", // v1 only
		"memories_v2/memory_summary.md",
		"memories_v2/rollout_summaries/2026-11-10T21-06-21-W0BO-fake_slug.md",
		"memories_v2/rollout_summaries/2026-11-10T21-06-30-ykZG-fake_slug.md",
		"memories_v2/rollout_summaries/2026-11-10T21-06-38-Ge6z-fake_slug.md",
	}
	if !reflect.DeepEqual(rels(s.Files), want) {
		t.Fatalf("sent %v, want %v", rels(s.Files), want)
	}
	wantDup := []string{
		"memories/memory_summary.md",
		"memories/rollout_summaries/2026-11-10T21-06-21-W0BO-fake_slug.md",
		"memories/rollout_summaries/2026-11-10T21-06-30-ykZG-fake_slug.md",
		"memories/rollout_summaries/2026-11-10T21-06-38-Ge6z-fake_slug.md",
	}
	if !reflect.DeepEqual(s.Skipped[SkipDuplicate], wantDup) {
		t.Errorf("duplicates = %v, want %v", s.Skipped[SkipDuplicate], wantDup)
	}
	if !reflect.DeepEqual(s.Stores, []string{"memories_v2", "memories"}) {
		t.Errorf("stores = %v", s.Stores)
	}
}

func TestScanCodexSameThreadKeepsTheNewerSummary(t *testing.T) {
	home := t.TempDir()
	head := func(at string) string {
		return "thread_id: 01a1c748-85f2-7011-b1ee-74ef2a380048\nupdated_at: " + at + "\nrollout_path: /nowhere/r.jsonl\ncwd: /src/app\n\nsummary " + at + "\n"
	}
	writeFile(t, filepath.Join(home, "memories_v2", "rollout_summaries", "a.md"), head("2026-11-10T19:00:00+00:00"))
	writeFile(t, filepath.Join(home, "memories", "rollout_summaries", "a.md"), head("2026-11-11T08:00:00+00:00"))
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rels(s.Files), []string{"memories/rollout_summaries/a.md"}) ||
		!reflect.DeepEqual(s.Skipped[SkipDuplicate], []string{"memories_v2/rollout_summaries/a.md"}) {
		t.Errorf("sent %v, duplicates %v: want the newer v1 copy", rels(s.Files), s.Skipped[SkipDuplicate])
	}
}

func TestScanCodexRolloutStillOnDisk(t *testing.T) {
	for _, tc := range []struct{ name, ext string }{{"at its path", ""}, {"compressed in place", ".zst"}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			rollout := filepath.Join(t.TempDir(), "rollout-x.jsonl")
			writeFile(t, rollout+tc.ext, "{}")
			writeFile(t, filepath.Join(home, "memories_v2", "rollout_summaries", "a.md"),
				"thread_id: 01a1c748-85f2-7011-b1ee-74ef2a380048\nupdated_at: 2026-11-10T19:00:00+00:00\nrollout_path: "+rollout+"\ncwd: /src/app\n\nbody\n")
			s, err := ScanCodex(home, CodexOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Files) != 0 || len(s.Skipped[SkipRolloutOnDisk]) != 1 {
				t.Errorf("sent %v, skipped %v: the rollout is on disk", rels(s.Files), s.Skipped)
			}
			if err := os.Remove(rollout + tc.ext); err != nil {
				t.Fatal(err)
			}
			if s, _ = ScanCodex(home, CodexOptions{}); len(s.Files) != 1 {
				t.Errorf("the rollout gone: sent %v, skipped %v", rels(s.Files), s.Skipped)
			}
		})
	}
}

func TestScanCodexProjectFromCWD(t *testing.T) {
	home := codexHome(t, "v2")
	var asked []string
	s, err := ScanCodex(home, CodexOptions{Project: func(cwd string) string { asked = append(asked, cwd); return "proj-of-" + filepath.Base(cwd) }})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range s.Files {
		if strings.Contains(f.Rel, "rollout_summaries/") {
			n++
			if f.Project != "proj-of-work" {
				t.Errorf("%s: project %q", f.Rel, f.Project)
			}
		} else if f.Project != "" {
			t.Errorf("%s: project %q, want the import's", f.Rel, f.Project)
		}
	}
	if n != 3 || len(asked) != 3 || asked[0] != "/home/tester/work" {
		t.Errorf("summaries %d, cwds asked %v", n, asked)
	}
}

func TestScanCodexKeepsOutExcludedFolders(t *testing.T) {
	out := func(cwd string) bool { return cwd == "/home/tester/work" }
	t.Run("summary", func(t *testing.T) {
		home := codexHome(t, "v2")
		s, err := ScanCodex(home, CodexOptions{OptedOut: out})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Files) != 0 || len(s.Skipped[SkipOptedOut]) != 3 {
			t.Errorf("sent %v, kept out %v", rels(s.Files), s.Skipped[SkipOptedOut])
		}
		// The consolidated file draws on the kept-out threads.
		if !reflect.DeepEqual(s.Skipped[SkipMixesExcluded], []string{"memories_v2/memory_summary.md"}) {
			t.Errorf("mixes = %v", s.Skipped[SkipMixesExcluded])
		}
	})
	t.Run("raw memories", func(t *testing.T) {
		home := codexHome(t, "v1")
		// No summary left for the thread: raw_memories.md still names its cwd.
		if err := os.RemoveAll(filepath.Join(home, "memories", "rollout_summaries")); err != nil {
			t.Fatal(err)
		}
		s, err := ScanCodex(home, CodexOptions{OptedOut: out})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Files) != 0 || !reflect.DeepEqual(s.Skipped[SkipMixesExcluded], []string{"memories/MEMORY.md", "memories/memory_summary.md"}) {
			t.Errorf("sent %v, mixes %v", rels(s.Files), s.Skipped[SkipMixesExcluded])
		}
	})
	t.Run("other folder", func(t *testing.T) {
		home := codexHome(t, "v1")
		s, _ := ScanCodex(home, CodexOptions{OptedOut: func(cwd string) bool { return cwd == "/elsewhere" }})
		if len(s.Files) != 5 || len(s.Skipped[SkipMixesExcluded]) != 0 || len(s.KeptOut) != 0 {
			t.Errorf("sent %v, mixes %v, kept out %v", rels(s.Files), s.Skipped[SkipMixesExcluded], s.KeptOut)
		}
	})
	t.Run("one store names it, both stores' consolidated files stay", func(t *testing.T) {
		home := codexHome(t, "dual")
		raw := filepath.Join(home, "memories", "raw_memories.md")
		b, err := os.ReadFile(raw)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, raw, string(b)+"\n## Thread `01a1c748-0000-7000-8000-000000000000`\nupdated_at: 2026-11-10T19:06:23+00:00\ncwd: /secret\nrollout_path: /gone.jsonl\n\nsecret work\n")
		s, err := ScanCodex(home, CodexOptions{OptedOut: func(cwd string) bool { return cwd == "/secret" }})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"memories/MEMORY.md", "memories/memory_summary.md", "memories_v2/memory_summary.md"}
		if got := s.Skipped[SkipMixesExcluded]; !reflect.DeepEqual(got, want) {
			t.Errorf("mixes = %v, want %v (sent %v)", got, want, rels(s.Files))
		}
		for _, f := range s.Files {
			if isCodexConsolidated(f.Rel) {
				t.Errorf("%s sent", f.Rel)
			}
		}
		if !reflect.DeepEqual(s.KeptOut, []string{"/secret"}) {
			t.Errorf("kept out = %v", s.KeptOut)
		}
	})
	// A summary Codex wrote badly still names its folder: it counts.
	for name, content := range map[string]string{
		"bad header": "thread_id: ../x\nupdated_at: yesterday\ncwd: /secret\n\nsecret work\n",
		"too large":  "thread_id: 01a1c748-85f2-7011-b1ee-74ef2a380048\nupdated_at: 2026-11-10T19:00:00+00:00\nrollout_path: /gone\ncwd: /secret\n\n" + strings.Repeat("x", 1<<20),
		"not text":   "thread_id: a\x00\ncwd: /secret\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, filepath.Join(home, "memories_v2", "memory_summary.md"), "v1\n## User Profile\n")
			writeFile(t, filepath.Join(home, "memories_v2", "rollout_summaries", "a.md"), content)
			s, err := ScanCodex(home, CodexOptions{OptedOut: func(cwd string) bool { return cwd == "/secret" }})
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Files) != 0 || !reflect.DeepEqual(s.Skipped[SkipMixesExcluded], []string{"memories_v2/memory_summary.md"}) {
				t.Errorf("sent %v, skipped %v", rels(s.Files), s.Skipped)
			}
		})
	}
	t.Run("a network cwd is never looked at", func(t *testing.T) {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, "memories_v2", "rollout_summaries", "a.md"), "cwd: //host/share\n\nx\n")
		var asked []string
		CodexKeptOut(home, func(cwd string) bool { asked = append(asked, cwd); return true })
		if len(asked) != 0 {
			t.Errorf("asked about %v", asked)
		}
	})
}

func TestScanCodexUntrustedFiles(t *testing.T) {
	home := t.TempDir()
	sum := func(lines ...string) string { return strings.Join(lines, "\n") + "\n\nbody\n" }
	ok := []string{"thread_id: 01a1c748-85f2-7011-b1ee-74ef2a380048", "updated_at: 2026-11-10T19:00:00+00:00", "rollout_path: /gone/r.jsonl", "cwd: /src/app"}
	files := map[string]string{
		"rollout_summaries/good.md":                               sum(ok...),
		"rollout_summaries/no-date.md":                            sum(ok[0], ok[2], ok[3]),
		"rollout_summaries/bad-date.md":                           sum(ok[0], "updated_at: yesterday", ok[2], ok[3]),
		"rollout_summaries/relative-cwd.md":                       sum(ok[0], ok[1], ok[2], "cwd: src/app"),
		"rollout_summaries/unc-rollout.md":                        sum(ok[0], ok[1], `rollout_path: \\host\share\r.jsonl`, ok[3]),
		"rollout_summaries/two-cwds.md":                           sum(ok[0], ok[1], ok[2], ok[3], "cwd: /other"),
		"rollout_summaries/bad-thread.md":                         sum("thread_id: ../../etc", ok[1], ok[2], ok[3]),
		"rollout_summaries/net-cwd-slashes.md":                    sum(ok[0], ok[1], ok[2], "cwd: //host/share/src"),
		"rollout_summaries/net-cwd-mixed.md":                      sum(ok[0], ok[1], ok[2], `cwd: /\host\share\src`),
		"rollout_summaries/net-cwd-mixed2.md":                     sum(ok[0], ok[1], ok[2], `cwd: \/host\share\src`),
		"rollout_summaries/device-rollout.md":                     sum(ok[0], ok[1], `rollout_path: \\?\C:\r.jsonl`, ok[3]),
		"rollout_summaries/pipe-cwd.md":                           sum(ok[0], ok[1], ok[2], `cwd: \\.\pipe\x`),
		"rollout_summaries/no-header.md":                          "just a summary\n",
		"rollout_summaries/not-md.txt":                            "x",
		"rollout_summaries/deeper/x.md":                           sum(ok...),
		"extensions/ad_hoc/resources/2026-11-10T21-00-00-note.md": "a user note",
		"notes.txt":                    "not Codex's",
		"memory_summary.md":            "v1\n## User Profile\n",
		"rollout_summaries/binary.md":  "thread_id: a\x00b\n",
		"rollout_summaries/invalid.md": "\xff\xfe",
	}
	for rel, c := range files {
		writeFile(t, filepath.Join(home, "memories_v2", filepath.FromSlash(rel)), c)
	}
	writeFile(t, filepath.Join(home, "memories_v2", "rollout_summaries", "huge.md"), sum(ok...)+strings.Repeat("x", 1<<20))
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(home, "memories_v2", "rollout_summaries", "good.md"), filepath.Join(home, "memories_v2", "rollout_summaries", "link.md")); err != nil {
			t.Fatal(err)
		}
	}
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rels(s.Files), []string{"memories_v2/memory_summary.md", "memories_v2/rollout_summaries/good.md"}) {
		t.Errorf("sent %v", rels(s.Files))
	}
	p := func(names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, "memories_v2/"+n)
		}
		slices.Sort(out)
		return out
	}
	want := map[string][]string{
		SkipBadHeader: p("rollout_summaries/no-date.md", "rollout_summaries/bad-date.md", "rollout_summaries/relative-cwd.md",
			"rollout_summaries/unc-rollout.md", "rollout_summaries/two-cwds.md", "rollout_summaries/bad-thread.md", "rollout_summaries/no-header.md",
			"rollout_summaries/net-cwd-slashes.md", "rollout_summaries/net-cwd-mixed.md", "rollout_summaries/net-cwd-mixed2.md",
			"rollout_summaries/device-rollout.md", "rollout_summaries/pipe-cwd.md"),
		SkipCodexUnknown: p("rollout_summaries/not-md.txt", "notes.txt"),
		SkipCodexNotes:   p("extensions/ad_hoc/resources/2026-11-10T21-00-00-note.md"),
		SkipNotText:      p("rollout_summaries/binary.md", "rollout_summaries/invalid.md"),
		SkipTooLarge:     p("rollout_summaries/huge.md"),
	}
	if runtime.GOOS != "windows" {
		want[SkipSymlink] = p("rollout_summaries/link.md")
	}
	if !reflect.DeepEqual(s.Skipped, want) {
		t.Errorf("skipped =\n%v\nwant\n%v", s.Skipped, want)
	}
	if !reflect.DeepEqual(s.SkippedDirs[SkipCodexUnknown], p("rollout_summaries/deeper")) {
		t.Errorf("unknown folders = %v", s.SkippedDirs)
	}
}

func TestScanCodexNoStoreAndExcludes(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.toml"), "")
	writeFile(t, filepath.Join(home, "sessions", "2026", "11", "10", "rollout-x.jsonl"), "{}")
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil || len(s.Files) != 0 || len(s.Stores) != 0 || len(s.Skipped) != 0 {
		t.Errorf("a home with no store: %+v %v (its other files are not the import's)", s, err)
	}
	home = codexHome(t, "v1")
	s, _ = ScanCodex(home, CodexOptions{Exclude: []string{"memories/rollout_summaries/**"}})
	if !reflect.DeepEqual(rels(s.Files), []string{"memories/MEMORY.md", "memories/memory_summary.md"}) || len(s.Skipped[SkipExcluded]) != 3 {
		t.Errorf("sent %v, excluded %v", rels(s.Files), s.Skipped[SkipExcluded])
	}
	if _, err := ScanCodex(filepath.Join(home, "absent"), CodexOptions{}); err == nil {
		t.Error("an absent home gave no error")
	}
}

func TestScanCodexRedactsSecrets(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "memories_v2", "memory_summary.md"), "v1\nkey sk-ant-api03-"+strings.Repeat("A", 90)+"\n")
	s, err := ScanCodex(home, CodexOptions{})
	if err != nil || len(s.Files) != 1 {
		t.Fatalf("scan %+v %v", s, err)
	}
	if len(s.Files[0].Redacted) == 0 {
		t.Errorf("no redaction counted: %+v", s.Files[0])
	}
}

func TestParseCodexSummaryHeader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(codexFixtures, "v2", "memories_v2", "rollout_summaries", "2026-11-10T21-05-22-NFaA-fake_slug.md"))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := ParseCodexSummary(raw)
	want := CodexSummary{ThreadID: v2NFaA, UpdatedAt: time.Date(2026, 11, 10, 19, 5, 24, 0, time.UTC),
		RolloutPath: "/home/tester/.codex/sessions/2026/11/10/rollout-2026-11-10T21-05-23-" + v2NFaA + ".jsonl", CWD: "/home/tester/work"}
	if !ok || s != want {
		t.Errorf("parse = %+v %v, want %+v", s, ok, want)
	}
	// A header line past the first blank line is the body's, not the header's.
	if _, ok := ParseCodexSummary([]byte("thread_id: a\n\nupdated_at: 2026-11-10T19:05:24Z\nrollout_path: /r\ncwd: /c\n")); ok {
		t.Error("a header ending at the blank line still read the body")
	}
	if s, ok := ParseCodexSummary([]byte("thread_id: a\r\nupdated_at: 2026-11-10T19:05:24Z\r\nrollout_path: /r\r\ncwd: /c\r\n\r\nx")); !ok || s.CWD != "/c" {
		t.Errorf("CRLF header = %+v %v", s, ok)
	}
}

func TestCodexRecheck(t *testing.T) {
	raw := []byte("thread_id: a\nupdated_at: 2026-11-10T19:05:24Z\nrollout_path: /r\ncwd: /c\n\nx")
	if d, dated, reason := CodexRecheck("memories_v2/rollout_summaries/a.md", raw, nil, nil); !dated || reason != "" || !d.Equal(time.Date(2026, 11, 10, 19, 5, 24, 0, time.UTC)) {
		t.Errorf("recheck = %v %v %q", d, dated, reason)
	}
	if _, _, reason := CodexRecheck("memories_v2/rollout_summaries/a.md", raw, func(cwd string) bool { return cwd == "/c" }, nil); reason != SkipOptedOut {
		t.Errorf("kept-out folder: reason %q", reason)
	}
	if _, _, reason := CodexRecheck("memories_v2/rollout_summaries/a.md", []byte("no header"), nil, nil); reason != SkipBadHeader {
		t.Errorf("bad header: reason %q", reason)
	}
	if _, dated, reason := CodexRecheck("memories_v2/memory_summary.md", []byte("v1"), nil, func() bool { return false }); dated || reason != "" {
		t.Errorf("consolidated file: %v %q", dated, reason)
	}
	// The home names a kept-out folder since the queue: the consolidated
	// files stay home, a summary is not asked about the home.
	for _, rel := range []string{"memories_v2/memory_summary.md", "memories/MEMORY.md"} {
		if _, _, reason := CodexRecheck(rel, []byte("v1"), nil, func() bool { return true }); reason != SkipMixesExcluded {
			t.Errorf("%s with a kept-out folder: reason %q", rel, reason)
		}
	}
	if _, _, reason := CodexRecheck("memories_v2/rollout_summaries/a.md", raw, nil, func() bool { return true }); reason != "" {
		t.Errorf("summary with a kept-out folder elsewhere: reason %q", reason)
	}
}

func TestLocalAbsRefusesNetworkAndDevicePaths(t *testing.T) {
	// The same answer on every OS: a header is read the same wherever it
	// was written.
	for p, want := range map[string]bool{
		"/home/u/src":                   true,
		"//host/share/x":                false,
		`/\host\share\x`:                false,
		`\/host\share\x`:                false,
		`\\host\share\x`:                false,
		`\\?\C:\x`:                      false,
		`\\.\pipe\x`:                    false,
		"src/app":                       false,
		"":                              false,
		"/a\x00b":                       false,
		"/" + strings.Repeat("a", 4096): false,
	} {
		if got := localAbs(p); got != want {
			t.Errorf("localAbs(%q) = %v, want %v", p, got, want)
		}
	}
	if runtime.GOOS == "windows" && !localAbs(`C:\Users\u\src`) {
		t.Error("a drive path is refused")
	}
}

// An entry sent before entries recorded the project of their send (no
// SentProject): a move still remembers the project it was sent to.
func TestMergeMovesAnEntryWithoutSentProject(t *testing.T) {
	m := Manifest{Source: "s", Project: "home", Preset: PresetCodex,
		Entries: []Entry{{Rel: "memories_v2/rollout_summaries/a.md", SHA256: "h", Project: "app", Sent: true}}}
	res := m.Merge(Scan{Files: []File{{Rel: "memories_v2/rollout_summaries/a.md", SHA256: "h", Project: "renamed"}}}, nil)
	if e := m.Entries[0]; res.Moved != 1 || e.Sent || e.SentProject != "app" || e.Project != "renamed" {
		t.Errorf("merge = %+v, entry %+v", res, e)
	}
}

func TestMergeMovesAFileToItsNewProject(t *testing.T) {
	m := Manifest{Source: "s", Project: "home", Preset: PresetCodex}
	s := Scan{Files: []File{{Rel: "memories_v2/rollout_summaries/a.md", SHA256: "h", Project: "app"}, {Rel: "memories_v2/memory_summary.md", SHA256: "k"}}}
	m.Merge(s, nil)
	if m.Entries[1].Project != "app" || m.Entries[0].Project != "" {
		t.Fatalf("entries = %+v", m.Entries)
	}
	for i := range m.Entries {
		m.Entries[i].Sent, m.Entries[i].SentProject = true, m.ProjectOf(m.Entries[i])
	}
	s.Files[0].Project = "renamed"
	res := m.Merge(s, nil)
	e := m.Entries[1]
	if res.Moved != 1 || res.Unchanged != 1 || e.Sent || e.Project != "renamed" || e.SentProject != "app" || len(m.Unsent()) != 1 {
		t.Errorf("merge = %+v, entry %+v: the moved file goes again, its old project remembered", res, e)
	}
	// Back to the project it was sent to before it went again: nothing to do.
	s.Files[0].Project = "app"
	if res := m.Merge(s, nil); res.Moved != 0 || !reflect.DeepEqual(m.Entries[1].Project, "app") {
		t.Errorf("merge back = %+v, entry %+v", res, m.Entries[1])
	}
	// To the import's own project: recorded as "".
	s.Files[0].Project = "home"
	if res := m.Merge(s, nil); res.Moved != 1 || m.Entries[1].Project != "" || m.ProjectOf(m.Entries[1]) != "home" {
		t.Errorf("merge to home = %+v, entry %+v", res, m.Entries[1])
	}
}

func TestBuildPlanPerProjectForAPreset(t *testing.T) {
	at := time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC)
	s := Scan{Root: "/c", Preset: PresetCodex, Stores: []string{"memories_v2"}, Files: []File{
		{Rel: "memories_v2/memory_summary.md", Chars: 600, DateFromMtime: true, Date: at},
		{Rel: "memories_v2/rollout_summaries/a.md", Chars: 1200, Project: "app", Date: at},
		{Rel: "memories_v2/rollout_summaries/b.md", Chars: 600, Project: "app", Date: at},
	}, Skipped: map[string][]string{SkipRolloutOnDisk: {"memories_v2/rollout_summaries/c.md"}}, KeptOut: []string{"/secret"}}
	p := retain.Prices{Input: 1, Output: 2, Currency: "USD"}
	plan := BuildPlan(s, nil, "src", "home", p)
	if len(plan.Projects) != 2 || plan.Projects[0].Project != "app" || plan.Projects[0].Files != 2 || plan.Projects[1].Project != "home" || plan.Projects[1].Files != 1 {
		t.Fatalf("projects = %+v", plan.Projects)
	}
	if want := retain.EstimateDocument(1200, p).Add(retain.EstimateDocument(600, p)); plan.Projects[0].Estimate != want {
		t.Errorf("app estimate = %+v, want %+v", plan.Projects[0].Estimate, want)
	}
	var folders []string
	for _, f := range plan.Folders {
		folders = append(folders, f.Folder)
	}
	if !reflect.DeepEqual(folders, []string{"memories_v2/rollout_summaries", "memories_v2"}) {
		t.Errorf("folders = %v", folders)
	}
	want := []PlanFile{{Rel: "memories_v2/memory_summary.md", Project: "home", Date: at, DateFrom: "mtime"},
		{Rel: "memories_v2/rollout_summaries/a.md", Project: "app", Date: at, DateFrom: "header"},
		{Rel: "memories_v2/rollout_summaries/b.md", Project: "app", Date: at, DateFrom: "header"}}
	if !reflect.DeepEqual(plan.MixesExcludedBy, []string{"/secret"}) {
		t.Errorf("mixes excluded by %v", plan.MixesExcludedBy)
	}
	if !reflect.DeepEqual(plan.Sends, want) || !reflect.DeepEqual(plan.SkippedFiles, s.Skipped) || plan.Preset != PresetCodex {
		t.Errorf("sends = %+v skipped files %v", plan.Sends, plan.SkippedFiles)
	}
	// A plain folder's plan does not list its files.
	if plain := BuildPlan(Scan{Files: s.Files[:1]}, nil, "src", "home", p); plain.Sends != nil || plain.SkippedFiles != nil || len(plain.Projects) != 1 {
		t.Errorf("plain plan = %+v", plain)
	}
}

func TestSeedFromServerKnowsEachDocumentsProject(t *testing.T) {
	m := Manifest{Source: "s", Project: "home", Preset: PresetCodex}
	s := scanOf(File{Rel: "memories_v2/rollout_summaries/a.md", SHA256: "ha", Project: "app"},
		File{Rel: "memories_v2/rollout_summaries/b.md", SHA256: "hb", Project: "renamed"})
	SeedFromServer(&m, s, []hindsight.DocumentItem{
		{ID: "import:s/memories_v2/rollout_summaries/a.md", Tags: []string{"project:app", "source:import"}, Metadata: map[string]string{"path": "memories_v2/rollout_summaries/a.md", "sha256": "ha"}},
		{ID: "import:s/memories_v2/rollout_summaries/b.md", Tags: []string{"project:app", "source:import"}, Metadata: map[string]string{"path": "memories_v2/rollout_summaries/b.md", "sha256": "hb"}},
	})
	if len(m.Entries) != 2 {
		t.Fatalf("entries = %+v", m.Entries)
	}
	if a := m.Entries[0]; !a.Sent || a.Project != "app" || a.SentProject != "app" {
		t.Errorf("a = %+v: the server holds it, same content, same project", a)
	}
	// Same content, another project: it goes again, its old project known.
	if b := m.Entries[1]; b.Sent || b.Project != "renamed" || b.SentProject != "app" {
		t.Errorf("b = %+v", b)
	}
}
