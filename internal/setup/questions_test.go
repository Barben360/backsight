package setup

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
)

func find(t *testing.T, qs []Question, id string) Question {
	t.Helper()
	for _, q := range qs {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("no question %q in %v", id, ids(qs))
	return Question{}
}

func ids(qs []Question) []string {
	var out []string
	for _, q := range qs {
		out = append(out, q.ID)
	}
	return out
}

func has(qs []Question, id string) bool {
	for _, q := range qs {
		if q.ID == id {
			return true
		}
	}
	return false
}

func TestFreshInstallAsksEverythingWithRecommendations(t *testing.T) {
	qs, extra, err := Resolve(Inputs{Backfill: func(string) (int, string) { return 12, "≈ $0.40" }})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"provider", "model", "retention", "backups", "backfill", "consent"}
	if got := strings.Join(ids(qs), ","); got != strings.Join(want, ",") {
		t.Fatalf("questions %s, want %s", got, strings.Join(want, ","))
	}
	for _, q := range qs {
		if !q.Asked() {
			t.Errorf("%s resolved without an answer: %+v", q.ID, q)
		}
	}
	if p := find(t, qs, "provider"); p.Recommended != "deepseek" || !p.Required {
		t.Errorf("provider: %+v", p)
	}
	if m := find(t, qs, "model"); m.Default != "deepseek-flash" {
		t.Errorf("model default %q", m.Default)
	}
	if b := find(t, qs, "backfill"); b.Options[0].Cost != "≈ $0.40" || !strings.Contains(b.Prompt, "12") {
		t.Errorf("backfill question lacks its plan: %+v", b)
	}
	if extra["api_port"] != "8888" || extra["ui_port"] != "9999" {
		t.Errorf("hidden defaults: %v", extra)
	}
}

func TestNonInteractiveDefaultsLeaveRequiredQuestionsOpen(t *testing.T) {
	qs, _, err := Resolve(Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	missing := Defaults(qs)
	if strings.Join(missing, ",") != "provider,consent" {
		t.Fatalf("missing %v, want provider and consent", missing)
	}
	if r := find(t, qs, "retention"); r.Value != "3650" || r.Source != "default" {
		t.Errorf("retention not defaulted: %+v", r)
	}
}

func TestProviderShapesTheFollowUpQuestions(t *testing.T) {
	cases := []struct {
		provider        string
		model           string
		baseURL, ack    bool
		backfillDefault string
	}{
		{"deepseek", "deepseek-flash", false, false, "yes"},
		{"ollama", "qwen3:8b", true, false, "yes"},
		{"anthropic", "claude-haiku-5-5", true, false, "yes"},
		{"claude-code", "claude-haiku-5-5", false, true, "no"},
	}
	for _, c := range cases {
		qs, _, err := Resolve(Inputs{Answers: map[string]string{"provider": c.provider}, Backfill: func(string) (int, string) { return 3, "" }})
		if err != nil {
			t.Fatalf("%s: %v", c.provider, err)
		}
		if has(qs, "base_url") != c.baseURL || has(qs, "ack_subscription") != c.ack {
			t.Errorf("%s: questions %v", c.provider, ids(qs))
		}
		if m := find(t, qs, "model"); m.Default != c.model {
			t.Errorf("%s: model default %q, want %q", c.provider, m.Default, c.model)
		}
		if b := find(t, qs, "backfill"); b.Default != c.backfillDefault {
			t.Errorf("%s: backfill default %q", c.provider, b.Default)
		}
	}
	qs, _, _ := Resolve(Inputs{Answers: map[string]string{"provider": "ollama"}})
	if b := find(t, qs, "base_url"); b.Default != "http://host.docker.internal:11434" {
		t.Errorf("ollama base URL default %q", b.Default)
	}
	// On Linux Ollama listens on 127.0.0.1, out of the container's reach:
	// the question says so and where the fix is.
	if b := find(t, qs, "base_url"); !strings.Contains(b.Prompt, "OLLAMA_HOST") || !strings.Contains(b.Prompt, "backsight guide install#local-models-with-ollama") {
		t.Errorf("ollama base URL prompt %q does not say how to make Ollama reachable", b.Prompt)
	}
}

func TestUnlistedProviderNeedsAModel(t *testing.T) { // see also TestNonInteractiveAnthropicNeedsNoBaseURL
	qs, _, err := Resolve(Inputs{Answers: map[string]string{"provider": "groq"}})
	if err != nil {
		t.Fatal(err)
	}
	if missing := Defaults(qs); !strings.Contains(strings.Join(missing, ","), "model") {
		t.Errorf("an unlisted provider must not get a default model: missing %v", missing)
	}
}

func TestBackfillPlanFollowsTheChosenModel(t *testing.T) {
	var asked string
	_, _, err := Resolve(Inputs{Answers: map[string]string{"provider": "anthropic"}, Backfill: func(m string) (int, string) { asked = m; return 1, "" }})
	if err != nil || asked != "claude-haiku-5-5" {
		t.Errorf("backfill estimated for %q (%v), want the anthropic preset", asked, err)
	}
	_, _, _ = Resolve(Inputs{Answers: map[string]string{"provider": "anthropic", "model": "claude-sonnet-5-5"}, Backfill: func(m string) (int, string) { asked = m; return 1, "" }})
	if asked != "claude-sonnet-5-5" {
		t.Errorf("backfill estimated for %q, want the answered model", asked)
	}
}

func TestNoBackfillOrReferenceQuestionWithoutCause(t *testing.T) {
	qs, _, _ := Resolve(Inputs{Backfill: func(string) (int, string) { return 0, "" }})
	if has(qs, "backfill") || has(qs, "reference") {
		t.Errorf("questions %v", ids(qs))
	}
	qs, _, _ = Resolve(Inputs{ReferencePieces: 4})
	if r := find(t, qs, "reference"); r.Default != "retire" || !strings.Contains(r.Prompt, "4 pieces") {
		t.Errorf("reference: %+v", r)
	}
}

func TestInvalidAnswersAreUsageErrors(t *testing.T) {
	for _, a := range []map[string]string{
		{"colour": "blue"},
		{"backups": "maybe"},
		{"retention": "0"},
		{"retention": "forever"},
		{"consent": "sure"},
		{"api_port": "70000"},
		{"api_port": "9999"}, // equals the ui port
		{"project": "Backsight"},
		{"project": "../x"},
		{"scheduler": "sometimes"},
		{"provider": "claude-code", "ack_subscription": "no"},
	} {
		_, _, err := Resolve(Inputs{Answers: a})
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%v accepted (%v)", a, err)
		}
	}
	if _, _, err := Resolve(Inputs{Answers: map[string]string{"retention": "90", "provider": "my-gateway", "model": "m"}}); err != nil {
		t.Errorf("free-text answers refused: %v", err)
	}
}

func TestAnswersBeatSavedAnswersBeatDefaults(t *testing.T) {
	qs, extra, err := Resolve(Inputs{Answers: map[string]string{"backups": "off", "ui_port": "9998"}, Saved: map[string]string{"backups": "on", "provider": "openai", "ui_port": "9000"}})
	if err != nil {
		t.Fatal(err)
	}
	if b := find(t, qs, "backups"); b.Value != "off" || b.Source != "answer" {
		t.Errorf("backups %+v", b)
	}
	if p := find(t, qs, "provider"); p.Value != "openai" || p.Source != "saved" {
		t.Errorf("provider %+v", p)
	}
	if extra["ui_port"] != "9998" {
		t.Errorf("ui_port %q", extra["ui_port"])
	}
}

func adopted(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default(paths.Dirs{Data: "/d"}, "/home/u")
	cfg.Server.URL, cfg.Server.UIPort = "http://127.0.0.1:18888", 19999
	cfg.LLM.Provider, cfg.LLM.Model = "deepseek", "deepseek-v4-flash"
	cfg.Client.RetentionDays = 3650
	return &cfg
}

func TestAnExistingConfigAnswersAndRefusesContradictions(t *testing.T) {
	cfg := adopted(t)
	qs, extra, err := Resolve(Inputs{Config: cfg, Answers: map[string]string{"provider": "deepseek"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"provider", "model", "retention", "backups"} {
		if q := find(t, qs, id); q.Source != "config" || q.Asked() {
			t.Errorf("%s not taken from the config: %+v", id, q)
		}
	}
	if extra["api_port"] != "18888" || extra["ui_port"] != "19999" {
		t.Errorf("ports %v, want the config's", extra)
	}
	if !find(t, qs, "consent").Asked() {
		t.Error("consent must still be asked when adopting")
	}
	for _, a := range []map[string]string{{"provider": "openai"}, {"api_port": "8888"}, {"backups": "off"}} {
		_, _, err := Resolve(Inputs{Config: cfg, Answers: a})
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "config.toml") {
			t.Errorf("%v contradicts the config but got %v", a, err)
		}
	}
	cfg.Client.RetentionDays = 0 // not managed yet: still a question
	qs, _, _ = Resolve(Inputs{Config: cfg})
	if !find(t, qs, "retention").Asked() {
		t.Error("retention_days = 0 must leave the retention question open")
	}
}

func TestQuestionJSONCarriesAsked(t *testing.T) {
	b, _ := json.Marshal(Question{ID: "x", Value: "v", Source: "answer"})
	if !strings.Contains(string(b), `"asked":false`) || !strings.Contains(string(b), `"id":"x"`) {
		t.Errorf("%s", b)
	}
}

func TestSavedModelFollowsOnlyItsProvider(t *testing.T) {
	saved := map[string]string{"provider": "deepseek", "model": "deepseek-v4-flash", "backups": "off"}
	qs, _, err := Resolve(Inputs{Answers: map[string]string{"provider": "openai"}, Saved: saved})
	if err != nil {
		t.Fatal(err)
	}
	Defaults(qs)
	if m := find(t, qs, "model"); m.Value != "gpt-6-luna" {
		t.Errorf("model %q (%s): the old provider's model leaked", m.Value, m.Source)
	}
	if b := find(t, qs, "backups"); b.Value != "off" || b.Source != "saved" {
		t.Errorf("unrelated saved answers must stay: %+v", b)
	}
	qs, _, _ = Resolve(Inputs{Saved: saved})
	if m := find(t, qs, "model"); m.Value != "deepseek-v4-flash" || m.Source != "saved" {
		t.Errorf("same provider: saved model expected, got %+v", m)
	}
}

func TestEmptyAnswersAreNoAnswers(t *testing.T) {
	qs, _, err := Resolve(Inputs{Answers: map[string]string{"provider": "claude-code", "ack_subscription": "", "consent": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if missing := Defaults(qs); strings.Join(missing, ",") != "ack_subscription" {
		t.Errorf("missing %v, want the acknowledgement", missing)
	}
	qs, _, _ = Resolve(Inputs{Answers: map[string]string{"provider": "openai", "base_url": "", "consent": "yes"}})
	if missing := Defaults(qs); len(missing) != 0 {
		t.Errorf("an empty base URL is the provider's own: missing %v", missing)
	}
}

func TestNonInteractiveAnthropicNeedsNoBaseURL(t *testing.T) {
	qs, _, _ := Resolve(Inputs{Answers: map[string]string{"provider": "anthropic"}})
	if missing := Defaults(qs); strings.Join(missing, ",") != "consent" {
		t.Errorf("missing %v, want only consent", missing)
	}
	if b := find(t, qs, "base_url"); b.Value != "" || b.Source != "default" {
		t.Errorf("base_url %+v", b)
	}
	qs, _, _ = Resolve(Inputs{Answers: map[string]string{"provider": "groq"}})
	if missing := Defaults(qs); strings.Join(missing, ",") != "model,consent" {
		t.Errorf("groq: missing %v, want model,consent", missing)
	}
}

// The provider choices say what a user accepts with them: the subscription
// lane quotes Anthropic's credential clause with its link and points to an
// API key, and DeepSeek says where it stores data.
func TestProviderChoicesStateTheirTerms(t *testing.T) {
	qs, _, err := Resolve(Inputs{Answers: map[string]string{"provider": "claude-code"}})
	if err != nil {
		t.Fatal(err)
	}
	options := map[string]Option{}
	for _, o := range find(t, qs, "provider").Options {
		options[o.Value] = o
	}
	ack := find(t, qs, "ack_subscription").Prompt
	for _, text := range []string{options["claude-code"].Caveat, ack} {
		for _, want := range []string{"may not collect, store, or intermediate Claude.ai credentials or session tokens",
			"https://code.claude.com/docs/en/legal-and-compliance", "API key"} {
			if !strings.Contains(text, want) {
				t.Errorf("subscription lane lacks %q: %s", want, text)
			}
		}
	}
	if c := options["deepseek"].Caveat; !strings.Contains(c, "People's Republic of China") {
		t.Errorf("deepseek caveat does not say where data is stored: %q", c)
	}
	for _, p := range []string{"anthropic", "openai", "ollama"} {
		if strings.Contains(options[p].Caveat, "China") {
			t.Errorf("%s caveat names China: %q", p, options[p].Caveat)
		}
	}
}
