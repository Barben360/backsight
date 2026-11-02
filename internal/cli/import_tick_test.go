package cli

import (
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
