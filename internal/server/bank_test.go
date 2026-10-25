package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	for _, name := range []string{"conversation", "document"} {
		s := strategies[name]
		instr, _ := s["retain_custom_instructions"].(string)
		if s["retain_extraction_mode"] != "custom" || s["retain_chunk_size"] != float64(30000) ||
			!strings.HasPrefix(instr, "Write every fact in English") || !strings.Contains(instr, "absolute dates") || !strings.Contains(instr, "kind") {
			t.Errorf("%s strategy: %v", name, s)
		}
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
