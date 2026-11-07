package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/retain"
)

func ptr(v float64) *float64 { return &v }

// perToken is LiteLLM's unit for a price per million tokens.
func perToken(perMillion float64) *float64 { return ptr(perMillion / 1e6) }

var table = map[string]retain.Prices{
	"claude-sonnet-5-5": {Input: 2, Write: 2.5, Cached: 0.10, Output: 10},
	"deepseek-flash":    {Input: 0.30, Cached: 0.006, Output: 1.20},
	"gpt-6-luna":        {Input: 0.10, Cached: 0.01, Output: 0.50},
}

func matching() map[string]entry {
	return map[string]entry{
		"anthropic/claude-sonnet-5-5": {Input: perToken(2), Output: perToken(10), Cached: perToken(0.10), Write: perToken(2.5)},
		"deepseek/deepseek-flash":     {Input: perToken(0.30), Output: perToken(1.20), Cached: perToken(0.006), Write: ptr(0)}, // LiteLLM lists 0 for no write price
		"gpt-6-luna":                  {Input: perToken(0.10), Output: perToken(0.50), Cached: perToken(0.01), Write: perToken(0.10)},
	}
}

func TestMatchingListHasNoDiff(t *testing.T) {
	diffs, missing := compare(table, matching())
	if len(diffs) != 0 || len(missing) != 0 {
		t.Errorf("diffs %+v, missing %v", diffs, missing)
	}
}

func TestEachChangedPriceIsADiff(t *testing.T) {
	list := matching()
	s := list["anthropic/claude-sonnet-5-5"]
	s.Output = perToken(12)
	s.Write = perToken(4) // a longer cache TTL: fresh input would cost more than the table charges
	list["anthropic/claude-sonnet-5-5"] = s
	d := list["deepseek/deepseek-flash"]
	d.Input = perToken(0.15) // an off-peak price: lower than the table's peak
	list["deepseek/deepseek-flash"] = d
	diffs, _ := compare(table, list)
	want := []diff{
		{Model: "claude-sonnet-5-5", Price: "output", Table: 10, Listed: 12},
		{Model: "claude-sonnet-5-5", Price: "cache write", Table: 2.5, Listed: 4},
		{Model: "deepseek-flash", Price: "input", Table: 0.30, Listed: 0.15},
	}
	if len(diffs) != len(want) {
		t.Fatalf("diffs %+v, want %+v", diffs, want)
	}
	for i, w := range want {
		g := diffs[i]
		if g.Model != w.Model || g.Price != w.Price || !near(g.Table, w.Table) || !near(g.Listed, w.Listed) {
			t.Errorf("diff %d: got %+v, want %+v", i, g, w)
		}
	}
}

// A price the list leaves out is not compared; a model it leaves out is
// named apart from the price rows.
func TestUnlistedIsNotADiff(t *testing.T) {
	list := matching()
	delete(list, "gpt-6-luna")
	s := list["anthropic/claude-sonnet-5-5"]
	s.Cached, s.Write = nil, nil
	list["anthropic/claude-sonnet-5-5"] = s
	diffs, missing := compare(table, list)
	if len(diffs) != 0 || len(missing) != 1 || missing[0] != "gpt-6-luna" {
		t.Errorf("diffs %+v, missing %v", diffs, missing)
	}
}

func TestSmallRoundingIsNotADiff(t *testing.T) {
	list := matching()
	s := list["anthropic/claude-sonnet-5-5"]
	s.Input = perToken(2.004) // 0.2%
	list["anthropic/claude-sonnet-5-5"] = s
	if diffs, _ := compare(table, list); len(diffs) != 0 {
		t.Errorf("diffs %+v", diffs)
	}
	s.Input = perToken(2.02) // 1%
	list["anthropic/claude-sonnet-5-5"] = s
	if diffs, _ := compare(table, list); len(diffs) != 1 {
		t.Errorf("a 1%% change must be a diff: %+v", diffs)
	}
}

func TestParse(t *testing.T) {
	list, err := parse([]byte(`{
		"sample_spec": {"input_cost_per_token": "cost per input token", "max_tokens": "LEGACY"},
		"Claude-Sonnet-5-5": {"input_cost_per_token": 2e-6, "output_cost_per_token": 1e-5, "cache_read_input_token_cost": 1e-7},
		"some-embedding": {"mode": "embedding"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := list["claude-sonnet-5-5"]
	if len(list) != 1 || !ok || *e.Input != 2e-6 || *e.Output != 1e-5 || *e.Cached != 1e-7 || e.Write != nil {
		t.Errorf("list %+v", list)
	}
	for _, bad := range []string{`<html>rate limited</html>`, `{}`, `{"sample_spec": {"input_cost_per_token": "x"}}`, `[]`} {
		if _, err := parse([]byte(bad)); err == nil {
			t.Errorf("%q: no error, a broken download would read as no difference", bad)
		}
	}
}

func TestReport(t *testing.T) {
	var b bytes.Buffer
	report(&b, []diff{{Model: "claude-sonnet-5-5", Price: "output", Table: 10, Listed: 12.000000000000002}}, []string{"gpt-6-luna"})
	out := b.String()
	for _, want := range []string{"| `claude-sonnet-5-5` | output | 10 | 12 |", "provider's own pricing page", "Not listed by LiteLLM (renamed or dropped?), to check by hand: `gpt-6-luna`."} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	report(&b, nil, nil)
	if !strings.HasPrefix(b.String(), "LiteLLM lists every model") || strings.Contains(b.String(), "|") {
		t.Errorf("no diff: %q", b.String())
	}
	// Only a missing model: the issue must not open with "matches".
	b.Reset()
	report(&b, nil, []string{"gpt-6-luna"})
	if out := b.String(); strings.Contains(out, "lists every model") || strings.Contains(out, "|") || !strings.Contains(out, "`gpt-6-luna`") {
		t.Errorf("missing only: %q", out)
	}
}

// The real table's names are lower case, as lookup expects.
func TestTableNamesAreLowerCase(t *testing.T) {
	for m := range retain.TablePrices() {
		if m != strings.ToLower(m) {
			t.Errorf("%q", m)
		}
	}
}

// writeList writes list as LiteLLM's JSON (prices per token).
func writeList(t *testing.T, list map[string]entry) string {
	t.Helper()
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(f, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// The exit status is what the workflow acts on: 0 nothing, 1 an issue,
// 2 a failed run.
func TestRunExitStatus(t *testing.T) {
	changed := matching()
	s := changed["gpt-6-luna"]
	s.Output = perToken(0.60)
	changed["gpt-6-luna"] = s
	oneMissing := matching()
	delete(oneMissing, "gpt-6-luna")
	mostMissing := map[string]entry{"gpt-6-luna": matching()["gpt-6-luna"], "some-other-model": {Input: perToken(1)}}
	garbage := filepath.Join(t.TempDir(), "garbage.json")
	if err := os.WriteFile(garbage, []byte("<html>rate limited</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, file string
		want       int
		says       string
	}{
		{"matching", writeList(t, matching()), 0, "lists every model"},
		{"a changed price", writeList(t, changed), 1, "| `gpt-6-luna` | output | 0.5 | 0.6 |"},
		{"a model no longer listed", writeList(t, oneMissing), 1, "`gpt-6-luna`"},
		{"most models missing", writeList(t, mostMissing), 2, "only 1 of the table's 3 models"},
		{"not JSON", garbage, 2, "not LiteLLM's price list"},
		{"no file", filepath.Join(t.TempDir(), "none.json"), 2, "pricedrift:"},
	} {
		var out, errw bytes.Buffer
		if got := run(c.file, table, &out, &errw); got != c.want {
			t.Errorf("%s: exit %d, want %d (out %q, err %q)", c.name, got, c.want, out.String(), errw.String())
		}
		if !strings.Contains(out.String()+errw.String(), c.says) {
			t.Errorf("%s: output lacks %q: out %q, err %q", c.name, c.says, out.String(), errw.String())
		}
		if c.want == 2 && out.Len() > 0 {
			t.Errorf("%s: a failed run wrote a report: %q", c.name, out.String())
		}
	}
}
