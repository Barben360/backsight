package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/state"
)

// codexStoreHome is a Codex home holding the recorded memory stores of
// variant (internal/testdata/codex/0.167.0/memories/<v1|v2|dual>), with
// the .git/ the recording lists but did not copy, every file dated at.
func codexStoreHome(t *testing.T, variant string, at time.Time) string {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join("..", "testdata", "codex", "0.167.0", "memories", variant)
	for _, store := range fileimport.CodexStores {
		from := filepath.Join(src, store)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
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
			return os.Chtimes(to, at, at)
		}); err != nil {
			t.Fatal(err)
		}
		writeCodexFile(t, filepath.Join(home, store, ".git", "HEAD"), "ref: refs/heads/main\n")
	}
	return home
}

func writeCodexFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// codexSummary is a rollout summary as Codex writes it, of a conversation
// in cwd whose rollout is gone.
func codexSummary(cwd, updatedAt, body string) string {
	return "thread_id: 01a1c748-85f2-7011-b1ee-74ef2a380048\nupdated_at: " + updatedAt +
		"\nrollout_path: /nowhere/.codex/sessions/2026/11/10/rollout-x.jsonl\ncwd: " + cwd + "\n\n" + body + "\n"
}

// retainedOf is the index of the last item the fake accepted for doc.
func retainedOf(doc string) int {
	i := -1
	for j, d := range retainedDocIDs {
		if d == doc {
			i = j
		}
	}
	return i
}

func TestImportCodexPlanGivesEachFileItsProjectAndDate(t *testing.T) {
	home := importHome(t)
	at := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	codex := codexStoreHome(t, "v2", at)
	// The first thread's conversation is still on disk: past sessions send it.
	writeCodexFile(t, filepath.Join(codex, "sessions", "2026", "11", "10", "rollout-2026-11-10T21-05-23-01a1c747-a22f-7a40-9fbe-74c9c7c099cc.jsonl"), "{}\n")
	code, env, errOut := importFiles(t, "--from", "codex", "--codex-home", codex, "--plan")
	if code != 0 || env.Data.Queued {
		t.Fatalf("exit %d, data %+v, %s", code, env.Data, errOut)
	}
	p := env.Data.Plan
	transverse := p.Project
	want := []fileimport.PlanFile{
		{Rel: "memories_v2/memory_summary.md", Project: transverse, Date: at, DateFrom: "mtime"},
		{Rel: "memories_v2/rollout_summaries/2026-11-10T21-05-31-3lru-fake_slug.md", Project: "work", Date: time.Date(2026, 11, 10, 19, 5, 33, 0, time.UTC), DateFrom: "header"},
		{Rel: "memories_v2/rollout_summaries/2026-11-10T21-05-40-fhUb-fake_slug.md", Project: "work", Date: time.Date(2026, 11, 10, 19, 5, 41, 0, time.UTC), DateFrom: "header"},
	}
	if p.Preset != "codex" || p.Files != 3 || !slices.EqualFunc(p.Sends, want, func(a, b fileimport.PlanFile) bool {
		return a.Rel == b.Rel && a.Project == b.Project && a.Date.Equal(b.Date) && a.DateFrom == b.DateFrom
	}) {
		t.Fatalf("plan sends = %+v, want %+v", p.Sends, want)
	}
	if got := p.SkippedFiles[fileimport.SkipRolloutOnDisk]; !slices.Equal(got, []string{"memories_v2/rollout_summaries/2026-11-10T21-05-22-NFaA-fake_slug.md"}) {
		t.Errorf("rollout on disk = %v", got)
	}
	if got := p.SkippedFiles[fileimport.SkipCodexWorking]; !slices.Equal(got, []string{"memories_v2/extensions/ad_hoc/instructions.md"}) {
		t.Errorf("working files = %v", got)
	}
	if len(p.Projects) != 2 || p.Projects[0].Project != "work" || p.Projects[0].Files != 2 || p.Projects[1].Project != transverse {
		t.Errorf("per project = %+v", p.Projects)
	}
	if retained.Load() != 0 || len(bankWrites()) != 0 {
		t.Errorf("a plan sent %d items, bank writes %v", retained.Load(), bankWrites())
	}
	if _, err := os.Stat(pathsOf(home).ImportDir()); !os.IsNotExist(err) {
		t.Errorf("a plan wrote the import folder: %v", err)
	}
	// The text plan names the projects and why a summary waits.
	_, text, _ := run(t, "import", "--from", "codex", "--codex-home", codex, "--plan")
	for _, w := range []string{"Codex memories in " + codex, "work", "1 conversation still on disk (sent with past sessions)", "1 Codex working file"} {
		if !strings.Contains(text, w) {
			t.Errorf("plan text lacks %q:\n%s", w, text)
		}
	}
}

func TestImportCodexStores(t *testing.T) {
	for _, tc := range []struct {
		variant string
		sends   []string
		skipped map[string]int
	}{
		{"v1", []string{"memories/MEMORY.md", "memories/memory_summary.md", "memories/rollout_summaries/2026-11-10T21-06-22-1qW9-fake_slug.md",
			"memories/rollout_summaries/2026-11-10T21-06-30-kwGL-fake_slug.md", "memories/rollout_summaries/2026-11-10T21-06-39-9BVv-fake_slug.md"},
			map[string]int{fileimport.SkipCodexWorking: 2}},
		{"dual", []string{"memories/MEMORY.md", "memories_v2/memory_summary.md", "memories_v2/rollout_summaries/2026-11-10T21-06-21-W0BO-fake_slug.md",
			"memories_v2/rollout_summaries/2026-11-10T21-06-30-ykZG-fake_slug.md", "memories_v2/rollout_summaries/2026-11-10T21-06-38-Ge6z-fake_slug.md"},
			map[string]int{fileimport.SkipCodexWorking: 3, fileimport.SkipDuplicate: 4}},
	} {
		t.Run(tc.variant, func(t *testing.T) {
			importHome(t)
			codex := codexStoreHome(t, tc.variant, time.Now())
			_, env, errOut := importFiles(t, "--from", "codex", "--codex-home", codex, "--plan")
			var sends []string
			for _, f := range env.Data.Plan.Sends {
				sends = append(sends, f.Rel)
			}
			if !slices.Equal(sends, tc.sends) {
				t.Errorf("sends %v, want %v (%s)", sends, tc.sends, errOut)
			}
			for reason, n := range tc.skipped {
				if env.Data.Plan.Skipped[reason] != n {
					t.Errorf("skipped %s = %d, want %d (%v)", reason, env.Data.Plan.Skipped[reason], n, env.Data.Plan.Skipped)
				}
			}
		})
	}
}

func TestImportCodexSendsToEachProjectWithItsDate(t *testing.T) {
	home := importHome(t)
	fastBackfill(t)
	at := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	codex := codexStoreHome(t, "v2", at)
	if code, _, errOut := run(t, "import", "--from", "codex", "--codex-home", codex); code != 0 {
		t.Fatalf("queue: exit %d %s", code, errOut)
	}
	m := manifestOf(t, home)
	if m.Preset != "codex" || m.Root != codex {
		t.Errorf("manifest preset %q root %q", m.Preset, m.Root)
	}
	tick(t)
	if n := len(importDocs()); n != 4 {
		t.Fatalf("sent %v, want the summary and three conversations' summaries", importDocs())
	}
	prefix := "import:" + m.Source + "/"
	sum := retainedOf(prefix + "memories_v2/memory_summary.md")
	conv := retainedOf(prefix + "memories_v2/rollout_summaries/2026-11-10T21-05-31-3lru-fake_slug.md")
	if sum < 0 || conv < 0 {
		t.Fatalf("documents %v", importDocs())
	}
	if !strings.Contains(retainedTags[sum], "project:"+m.Project) || !retainedTimestamps[sum].Equal(at) {
		t.Errorf("memory_summary.md: tags %q date %v", retainedTags[sum], retainedTimestamps[sum])
	}
	if !strings.Contains(retainedTags[conv], "project:work") || strings.Contains(retainedTags[conv], "project:"+m.Project) ||
		!retainedTimestamps[conv].Equal(time.Date(2026, 11, 10, 19, 5, 33, 0, time.UTC)) {
		t.Errorf("summary: tags %q date %v", retainedTags[conv], retainedTimestamps[conv])
	}
	// Run again on the same store: nothing new to send.
	_, env, _ := importFiles(t, "--from", "codex", "--codex-home", codex)
	if env.Data.Merge.Unchanged != 4 || env.Data.Plan.Files != 0 {
		t.Errorf("rerun merge %+v, files %d", env.Data.Merge, env.Data.Plan.Files)
	}
	// import list names it as Codex's memories.
	_, text, _ := run(t, "import", "list")
	if !strings.Contains(text, "Codex memories in "+codex) || !strings.Contains(text, "4/4 sent") {
		t.Errorf("import list:\n%s", text)
	}
}

func TestImportCodexChangedFileGoesAgainAndPurges(t *testing.T) {
	home := importHome(t)
	fastBackfill(t)
	codex := codexStoreHome(t, "v1", time.Now())
	run(t, "import", "--from", "codex", "--codex-home", codex)
	tick(t)
	m := manifestOf(t, home)
	doc := "import:" + m.Source + "/memories/MEMORY.md"
	first := retainedOf(doc)
	writeCodexFile(t, filepath.Join(codex, "memories", "MEMORY.md"), "# Memory\n\nthe deploy runs on Fridays\n")
	_, env, _ := importFiles(t, "--from", "codex", "--codex-home", codex)
	if env.Data.Merge.Changed != 1 {
		t.Fatalf("merge = %+v", env.Data.Merge)
	}
	tick(t)
	if again := retainedOf(doc); again <= first || !strings.Contains(retainedContents[again], "Fridays") {
		t.Fatalf("the changed MEMORY.md was not sent again under %s (%d then %d)", doc, first, again)
	}
	code, out, errOut := run(t, "import", "purge", m.Source, "--yes", "--json")
	if code != 0 || !strings.Contains(out, doc) {
		t.Fatalf("purge: exit %d %s %s", code, out, errOut)
	}
	if all, _ := fileimport.ListManifests(pathsOf(home).ImportDir()); len(all) != 0 {
		t.Errorf("manifest left after the purge: %+v", all)
	}
}

// A re-import that maps a file to another project sends it there again,
// and first clears what the old project drew from it.
func TestImportCodexMovedFileLeavesNothingInTheOldProject(t *testing.T) {
	home := importHome(t)
	fastBackfill(t)
	codex := t.TempDir()
	app := filepath.Join(t.TempDir(), "src", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(codex, "memories_v2", "rollout_summaries", "2026-11-10T21-06-21-W0BO-x.md")
	writeCodexFile(t, summary, codexSummary(app, "2026-11-10T19:06:22+00:00", "the app's tests need Docker"))
	run(t, "import", "--from", "codex", "--codex-home", codex)
	tick(t)
	m := manifestOf(t, home)
	doc := m.Entries[0].Document
	if i := retainedOf(doc); i < 0 || !strings.Contains(retainedTags[i], "project:app") {
		t.Fatalf("first send: %v", importDocs())
	}
	// The server extracted a fact from it, and an observation drew on it.
	listAnswer[""] = `[{"id":"fact-1","document_id":"` + doc + `","fact_type":"world"},{"id":"obs-1","document_id":"` + doc + `","fact_type":"observation"}]`
	memories["fact-1"] = `{"id":"fact-1","type":"world"}`

	// Same content: an untouched send moves nothing and clears nothing.
	_, env, _ := importFiles(t, "--from", "codex", "--codex-home", codex)
	if env.Data.Merge.Unchanged != 1 || env.Data.Merge.Moved != 0 {
		t.Fatalf("rerun merge = %+v", env.Data.Merge)
	}
	// The user names the folder's project: the summary belongs to it now.
	writeCodexFile(t, filepath.Join(app, ".backsight.toml"), "project = \"renamed\"\n")
	_, env, _ = importFiles(t, "--from", "codex", "--codex-home", codex)
	if env.Data.Merge.Moved != 1 || env.Data.Plan.Files != 1 || env.Data.Plan.Sends[0].Project != "renamed" {
		t.Fatalf("merge = %+v, sends %+v", env.Data.Merge, env.Data.Plan.Sends)
	}
	before := retainedOf(doc)
	tick(t)
	cleared := slices.ContainsFunc(curation, func(c string) bool {
		return strings.HasPrefix(c, "DELETE ") && strings.Contains(c, "/memories/fact-1/observations")
	})
	if !cleared {
		t.Errorf("the old project's observations were not cleared: curation %v", curation)
	}
	if slices.ContainsFunc(curation, func(c string) bool { return strings.Contains(c, "obs-1") }) {
		t.Errorf("an observation was curated as a fact: %v", curation)
	}
	i := retainedOf(doc)
	if i <= before || !strings.Contains(retainedTags[i], "project:renamed") || strings.Contains(retainedTags[i], "project:app") {
		t.Errorf("not sent again to the new project: %d after %d, tags %v", i, before, retainedTags[max(i, 0)])
	}
	if e := manifestOf(t, home).Entries[0]; e.Document != doc || e.SentProject != "renamed" || !e.Sent {
		t.Errorf("entry = %+v", e)
	}
	// A plain content change later, same project: its old facts' observations stay.
	curation = nil
	writeCodexFile(t, summary, codexSummary(app, "2026-11-11T08:00:00+00:00", "the app's tests need Docker 27"))
	importFiles(t, "--from", "codex", "--codex-home", codex)
	tick(t)
	if len(curation) != 0 {
		t.Errorf("a change in the same project cleared observations: %v", curation)
	}
	if i := retainedOf(doc); !retainedTimestamps[i].Equal(time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("the changed summary's date = %v, want its new updated_at", retainedTimestamps[i])
	}
}

// A summary Codex rewrote between the queue and the send goes with its
// new header's date.
func TestImportCodexSendReadsTheHeaderAgain(t *testing.T) {
	importHome(t)
	fastBackfill(t)
	codex := t.TempDir()
	summary := filepath.Join(codex, "memories_v2", "rollout_summaries", "a.md")
	writeCodexFile(t, summary, codexSummary("/src/app", "2026-11-10T19:00:00+00:00", "first"))
	run(t, "import", "--from", "codex", "--codex-home", codex)
	writeCodexFile(t, summary, codexSummary("/src/app", "2026-11-12T07:30:00+00:00", "rewritten"))
	tick(t)
	if len(retainedTimestamps) != 1 || !retainedTimestamps[0].Equal(time.Date(2026, 11, 12, 7, 30, 0, 0, time.UTC)) || !strings.Contains(retainedContents[0], "rewritten") {
		t.Errorf("sent %v at %v", retainedContents, retainedTimestamps)
	}
}

func TestImportCodexKeepsOutExcludedFolders(t *testing.T) {
	home := importHome(t)
	fastBackfill(t)
	codex := t.TempDir()
	secret, open := filepath.Join(t.TempDir(), "secret"), filepath.Join(t.TempDir(), "open")
	writeCodexFile(t, filepath.Join(codex, "memories_v2", "memory_summary.md"), "v1\n## User Profile\nworks on secret and open\n")
	writeCodexFile(t, filepath.Join(codex, "memories_v2", "rollout_summaries", "a.md"), codexSummary(secret, "2026-11-10T19:00:00+00:00", "secret plans"))
	configure(t, home, "[privacy]\nexclude_paths = ['"+filepath.ToSlash(secret)+"']")
	_, env, _ := importFiles(t, "--from", "codex", "--codex-home", codex, "--plan")
	p := env.Data.Plan
	if p.Files != 0 || p.Skipped[fileimport.SkipOptedOut] != 1 || p.Skipped[fileimport.SkipMixesExcluded] != 1 {
		t.Fatalf("plan = files %d skipped %v", p.Files, p.Skipped)
	}

	// A folder kept out after the import was queued: its summary is not sent.
	home = importHome(t)
	fastBackfill(t)
	codex = t.TempDir()
	writeCodexFile(t, filepath.Join(codex, "memories_v2", "rollout_summaries", "a.md"), codexSummary(open, "2026-11-10T19:00:00+00:00", "open plans"))
	run(t, "import", "--from", "codex", "--codex-home", codex)
	configure(t, home, "[privacy]\nexclude_paths = ['"+filepath.ToSlash(open)+"']")
	tick(t)
	if len(importDocs()) != 0 || len(manifestOf(t, home).Unsent()) != 0 {
		t.Errorf("sent %v, left %v", importDocs(), manifestOf(t, home).Unsent())
	}
}

func TestImportCodexHidesSecrets(t *testing.T) {
	importHome(t)
	fastBackfill(t)
	codex := t.TempDir()
	key := "sk-ant-api03-" + strings.Repeat("A", 90)
	writeCodexFile(t, filepath.Join(codex, "memories_v2", "memory_summary.md"), "v1\nthe key is "+key+"\n")
	_, env, _ := importFiles(t, "--from", "codex", "--codex-home", codex)
	if env.Data.Plan.RedactedFiles != 1 {
		t.Errorf("redacted files = %d", env.Data.Plan.RedactedFiles)
	}
	tick(t)
	if len(retainedContents) != 1 || strings.Contains(retainedContents[0], key) {
		t.Errorf("sent %q", retainedContents)
	}
}

func TestImportCodexRefusals(t *testing.T) {
	importHome(t)
	codex := codexStoreHome(t, "v2", time.Now())
	folder := importTree(t, map[string]string{"a.md": "alpha"})
	for name, args := range map[string][]string{
		"nothing to import":       {},
		"folder and preset":       {folder, "--from", "codex"},
		"unknown tool":            {"--from", "cursor"},
		"codex home alone":        {folder, "--codex-home", codex},
		"project with the preset": {"--from", "codex", "--codex-home", codex, "--project", "x"},
		"ext with the preset":     {"--from", "codex", "--codex-home", codex, "--ext", "jsonl"},
		"no such home":            {"--from", "codex", "--codex-home", filepath.Join(codex, "absent")},
	} {
		code, env, _ := importFiles(t, append(args, "--plan")...)
		if code != output.Usage || env.Error == nil {
			t.Errorf("%s: exit %d %+v", name, code, env.Error)
		}
	}
}

func TestImportCodexHome(t *testing.T) {
	home := t.TempDir()
	def, env, recorded := filepath.Join(home, ".codex"), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(v string) func(string) string {
		return func(k string) string {
			if k == "CODEX_HOME" {
				return v
			}
			return ""
		}
	}
	var none state.State
	var rec state.State
	rec.ClientFor(harnessid.Codex).Profile = recorded
	var recDefault state.State
	rec2 := recDefault.ClientFor(harnessid.Codex)
	rec2.Profile = ""
	flag := t.TempDir()
	for _, tc := range []struct {
		name, flag, env string
		st              state.State
		want            string
	}{
		{"default", "", "", none, def},
		{"CODEX_HOME", "", env, none, env},
		{"recorded wins over CODEX_HOME", "", env, rec, recorded},
		{"recorded default", "", env, recDefault, def},
		{"flag wins", flag, env, rec, flag},
	} {
		got, err := importCodexHome(home, tc.flag, tc.st, getenv(tc.env))
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := importCodexHome(home, filepath.Join(home, "absent"), none, getenv("")); err == nil {
		t.Error("an absent home gave no error")
	}
}
