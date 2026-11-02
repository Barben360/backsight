package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// queuedImport is a home with files queued for import, ready for ticks.
func queuedImport(t *testing.T, files map[string]string, args ...string) (home, root string) {
	t.Helper()
	home = importHome(t)
	fastBackfill(t)
	root = importTree(t, files)
	if code, _, errOut := run(t, append([]string{"import", root}, args...)...); code != 0 {
		t.Fatalf("queue: exit %d %s", code, errOut)
	}
	return home, root
}

func tick(t *testing.T) tickResult {
	t.Helper()
	code, out, errOut := run(t, "jobs", "tick", "--json")
	var env struct {
		Data tickResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("tick: exit %d %q %s", code, out, errOut)
	}
	return env.Data
}

func importDocs() []string {
	var out []string
	for _, d := range retainedDocIDs {
		if strings.HasPrefix(d, "import:") {
			out = append(out, d)
		}
	}
	return out
}

func TestTickSendsQueuedImport(t *testing.T) {
	home, _ := queuedImport(t, map[string]string{"a.md": "alpha", "b/c.md": "gamma", "d.txt": "delta"})
	res := tick(t)
	docs := importDocs()
	if len(docs) != 3 || !strings.Contains(res.Import, "3 sent") {
		t.Fatalf("sent %v, tick import %q", docs, res.Import)
	}
	m := manifestOf(t, home)
	for _, d := range docs {
		if !strings.HasPrefix(d, "import:"+m.Source+"/") {
			t.Errorf("document %q", d)
		}
	}
	for i, d := range retainedDocIDs {
		if strings.HasPrefix(d, "import:") && (retainedStrategies[i] != "import" || !strings.Contains(retainedTags[i], "source:import")) {
			t.Errorf("%s: strategy %q tags %q", d, retainedStrategies[i], retainedTags[i])
		}
	}
	if len(m.Unsent()) != 0 {
		t.Errorf("unsent after the tick: %+v", m.Unsent())
	}
	tick(t)
	if len(importDocs()) != 3 {
		t.Errorf("a second tick sent again: %v", importDocs())
	}
}

func TestTickImportPacesTenFiles(t *testing.T) {
	files := map[string]string{}
	for i := range 12 {
		files[fmt.Sprintf("n%02d.md", i)] = fmt.Sprintf("note %d", i)
	}
	queuedImport(t, files)
	tick(t)
	if n := len(importDocs()); n != 10 {
		t.Fatalf("first tick sent %d, want 10", n)
	}
	tick(t)
	if n := len(importDocs()); n != 12 {
		t.Errorf("after two ticks %d sent, want 12", n)
	}
}

func TestTickImportWaitsBehindBackfill(t *testing.T) {
	for _, paused := range []string{"", "budget"} {
		t.Run("paused="+paused, func(t *testing.T) {
			home, _ := queuedImport(t, map[string]string{"a.md": "alpha"})
			old := tickBackfillLimit
			tickBackfillLimit = 0 // the past sessions stay queued through the tick
			t.Cleanup(func() { tickBackfillLimit = old })
			oldSession(t, home, "x1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
			d := pathsOf(home)
			queue := []string{filepath.Join(home, ".claude", "projects", "-home-u-billing", "x1.jsonl")}
			_ = state.Update(d, "test", func(s *state.State) error {
				s.Backfill = &state.Backfill{Sessions: queue, Total: 1, Paused: paused, Currency: "USD", MaxCost: 1, StartDay: time.Now().UTC().Format("2006-01-02")}
				return nil
			})
			res := tick(t)
			if len(importDocs()) != 0 || !strings.Contains(res.Import, "waiting") {
				t.Fatalf("sent %v while past sessions are pending; tick import %q", importDocs(), res.Import)
			}
			_ = state.Update(d, "test", func(s *state.State) error { s.Backfill = nil; return nil })
			tick(t)
			if len(importDocs()) != 1 {
				t.Errorf("after the backfill: sent %v", importDocs())
			}
		})
	}
}

func TestTickImportPausesAtTheCap(t *testing.T) {
	home, _ := queuedImport(t, map[string]string{"a.md": "alpha", "b.md": "beta", "c.md": "gamma"}, "--max-tokens", "1")
	llmPerRetain.Store(1000)
	tick(t)
	if n := len(importDocs()); n != 1 {
		t.Fatalf("sent %d under a 1-token cap, want 1 then a pause", n)
	}
	if m := manifestOf(t, home); m.Paused != "budget" || m.SpentTokens < 1000 {
		t.Errorf("manifest = paused %q spent %d", m.Paused, m.SpentTokens)
	}
	tick(t)
	if n := len(importDocs()); n != 1 {
		t.Errorf("a paused import sent again: %d", n)
	}
}

func TestTickSendsCurrentContentAndSkipsVanished(t *testing.T) {
	home, root := queuedImport(t, map[string]string{"a.md": "alpha", "b.md": "beta"})
	_ = os.WriteFile(filepath.Join(root, "a.md"), []byte("alpha edited after queueing"), 0o644)
	_ = os.Remove(filepath.Join(root, "b.md"))
	tick(t)
	if docs := importDocs(); len(docs) != 1 || !slices.Contains(retainedContents, "alpha edited after queueing") {
		t.Fatalf("sent %v, contents %q", docs, retainedContents)
	}
	m := manifestOf(t, home)
	for _, e := range m.Entries {
		switch e.Rel {
		case "a.md":
			raw, _ := os.ReadFile(filepath.Join(root, "a.md"))
			if !e.Sent || e.SHA256 != fileSHA(raw) {
				t.Errorf("a.md = %+v", e)
			}
		case "b.md":
			if !e.Gone || e.Sent {
				t.Errorf("b.md = %+v, want gone and never sent", e)
			}
		}
	}
	if len(m.Unsent()) != 0 {
		t.Errorf("the import does not finish: %+v", m.Unsent())
	}
}

func TestTickImportRedactsAtSend(t *testing.T) {
	_, root := queuedImport(t, map[string]string{"a.md": "alpha"})
	secret := "sk-ant-" + strings.Repeat("Zq9", 20)
	_ = os.WriteFile(filepath.Join(root, "a.md"), []byte("key "+secret), 0o644)
	tick(t)
	for _, c := range retainedContents {
		if strings.Contains(c, secret) {
			t.Fatal("a secret reached the server")
		}
	}
	if !slices.ContainsFunc(retainedContents, func(c string) bool { return strings.Contains(c, "[redacted:") }) {
		t.Errorf("contents %q", retainedContents)
	}
}

func TestTickImportRespectsSubscriptionLane(t *testing.T) {
	home, _ := queuedImport(t, map[string]string{"a.md": "alpha"})
	_ = os.WriteFile(pathsOf(home).ConfigFile(), []byte(strings.Replace(mustRead(t, pathsOf(home).ConfigFile()), "[llm]", "[llm]\nprovider = \"claude-code\"", 1)), 0o600)
	tick(t)
	if len(importDocs()) != 0 {
		t.Errorf("sent %v on the subscription lane without consent", importDocs())
	}
}

func TestTickImportSkipsTombstonedIDs(t *testing.T) {
	home := importHome(t)
	fastBackfill(t)
	root := importTree(t, map[string]string{"a.md": "alpha"})
	// The source id is known only after a plan.
	_, env, _ := importFiles(t, root, "--plan")
	src := env.Data.Plan.Source
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.AddTombstones(state.Tombstone{Bank: serverBank(t), Document: "import:" + src + "/a.md"})
		return nil
	})
	if code, _, errOut := run(t, "import", root); code != 0 {
		t.Fatalf("queue: %d %s", code, errOut)
	}
	tick(t)
	if docs := importDocs(); len(docs) != 1 || docs[0] != "import:"+src+"/a.md#2" {
		t.Errorf("sent %v, want the successor id", docs)
	}
}

func fileSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type listEnvelope struct {
	Data []filesImportSource `json:"data"`
}

func TestImportListStatuses(t *testing.T) {
	home := importHome(t)
	fastBackfill(t)
	d := pathsOf(home).ImportDir()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, m := range []fileimport.Manifest{
		{Schema: 1, Source: "aaaaaaaaaaaa", Root: "/r/a", Project: "p", Entries: []fileimport.Entry{{Rel: "x", Sent: true}, {Rel: "y", Sent: true, Gone: true}}},
		{Schema: 1, Source: "bbbbbbbbbbbb", Root: "/r/b", Project: "p", Paused: "budget", SpentCost: 0.5, MaxCost: 0.4, Currency: "USD", Entries: []fileimport.Entry{{Rel: "x"}}},
		{Schema: 1, Source: "cccccccccccc", Root: "/r/c", Project: "p", Entries: []fileimport.Entry{{Rel: "x"}, {Rel: "y", Sent: true}}},
	} {
		m.QueuedAt = base.Add(time.Duration(i) * time.Hour)
		if err := fileimport.SaveManifest(d, m); err != nil {
			t.Fatal(err)
		}
	}
	_, out, _ := run(t, "import", "list", "--json")
	var env listEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil || len(env.Data) != 3 {
		t.Fatalf("list = %q %v", out, err)
	}
	a, b, c := env.Data[0], env.Data[1], env.Data[2]
	if a.Source != "aaaaaaaaaaaa" || a.Status != "done" || a.Files != 2 || a.Sent != 2 || a.Gone != 1 {
		t.Errorf("done source = %+v", a)
	}
	if b.Status != "paused" || b.Reason != "budget" || b.SpentCost != 0.5 || b.MaxCost != 0.4 {
		t.Errorf("paused source = %+v", b)
	}
	if c.Status != "sending" || c.Sent != 1 {
		t.Errorf("sending source = %+v", c)
	}
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Sessions: []string{"x.jsonl"}}
		return nil
	})
	_, out, _ = run(t, "import", "list", "--json")
	_ = json.Unmarshal([]byte(out), &env)
	if c := env.Data[2]; c.Status != "waiting" || c.Reason != "past_sessions" {
		t.Errorf("with past sessions pending: %+v", c)
	}
}

// sentImport is a home whose folder of two files was imported and sent.
func sentImport(t *testing.T) (home, root, src string) {
	t.Helper()
	home, root = queuedImport(t, map[string]string{"a.md": "alpha", "b.md": "beta"})
	tick(t)
	src = manifestOf(t, home).Source
	if len(importDocs()) != 2 {
		t.Fatalf("sent %v", importDocs())
	}
	return home, root, src
}

func TestImportPurgePlanListsDocuments(t *testing.T) {
	_, _, src := sentImport(t)
	code, out, errOut := run(t, "import", "purge", src, "--plan", "--json")
	if code != 0 || !strings.Contains(out, "import:"+src+"/a.md") || !strings.Contains(out, "import:"+src+"/b.md") {
		t.Fatalf("plan: exit %d %s %s", code, out, errOut)
	}
	if code, _, _ := run(t, "import", "purge", src); code != output.NeedsUser {
		t.Errorf("without --yes: exit %d, want %d", code, output.NeedsUser)
	}
	if fakeDocExists(t, "import:"+src+"/a.md") == false {
		t.Error("a plan or an unconfirmed purge deleted a document")
	}
}

func TestImportPurgeDeletesOnlyThatSource(t *testing.T) {
	home, _, src := sentImport(t)
	sendFakeDocument(t, "conversation:keep", map[string]string{"session_id": "keep"})
	sendFakeDocument(t, "import:ffffffffffff/other.md", map[string]string{"path": "other.md"})
	if code, out, errOut := run(t, "import", "purge", src, "--yes"); code != 0 {
		t.Fatalf("exit %d %s %s", code, out, errOut)
	}
	for doc, want := range map[string]bool{"import:" + src + "/a.md": false, "import:" + src + "/b.md": false, "conversation:keep": true, "import:ffffffffffff/other.md": true} {
		if got := fakeDocExists(t, doc); got != want {
			t.Errorf("%s exists = %v, want %v", doc, got, want)
		}
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	var tombs []string
	for _, tb := range st.TombstonesOf(serverBank(t)) {
		tombs = append(tombs, tb.Document)
	}
	slices.Sort(tombs)
	if !slices.Equal(tombs, []string{"import:" + src + "/a.md", "import:" + src + "/b.md"}) {
		t.Errorf("tombstones = %v", tombs)
	}
	if !fakeDocExists(t, "backsight:tombstones") {
		t.Error("the tombstones were not mirrored to the control bank")
	}
	if all, _ := fileimport.ListManifests(pathsOf(home).ImportDir()); len(all) != 0 {
		t.Errorf("manifest kept after purge: %+v", all)
	}
}

func TestImportPurgeByPrefix(t *testing.T) {
	home, _, src := sentImport(t)
	other := fileimport.Manifest{Schema: 1, Source: src[:3] + "zzzzzzzzz", Root: "/r/z", QueuedAt: time.Now()}
	_ = fileimport.SaveManifest(pathsOf(home).ImportDir(), other)
	if code, _, errOut := run(t, "import", "purge", src[:3], "--plan"); code != output.Usage {
		t.Errorf("3 characters: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "import", "purge", src[:4], "--plan"); code != 0 {
		t.Errorf("unique 4-character prefix: exit %d %s", code, errOut)
	}
	other.Source = src[:5] + "zzzzzzz"
	_ = fileimport.SaveManifest(pathsOf(home).ImportDir(), other)
	if code, _, errOut := run(t, "import", "purge", src[:5], "--plan"); code != output.Usage || !strings.Contains(errOut, src) {
		t.Errorf("ambiguous prefix: exit %d %s", code, errOut)
	}
}

func TestImportPurgeThenReimportUsesSuccessors(t *testing.T) {
	_, root, src := sentImport(t)
	if code, _, errOut := run(t, "import", "purge", src, "--yes"); code != 0 {
		t.Fatalf("purge: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "import", root); code != 0 {
		t.Fatalf("queue again: %d %s", code, errOut)
	}
	tick(t)
	docs := importDocs()
	if !slices.Contains(docs, "import:"+src+"/a.md#2") || !slices.Contains(docs, "import:"+src+"/b.md#2") {
		t.Errorf("sent %v, want the successors", docs)
	}
}

// fakeDocExists asks the fake server for a document (memory or control bank).
func fakeDocExists(t *testing.T, doc string) bool {
	t.Helper()
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	bank := cfg.Server.Bank
	if strings.HasPrefix(doc, "backsight:") {
		bank = retain.ControlBank(bank)
	}
	_, err = cl.GetDocument(context.Background(), bank, doc)
	return err == nil
}

// --- final review fixes ---

func TestImportRefusesWhileAnImportIsSending(t *testing.T) {
	home, root, src := sentImport(t)
	l, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "job-import.lock"), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if code, out, errOut := run(t, "import", root); code != output.Blocked {
		t.Errorf("queue while sending: exit %d %s %s", code, out, errOut)
	}
	if code, out, errOut := run(t, "import", "purge", src, "--yes"); code != output.Blocked {
		t.Errorf("purge while sending: exit %d %s %s", code, out, errOut)
	}
	if !fakeDocExists(t, "import:"+src+"/a.md") {
		t.Error("a purge refused for the lock deleted a document")
	}
	if code, _, errOut := run(t, "import", root, "--plan"); code != 0 {
		t.Errorf("a plan needs no lock: exit %d %s", code, errOut)
	}
}

func TestTickImportBaseStartsAtFirstSend(t *testing.T) {
	home, _ := queuedImport(t, map[string]string{"a.md": "alpha", "b.md": "beta"})
	// Past sessions sent after the queue spend far more than this import's cap.
	llmInput.Add(50_000_000)
	tick(t)
	if n := len(importDocs()); n != 2 {
		t.Errorf("sent %d: the spend before the first send counted against the import", n)
	}
	if m := manifestOf(t, home); m.Paused != "" {
		t.Errorf("paused %q", m.Paused)
	}
}

func TestImportRerunStartsANewCapAndKeepsTheTotal(t *testing.T) {
	home, root, _ := sentImport(t)
	m := manifestOf(t, home)
	m.SpentCost, m.SpentTokens = 3, 3000
	_ = fileimport.SaveManifest(pathsOf(home).ImportDir(), m)
	_ = os.WriteFile(filepath.Join(root, "c.md"), []byte("gamma"), 0o644)
	llmInput.Add(50_000_000) // weeks of sessions since the first import
	if code, _, errOut := run(t, "import", root); code != 0 {
		t.Fatalf("rerun: %d %s", code, errOut)
	}
	m = manifestOf(t, home)
	if m.StartDay != "" || m.SpentCost != 0 || m.PriorCost != 3 || m.PriorTokens != 3000 || m.MaxCost <= 0 {
		t.Errorf("after rerun: start %q spent %v prior %v/%d cap %v", m.StartDay, m.SpentCost, m.PriorCost, m.PriorTokens, m.MaxCost)
	}
	tick(t)
	if !slices.ContainsFunc(importDocs(), func(d string) bool { return strings.HasSuffix(d, "/c.md") }) {
		t.Errorf("the new file was not sent: %v (paused %q)", importDocs(), manifestOf(t, home).Paused)
	}
}

func TestTickRechecksFilesAtSend(t *testing.T) {
	home, root := queuedImport(t, map[string]string{"big.md": "small", "link.md": "plain", "empty.md": "text", "ok.md": "fine"})
	_ = os.WriteFile(filepath.Join(root, "big.md"), []byte(strings.Repeat("x", 1<<20+1)), 0o644)
	outside := filepath.Join(t.TempDir(), "secret.md")
	_ = os.WriteFile(outside, []byte("outside the folder"), 0o644)
	_ = os.Remove(filepath.Join(root, "link.md"))
	linked := os.Symlink(outside, filepath.Join(root, "link.md")) == nil
	_ = os.WriteFile(filepath.Join(root, "empty.md"), []byte("---\ntype: x\n---\n"), 0o644)
	tick(t)
	docs := importDocs()
	if len(docs) != 1 || !strings.HasSuffix(docs[0], "/ok.md") {
		t.Errorf("sent %v, want only ok.md", docs)
	}
	if slices.Contains(retainedContents, "outside the folder") {
		t.Error("a link sent a file from outside the folder")
	}
	for _, e := range manifestOf(t, home).Entries {
		if e.Rel != "ok.md" && (e.Sent || !e.Gone) && (e.Rel != "link.md" || linked) {
			t.Errorf("%s = %+v, want set aside, not sent", e.Rel, e)
		}
	}
}

func TestImportTelemetryCoversSendDoneAndPurge(t *testing.T) {
	home, _, src := sentImport(t)
	actions := func() []string {
		var out []string
		for _, e := range eventsNamed(t, home, "import") {
			out = append(out, e["action"].(string))
		}
		return out
	}
	if a := actions(); !slices.Contains(a, "send") || !slices.Contains(a, "done") {
		t.Errorf("actions after the tick = %v", a)
	}
	for _, e := range eventsNamed(t, home, "import") {
		if e["action"] == "send" && (e["files"] != float64(2) || e["source"] != src) {
			t.Errorf("send event = %v", e)
		}
	}
	project := manifestOf(t, home).Project
	run(t, "import", "purge", src, "--yes")
	if a := actions(); !slices.Contains(a, "purge") {
		t.Errorf("actions after purge = %v", a)
	}
	// Each event carries the import's project: stats filtered on it count it.
	for _, e := range eventsNamed(t, home, "import") {
		if project == "" || e["project"] != project {
			t.Errorf("%v event project %v, want %q", e["action"], e["project"], project)
		}
	}
}

func TestTickImportEmitsPause(t *testing.T) {
	home, _ := queuedImport(t, map[string]string{"a.md": "alpha", "b.md": "beta"}, "--max-tokens", "1")
	llmPerRetain.Store(1000)
	tick(t)
	var paused bool
	for _, e := range eventsNamed(t, home, "import") {
		paused = paused || e["action"] == "pause"
	}
	if !paused {
		t.Error("no pause event at the cap")
	}
}

func TestImportRerunReusesSavedExclusions(t *testing.T) {
	home, root := queuedImport(t, map[string]string{"a.md": "alpha", "SCRATCH/x.md": "draft", "e.jsonl": "{}"}, "--exclude", "SCRATCH/**", "--ext", "jsonl")
	_ = os.WriteFile(filepath.Join(root, "SCRATCH", "y.md"), []byte("another draft"), 0o644)
	code, out, errOut := run(t, "import", root)
	if code != 0 || !strings.Contains(out, "SCRATCH/**") {
		t.Fatalf("rerun: exit %d, text does not name the saved exclusions:\n%s %s", code, out, errOut)
	}
	m := manifestOf(t, home)
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Rel, "SCRATCH/") {
			t.Errorf("an excluded draft was queued: %s", e.Rel)
		}
	}
	if !slices.ContainsFunc(m.Entries, func(e fileimport.Entry) bool { return e.Rel == "e.jsonl" && !e.Gone }) {
		t.Errorf("the saved --ext was dropped: %+v", m.Entries)
	}
}
