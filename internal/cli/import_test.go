package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/fileimport"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// importHome is a dev install pointed at the fake server, with known prices.
func importHome(t *testing.T) string {
	t.Helper()
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[llm]\nprice_input = 1.0\nprice_output = 2.0")
	return home
}

// importTree writes files (relative path -> content) under a new folder.
func importTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type filesImportEnvelope struct {
	OK    bool              `json:"ok"`
	Error *output.Error     `json:"error"`
	Data  filesImportResult `json:"data"`
}

func importFiles(t *testing.T, args ...string) (int, filesImportEnvelope, string) {
	t.Helper()
	code, out, errOut := run(t, append(append([]string{"import"}, args...), "--json")...)
	var env filesImportEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("import %v: exit %d, bad JSON %q (%s)", args, code, out, errOut)
	}
	return code, env, errOut
}

// manifestOf is the only manifest of home.
func manifestOf(t *testing.T, home string) fileimport.Manifest {
	t.Helper()
	all, err := fileimport.ListManifests(pathsOf(home).ImportDir())
	if err != nil || len(all) != 1 {
		t.Fatalf("manifests = %v %v", all, err)
	}
	return all[0]
}

func TestImportPlanSendsNothing(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha note", "b/c.md": "gamma note"})
	code, env, errOut := importFiles(t, root, "--plan")
	if code != 0 || env.Data.Plan.Files != 2 || env.Data.Queued {
		t.Fatalf("exit %d, data %+v, %s", code, env.Data, errOut)
	}
	if retained.Load() != 0 || len(bankWrites()) != 0 {
		t.Errorf("a plan sent %d items, bank writes %v", retained.Load(), bankWrites())
	}
	if _, err := os.Stat(pathsOf(home).ImportDir()); !os.IsNotExist(err) {
		t.Errorf("a plan wrote the import folder: %v", err)
	}
}

func TestImportQueuesAndWritesManifest(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha note", "b/c.md": "gamma note"})
	code, env, errOut := importFiles(t, root)
	if code != 0 || !env.Data.Queued {
		t.Fatalf("exit %d, data %+v, %s", code, env.Data, errOut)
	}
	m := manifestOf(t, home)
	if len(m.Unsent()) != 2 || m.StartDay == "" || m.Project == "" {
		t.Errorf("manifest = %+v", m)
	}
	if want := 1.5 * env.Data.Plan.Total.Cost; m.MaxCost != want || env.Data.MaxCost != want {
		t.Errorf("cap = %v (data %v), want %v", m.MaxCost, env.Data.MaxCost, want)
	}
	if retained.Load() != 0 {
		t.Errorf("queueing sent %d items", retained.Load())
	}
}

func TestImportPlanFoldersAndSkips(t *testing.T) {
	importHome(t)
	root := importTree(t, map[string]string{"LOGS/a.md": strings.Repeat("log line\n", 5000), "KNOW/p.md": "a person", "x.pdf": "pdf"})
	_, env, _ := importFiles(t, root, "--plan", "--exclude", "LOGS/**")
	for _, f := range env.Data.Plan.Folders {
		if f.Folder == "LOGS" {
			t.Errorf("excluded folder in the plan: %+v", env.Data.Plan.Folders)
		}
	}
	if env.Data.Plan.SkippedDirs["excluded"] != 1 || env.Data.Plan.Skipped["extension"] != 1 || env.Data.Plan.Files != 1 {
		t.Errorf("plan = %+v", env.Data.Plan)
	}
}

func TestImportRerunMergesAndKeepsSent(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha", "b.md": "beta"})
	importFiles(t, root)
	m := manifestOf(t, home)
	for i := range m.Entries {
		if m.Entries[i].Rel == "a.md" {
			m.Entries[i].Sent = true
		}
	}
	if err := fileimport.SaveManifest(pathsOf(home).ImportDir(), m); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "b.md"), []byte("beta changed"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "c.md"), []byte("new"), 0o644)
	code, env, errOut := importFiles(t, root)
	if code != 0 || env.Data.Merge != (fileimport.MergeResult{New: 1, Changed: 1, Unchanged: 1}) {
		t.Fatalf("exit %d merge %+v %s", code, env.Data.Merge, errOut)
	}
	if env.Data.Plan.Files != 2 {
		t.Errorf("plan files = %d, want the two unsent", env.Data.Plan.Files)
	}
	for _, e := range manifestOf(t, home).Entries {
		if e.Rel == "a.md" && !e.Sent {
			t.Error("a sent, unchanged file became unsent")
		}
	}
}

func TestImportRerunWhileBudgetPausedNeedsACap(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha"})
	importFiles(t, root)
	m := manifestOf(t, home)
	m.Paused = "budget"
	_ = fileimport.SaveManifest(pathsOf(home).ImportDir(), m)
	if code, _, _ := importFiles(t, root); code != output.Partial {
		t.Errorf("rerun while paused: exit %d, want %d", code, output.Partial)
	}
	code, _, errOut := importFiles(t, root, "--max-cost", "5")
	if m = manifestOf(t, home); code != 0 || m.Paused != "" || m.MaxCost != 5 {
		t.Errorf("rerun with a cap: exit %d, manifest %+v %s", code, m, errOut)
	}
}

func TestImportNestedRootSendsNothingTwice(t *testing.T) {
	importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha", "sub/b.md": "beta", "sub/c.md": "gamma"})
	importFiles(t, root)
	_, env, _ := importFiles(t, filepath.Join(root, "sub"), "--plan")
	if env.Data.Plan.Files != 0 || env.Data.Plan.Skipped["other_source"] != 2 {
		t.Errorf("nested plan = %+v", env.Data.Plan)
	}
}

func TestImportRebuildsLostManifest(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha", "b.md": "beta"})
	importFiles(t, root)
	m := manifestOf(t, home)
	var aDoc, aSHA string
	for _, e := range m.Entries {
		if e.Rel == "a.md" {
			aDoc, aSHA = e.Document, e.SHA256
		}
	}
	// The server holds a.md as sent; the manifest is lost.
	sendFakeDocument(t, aDoc, map[string]string{"path": "a.md", "sha256": aSHA, "source": m.Source})
	if err := fileimport.DeleteManifest(pathsOf(home).ImportDir(), m.Source); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := importFiles(t, root); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	for _, e := range manifestOf(t, home).Entries {
		if (e.Rel == "a.md") != e.Sent {
			t.Errorf("entry %+v: only a.md is sent", e)
		}
	}
}

func TestImportRefusesWhenTheServerLacksTheStrategy(t *testing.T) {
	root := importTree(t, map[string]string{"a.md": "alpha"})
	t.Run("client", func(t *testing.T) {
		home := devInstall(t, true)
		asClient(t, home, fakeHindsight(t))
		code, env, _ := importFiles(t, root)
		if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "update backsight on the server's computer") {
			t.Errorf("exit %d %+v", code, env.Error)
		}
		if len(bankWrites()) != 0 {
			t.Errorf("a client changed the bank: %v", bankWrites())
		}
		code, env, _ = importFiles(t, root, "--plan")
		if code != 0 || !strings.Contains(strings.Join(env.Data.Warnings, ","), "strategy_missing") {
			t.Errorf("plan on a client: exit %d warnings %v", code, env.Data.Warnings)
		}
	})
	t.Run("server", func(t *testing.T) {
		importHome(t)
		code, env, _ := importFiles(t, root, "--plan")
		if code != 0 || !strings.Contains(strings.Join(env.Data.Warnings, ","), "strategy_missing") {
			t.Errorf("plan: exit %d warnings %v", code, env.Data.Warnings)
		}
		code, env, errOut := importFiles(t, root)
		if code != 0 || !env.Data.Queued {
			t.Fatalf("exit %d %s", code, errOut)
		}
		var strategies map[string]json.RawMessage
		_ = json.Unmarshal(bankOverrides(serverBank(t), nil)["retain_strategies"], &strategies)
		if strategies["import"] == nil {
			t.Errorf("the bank was not given the import strategy: %v", strategies)
		}
	})
}

func TestImportRefusals(t *testing.T) {
	importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha"})
	file := filepath.Join(root, "a.md")
	for name, args := range map[string][]string{
		"root is a file": {file},
		"missing root":   {filepath.Join(root, "nope")},
		"malformed glob": {root, "--exclude", "["},
		"two paths":      {root, root},
	} {
		if code, _, errOut := run(t, append([]string{"import"}, args...)...); code != output.Usage {
			t.Errorf("%s: exit %d %s", name, code, errOut)
		}
	}
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[llm]\nprovider = \"claude-code\"")
	if code, _, errOut := run(t, "import", root); code != output.Usage || !strings.Contains(errOut, "--allow-subscription") {
		t.Errorf("subscription lane: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "import", root, "--plan"); code != 0 {
		t.Errorf("plan on the subscription lane: exit %d %s", code, errOut)
	}
}

func TestImportRefusesUnderMaintenance(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha"})
	l, err := state.TryAcquire(pathsOf(home).MaintenanceLock(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if code, _, errOut := run(t, "import", root); code != output.Blocked {
		t.Errorf("exit %d %s", code, errOut)
	}
}

func TestImportMaxCostNeedsPrices(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	configure(t, home, "[llm]\nmodel = \"no-such-model\"")
	root := importTree(t, map[string]string{"a.md": "alpha"})
	if code, _, errOut := run(t, "import", root, "--max-cost", "1"); code != output.Usage || !strings.Contains(errOut, "--max-tokens") {
		t.Errorf("exit %d %s", code, errOut)
	}
}

func TestImportPlanWarnsWhenPastSessionsArePending(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"a.md": "alpha"})
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.Backfill = &state.Backfill{Sessions: []string{"x.jsonl"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, env, _ := importFiles(t, root, "--plan")
	if !strings.Contains(strings.Join(env.Data.Warnings, ","), "backfill_pending") {
		t.Errorf("warnings = %v", env.Data.Warnings)
	}
	_, out, _ := run(t, "import", root, "--plan")
	if !strings.Contains(out, "past sessions") {
		t.Errorf("text does not say the import waits for past sessions:\n%s", out)
	}
}

func TestImportProjectDefaultsToTransverse(t *testing.T) {
	home := importHome(t)
	configure(t, home, "[projects]\ntransverse = [\"home\", \".claude\"]")
	root := importTree(t, map[string]string{"a.md": "alpha"})
	for flag, want := range map[string]string{"": "home", "transverse": "home", ".claude": "home", "billing": "billing"} {
		args := []string{root, "--plan"}
		if flag != "" {
			args = append(args, "--project", flag)
		}
		if _, env, _ := importFiles(t, args...); env.Data.Plan.Project != want {
			t.Errorf("--project %q: project %q, want %q", flag, env.Data.Plan.Project, want)
		}
	}
}

func TestImportEmitsNoPathsInTelemetry(t *testing.T) {
	home := importHome(t)
	root := importTree(t, map[string]string{"secret-name-zq.md": "alpha"})
	importFiles(t, root, "--plan")
	importFiles(t, root)
	evs := eventsNamed(t, home, "import")
	if len(evs) != 2 || evs[0]["action"] != "plan" || evs[1]["action"] != "queue" {
		t.Errorf("import events = %v", evs)
	}
	text := telemetryText(t, home)
	if strings.Contains(text, root) || strings.Contains(text, "secret-name-zq") {
		t.Error("telemetry holds the folder or a file name")
	}
}

func TestSpendSince(t *testing.T) {
	days := map[string]hindsight.LLMTokens{
		"2026-11-01": {Input: 100},
		"2026-11-02": {Input: 50, Output: 10, Thoughts: 5},
		"2026-11-03": {Input: 7},
	}
	tokens, cost := spendSince(days, "2026-11-02", [4]int64{20, 0, 4, 0}, retain.Prices{Input: 1, Output: 2})
	// the start day above its base: input 30, output 6, thoughts 5; the next day: input 7
	if tokens != 30+6+5+7 {
		t.Errorf("tokens = %d", tokens)
	}
	if want := (37*1.0 + 11*2.0) / 1e6; cost != want {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

// bankWrites are the fake server's bank calls that change something.
func bankWrites() []string {
	var out []string
	for _, c := range bankCalls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// serverBank is the memory bank home's config names.
func serverBank(t *testing.T) string {
	t.Helper()
	cfg, _, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Server.Bank
}

// sendFakeDocument stores a document on the fake server, as an earlier
// import would have.
func sendFakeDocument(t *testing.T, doc string, meta map[string]string) {
	t.Helper()
	cfg, sec, err := loadServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.RetainNow(context.Background(), cfg.Server.Bank, []hindsight.RetainItem{{Content: "x", DocumentID: doc, Tags: []string{}, Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
}

func TestImportPlanTextShowsSmallAmounts(t *testing.T) {
	cases := map[string]string{"0.004": "< 0.01 USD", "0": "0.00 USD", "1.234": "1.23 USD"}
	for in, want := range cases {
		var v float64
		_, _ = fmt.Sscan(in, &v)
		if got := importMoney(v, true, "USD"); got != want {
			t.Errorf("importMoney(%s) = %q, want %q", in, got, want)
		}
	}
	if got := importMoney(1, false, ""); got != "price unknown" {
		t.Errorf("unknown price: %q", got)
	}
	if got := importDuration(20 * time.Second); got != "under a minute" {
		t.Errorf("importDuration(20s) = %q", got)
	}
	if got := importDuration(90 * time.Minute); got != "about 1h30m" {
		t.Errorf("importDuration(90m) = %q", got)
	}
}
