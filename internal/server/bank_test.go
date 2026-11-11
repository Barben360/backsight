package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// fakeBank is a Hindsight that stores PATCHed overrides and records calls.
type fakeBank struct {
	banks     []string
	overrides map[string]json.RawMessage
	calls     []string
	patches   []map[string]json.RawMessage
}

func (f *fakeBank) handler(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.EscapedPath())
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/default/banks":
		var items []string
		for _, b := range f.banks {
			items = append(items, `{"bank_id":"`+b+`"}`)
		}
		_, _ = w.Write([]byte(`{"banks":[` + strings.Join(items, ",") + `]}`))
	case r.Method == http.MethodPut:
		id := strings.TrimPrefix(r.URL.Path, "/v1/default/banks/")
		found := false
		for _, b := range f.banks {
			found = found || b == id
		}
		if !found {
			f.banks = append(f.banks, id)
		}
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPatch:
		var body struct {
			Updates map[string]json.RawMessage `json:"updates"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.patches = append(f.patches, body.Updates)
		for k, v := range body.Updates {
			f.overrides[k] = v
		}
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodGet:
		ov, _ := json.Marshal(f.overrides)
		_, _ = w.Write([]byte(`{"config":{},"overrides":` + string(ov) + `}`))
	}
}

func client(t *testing.T, f *fakeBank) *hindsight.Client {
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	c, _ := hindsight.New(srv.URL, "k", 2*time.Second)
	return c
}

func TestApplyCreatesBankAndSendsTemplate(t *testing.T) {
	f := &fakeBank{overrides: map[string]json.RawMessage{"reflect_mission": json.RawMessage(`"user's own"`)}}
	res, err := ApplyBank(context.Background(), client(t, f), "claude-memory", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.Sent || res.Hash != TemplateHash() || len(f.patches) != 1 {
		t.Fatalf("%+v patches %d", res, len(f.patches))
	}
	p := f.patches[0]
	if _, ok := p["reflect_mission"]; ok {
		t.Error("a key backsight does not own was sent")
	}
	if string(f.overrides["reflect_mission"]) != `"user's own"` {
		t.Error("the user's own override was lost")
	}
	if string(p["default_observation_scopes"]) != `[["project:*"]]` || string(p["retain_default_strategy"]) != `"conversation"` {
		t.Errorf("scopes/default strategy: %s %s", p["default_observation_scopes"], p["retain_default_strategy"])
	}
	var labels []struct {
		Key    string `json:"key"`
		Tag    bool   `json:"tag"`
		Values []struct {
			Value string `json:"value"`
		} `json:"values"`
	}
	_ = json.Unmarshal(p["entity_labels"], &labels)
	if len(labels) != 1 || labels[0].Key != "kind" || !labels[0].Tag || len(labels[0].Values) != 6 {
		t.Errorf("kind labels: %s", p["entity_labels"])
	}
	var strategies map[string]map[string]any
	_ = json.Unmarshal(p["retain_strategies"], &strategies)
	for _, name := range []string{"conversation", "document", "subagent"} {
		s := strategies[name]
		instr, _ := s["retain_custom_instructions"].(string)
		if s["retain_extraction_mode"] != "custom" || s["retain_chunk_size"] != float64(30000) ||
			!strings.HasPrefix(instr, "Write every fact in English") || !strings.Contains(instr, "absolute dates") || !strings.Contains(instr, "kind") {
			t.Errorf("%s strategy: %v", name, s)
		}
	}
	sub, _ := strategies["subagent"]["retain_custom_instructions"].(string)
	if !strings.Contains(sub, "final report of a sub-agent") || !strings.Contains(sub, "holds no fact") || strings.Contains(sub, "a note someone chose to store") {
		t.Errorf("subagent strategy: %s", sub)
	}
}

func TestApplyUnchangedTemplateSendsNothing(t *testing.T) {
	f := &fakeBank{banks: []string{"claude-memory"}, overrides: map[string]json.RawMessage{}}
	res, err := ApplyBank(context.Background(), client(t, f), "claude-memory", TemplateHash(), false)
	if err != nil || res.Created || res.Sent || len(f.patches) != 0 || hasCall(f, "PUT") {
		t.Errorf("%+v %v calls %v", res, err, f.calls)
	}
	res, err = ApplyBank(context.Background(), client(t, f), "claude-memory", TemplateHash(), true)
	if err != nil || !res.Sent || res.Created || len(f.patches) != 1 || !hasCall(f, "PUT") {
		t.Errorf("--force must re-send: %+v %v", res, err)
	}
}

func TestApplyNewTemplateResends(t *testing.T) {
	f := &fakeBank{banks: []string{"claude-memory"}, overrides: map[string]json.RawMessage{}}
	res, err := ApplyBank(context.Background(), client(t, f), "claude-memory", "older-template-hash", false)
	// A new template also refreshes the profile: PUT then PATCH.
	if err != nil || res.Created || !res.Sent || !hasCall(f, "PUT") {
		t.Errorf("%+v %v", res, err)
	}
}

// A server that silently drops a key must not be reported as applied.
func TestApplyVerifiesReadBack(t *testing.T) {
	f := &fakeBank{banks: []string{"claude-memory"}, overrides: map[string]json.RawMessage{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path != "/v1/default/banks" {
			_, _ = w.Write([]byte(`{"config":{},"overrides":{"enable_observations":true}}`))
			return
		}
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c, _ := hindsight.New(srv.URL, "k", 2*time.Second)
	if _, err := ApplyBank(context.Background(), c, "claude-memory", "", false); err == nil || !strings.Contains(err.Error(), "retain_strategies") {
		t.Errorf("want a read-back error naming the missing keys, got %v", err)
	}
}

func hasCall(f *fakeBank, method string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" ") {
			return true
		}
	}
	return false
}

// After a volume wipe state.json still holds the hash: the recreated bank
// must get its template.
func TestApplyRecreatedBankGetsTemplateDespiteHash(t *testing.T) {
	f := &fakeBank{overrides: map[string]json.RawMessage{}}
	res, err := ApplyBank(context.Background(), client(t, f), "claude-memory", TemplateHash(), false)
	if err != nil || !res.Created || !res.Sent || len(f.patches) != 1 || f.banks[0] != "claude-memory" {
		t.Errorf("%+v %v banks %v", res, err, f.banks)
	}
}

func TestApplyServerErrors(t *testing.T) {
	for name, failing := range map[string]string{"list": "GET /v1/default/banks", "patch": "PATCH"} {
		f := &fakeBank{banks: []string{"claude-memory"}, overrides: map[string]json.RawMessage{}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.Method+" "+r.URL.Path, failing) {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			f.handler(w, r)
		}))
		c, _ := hindsight.New(srv.URL, "k", 2*time.Second)
		if _, err := ApplyBank(context.Background(), c, "claude-memory", "", false); err == nil {
			t.Errorf("%s failure not reported", name)
		}
		srv.Close()
	}
}

func TestMissionsStartWithTheEnglishRule(t *testing.T) {
	tmpl := bankTemplate()
	for _, m := range []string{bankMission, tmpl["retain_mission"].(string), tmpl["observations_mission"].(string)} {
		if !strings.HasPrefix(m, "Write every") || !strings.Contains(strings.SplitN(m, ".", 2)[0], "English") {
			t.Errorf("mission must open with the English rule: %q", m)
		}
	}
}

// After apply nothing drifts, even with the server's own defaults added
// inside a key; a changed value, a changed list item or a removed key does.
func TestBankDrift(t *testing.T) {
	f := &fakeBank{banks: []string{"b"}, overrides: map[string]json.RawMessage{}}
	c := client(t, f)
	ctx := context.Background()
	if _, err := ApplyBank(ctx, c, "b", "", false); err != nil {
		t.Fatal(err)
	}
	if drift, err := BankDrift(ctx, c, "b"); err != nil || len(drift) != 0 {
		t.Fatalf("right after apply: %v %v", drift, err)
	}
	var strategies map[string]map[string]any
	_ = json.Unmarshal(f.overrides["retain_strategies"], &strategies)
	strategies["conversation"]["server_default"] = 1 // added by the server: still holds
	f.overrides["retain_strategies"], _ = json.Marshal(strategies)
	if drift, _ := BankDrift(ctx, c, "b"); len(drift) != 0 {
		t.Errorf("an extra key inside a value is no drift: %v", drift)
	}
	strategies["conversation"]["retain_chunk_size"] = 8000
	f.overrides["retain_strategies"], _ = json.Marshal(strategies)
	f.overrides["enable_observations"] = json.RawMessage(`false`)
	var labels []map[string]any
	_ = json.Unmarshal(f.overrides["entity_labels"], &labels)
	labels[0]["values"] = labels[0]["values"].([]any)[1:] // one kind dropped
	f.overrides["entity_labels"], _ = json.Marshal(labels)
	delete(f.overrides, "retain_mission")
	drift, err := BankDrift(ctx, c, "b")
	if err != nil || strings.Join(drift, ",") != "enable_observations,entity_labels,retain_mission,retain_strategies" {
		t.Errorf("drift %v %v", drift, err)
	}
}

// The recorded template reads back as the one sent: same hash, and the
// bank it was sent to holds it.
func TestAppliedTemplateRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "bank-template.json")
	if _, err := LoadApplied(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent: %v", err)
	}
	if err := SaveApplied(p); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", fi.Mode().Perm())
		}
	}
	got, err := LoadApplied(p)
	if err != nil || got.Hash() != TemplateHash() || got.Name != bankName {
		t.Fatalf("%v: hash %s, want %s", err, got.Hash(), TemplateHash())
	}
	f := &fakeBank{banks: []string{"b"}, overrides: map[string]json.RawMessage{}}
	c := client(t, f)
	if _, err := ApplyBank(context.Background(), c, "b", "", false); err != nil {
		t.Fatal(err)
	}
	if drift, err := DriftFrom(context.Background(), c, "b", got.Config); err != nil || len(drift) != 0 {
		t.Errorf("the recorded template differs from the one sent: %v %v", drift, err)
	}
	// Another template (an older one without the subagent strategy) does
	// not hold on a bank given this one.
	old := Current().Config
	old["retain_strategies"] = map[string]any{"conversation": old["retain_strategies"].(map[string]any)["conversation"]}
	old["enable_observations"] = false
	if drift, _ := DriftFrom(context.Background(), c, "b", old); strings.Join(drift, ",") != "enable_observations" {
		t.Errorf("drift from the older template %v", drift)
	}
	if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApplied(p); err == nil {
		t.Error("a broken record was read")
	}
}

const importRuleText = `The source is a file imported from another memory or notes tool. Keep durable statements (decisions, conventions, pitfalls, facts about people, projects and tools) with their reason. A log line, a status, a to-do or a draft without a conclusion holds no fact.`

func TestTemplateHasImportStrategy(t *testing.T) {
	strategies, _ := Current().Config["retain_strategies"].(map[string]any)
	imp, ok := strategies["import"].(map[string]any)
	if !ok {
		t.Fatalf("retain_strategies = %v, want an import strategy", strategies)
	}
	text, _ := imp["retain_custom_instructions"].(string)
	if !strings.HasPrefix(text, rules) || !strings.HasSuffix(text, "\n"+importRuleText) {
		t.Errorf("import instructions = %q", text)
	}
	if imp["retain_chunk_size"] != 30000 || imp["retain_extraction_mode"] != "custom" {
		t.Errorf("import strategy = %v", imp)
	}
}

func TestApplyNewTemplateSendsTheImportStrategy(t *testing.T) {
	f := &fakeBank{banks: []string{"claude-memory"}, overrides: map[string]json.RawMessage{}}
	if _, err := ApplyBank(context.Background(), client(t, f), "claude-memory", "older-template-hash", false); err != nil {
		t.Fatal(err)
	}
	if len(f.patches) != 1 {
		t.Fatalf("patches = %d", len(f.patches))
	}
	var strategies map[string]json.RawMessage
	if err := json.Unmarshal(f.patches[0]["retain_strategies"], &strategies); err != nil || strategies["import"] == nil {
		t.Errorf("patched strategies = %v %v, want import", strategies, err)
	}
}

// strategyText is the instructions of every retain strategy the template
// sends, by name, in a fixed order.
func strategyText(t *testing.T) []byte {
	t.Helper()
	strategies, _ := Current().Config["retain_strategies"].(map[string]any)
	var b strings.Builder
	for _, name := range []string{"conversation", "document", "subagent", "import"} {
		s, _ := strategies[name].(map[string]any)
		text, _ := s["retain_custom_instructions"].(string)
		if text == "" {
			t.Fatalf("strategy %s has no instructions", name)
		}
		b.WriteString("## " + name + "\n" + text + "\n\n")
	}
	return []byte(b.String())
}

// The instructions the bank is given, as text, so a wording change shows
// in review (go test ./internal/server -run TestStrategyInstructionsGolden -update).
func TestStrategyInstructionsGolden(t *testing.T) {
	golden(t, "bank-strategies.golden", strategyText(t))
}

// Decision 0083 C6: one fact per delivered outcome and no process steps,
// in every strategy; the old item asking for every delivered state ("what
// was shipped, merged or configured") is gone.
func TestStrategiesKeepOutcomesAndDropProcessSteps(t *testing.T) {
	strategies, _ := Current().Config["retain_strategies"].(map[string]any)
	for _, name := range []string{"conversation", "document", "subagent", "import"} {
		s, _ := strategies[name].(map[string]any)
		text, _ := s["retain_custom_instructions"].(string)
		for _, want := range []string{
			"Keep one fact per delivered outcome: a pull request merged with what it closes, a release published, a setting changed and where.",
			"Do not extract process steps: commits made, pushes, branches created or deleted, rebases, CI runs and their status, pull requests opened or updated, waiting states.",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s strategy lacks %q", name, want)
			}
		}
		if strings.Contains(text, "delivered states (what was shipped, merged or configured, and where)") {
			t.Errorf("%s strategy still asks for every delivered state: %s", name, text)
		}
	}
	// The state kind is labelled with the same wording.
	for _, v := range kindValues {
		if v[0] == "state" && v[1] != "A delivered outcome the source says happened (merged, released or configured), and where." {
			t.Errorf("state kind: %q", v[1])
		}
	}
}

// Issue 317: a plan stays a plan (a note phrased as who does what was
// extracted as done), a constraint given to a sub-agent for its task is
// not the developer's rule, a permission for one task keeps its scope, and
// consolidation never dates a rule by the day it was mentioned.
func TestRulesKeepPlansAndTaskScopes(t *testing.T) {
	strategies, _ := Current().Config["retain_strategies"].(map[string]any)
	text := func(name string) string {
		s, _ := strategies[name].(map[string]any)
		v, _ := s["retain_custom_instructions"].(string)
		return v
	}
	plan := "A plan, an assignment or an intended step is never a delivered outcome: where one is kept, record it as planned, with its date, never as done. " +
		"Only a source that says something was done, verified, merged or released yields a state."
	for _, name := range []string{"conversation", "document", "subagent", "import"} {
		if !strings.Contains(text(name), plan) {
			t.Errorf("%s strategy lacks the plan rule:\n%s", name, text(name))
		}
	}
	for name, want := range map[string]string{
		"document":     "A note that states a plan keeps it a plan.",
		"subagent":     "Constraints the sub-agent was given for its task (do not merge, do not push) are not the developer's rules: never extract them as conventions or attribute them to the user.",
		"conversation": "A permission or restriction granted for one task is recorded with that scope (\"for the CI speedup pull request\").",
	} {
		if !strings.Contains(text(name), want) {
			t.Errorf("%s strategy lacks %q:\n%s", name, want, text(name))
		}
	}
	// the document rule no longer reads as "keep every statement" alone
	if strings.Contains(text("document"), "keep every durable statement in it, with its reason.\n") || strings.HasSuffix(text("document"), "keep every durable statement in it, with its reason.") {
		t.Errorf("document strategy keeps every statement without the plan exception:\n%s", text("document"))
	}
	mission, _ := Current().Config["observations_mission"].(string)
	for _, want := range []string{
		"A fact with no date of its own is not a change: never write \"since <date>\" from the date it was mentioned.",
		"A rule the developer stated outranks one inferred from an agent's report.",
	} {
		if !strings.Contains(mission, want) {
			t.Errorf("observations mission lacks %q:\n%s", want, mission)
		}
	}
}
