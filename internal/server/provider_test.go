package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

func dryRun(t *testing.T, status int, body string) (*hindsight.Client, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/default/banks/claude-memory/memories/dry-run-extract" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, _ := hindsight.New(srv.URL, "k", 5*time.Second)
	return c, &got
}

func TestProviderCheckPasses(t *testing.T) {
	c, req := dryRun(t, 200, `{"facts":[{"text":"The billing service keeps PostgreSQL 16 instead of MySQL because it needs partial indexes (decided 2026-10-24).","fact_type":"world"},{"text":"postgres:16 is pinned in billing/compose.yaml.","fact_type":"experience"}],"usage":{"input_tokens":900,"output_tokens":80}}`)
	res, err := CheckProvider(context.Background(), c, "claude-memory", "deepseek")
	if err != nil || res.Facts != 2 || res.InputTokens != 900 || res.OutputTokens != 80 || len(res.Sample) != 2 || !strings.HasPrefix(res.Sample[1], "postgres:16 is pinned") {
		t.Fatalf("%+v %v", res, err)
	}
	content, _ := (*req)["content"].(string)
	if (*req)["strategy"] != "conversation" || !strings.Contains(content, "PostgreSQL") ||
		(*req)["timestamp"] != "2026-10-24T09:00:00Z" || (*req)["context"] != "backsight provider check" {
		t.Errorf("request %v", *req)
	}
}

func TestProviderCheckFailures(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		inErr  string
	}{
		"no fact":       {200, `{"facts":[]}`, "no fact"},
		"empty text":    {200, `{"facts":[{"text":" ","fact_type":"world"}]}`, "malformed"},
		"unknown type":  {200, `{"facts":[{"text":"Billing keeps PostgreSQL 16.","fact_type":"banana"}]}`, "malformed"},
		"french fact":   {200, `{"facts":[{"text":"Le service billing garde PostgreSQL 16 pour les index partiels.","fact_type":"world"}]}`, "not in English"},
		"provider auth": {500, `{"detail":"LLM authentication failed: invalid api key"}`, "invalid api key"},
	}
	for name, c := range cases {
		cl, _ := dryRun(t, c.status, c.body)
		if _, err := CheckProvider(context.Background(), cl, "claude-memory", "deepseek"); err == nil || !strings.Contains(err.Error(), c.inErr) {
			t.Errorf("%s: want an error containing %q, got %v", name, c.inErr, err)
		}
	}
}

func TestLooksEnglish(t *testing.T) {
	for text, want := range map[string]bool{
		"The billing service keeps PostgreSQL 16 because it needs partial indexes.": true,
		"Deploy to la-defense-cluster on 2026-10-24.":                               true, // one stray token is fine
		"Le service garde PostgreSQL pour les index partiels.":                      false,
		"Décision : on garde la base actuelle.":                                     false,
		"Le service billing uses PostgreSQL.":                                       true,  // one French word
		"Le service pour billing uses PostgreSQL.":                                  false, // two
	} {
		if got := looksEnglish(text); got != want {
			t.Errorf("%q: %v", text, got)
		}
	}
}

// The mock echoes the French sample: only its shape is checked; every
// other provider still has to answer in English.
func TestMockProviderSkipsOnlyTheLanguageCheck(t *testing.T) {
	echo := `{"facts":[{"text":"user: On garde PostgreSQL 16 pour le service billing, pas MySQL : les index partiels sont indispensables.","fact_type":"world"}]}`
	cl, _ := dryRun(t, 200, echo)
	if _, err := CheckProvider(context.Background(), cl, "claude-memory", MockProvider); err != nil {
		t.Errorf("mock: %v", err)
	}
	if _, err := CheckProvider(context.Background(), cl, "claude-memory", "openai"); err == nil {
		t.Error("a real provider answering in French must fail")
	}
	bad, _ := dryRun(t, 200, `{"facts":[]}`)
	if _, err := CheckProvider(context.Background(), bad, "claude-memory", MockProvider); err == nil {
		t.Error("the mock returning no fact must still fail")
	}
}
