// Command pricedrift compares the binary's built-in price table
// (internal/retain) with LiteLLM's public list,
// model_prices_and_context_window.json, so a changed price is noticed
// without users' PCs fetching anything (issue 143). The weekly
// price-drift workflow runs it and opens an issue on a difference; a
// person then checks the provider's own pricing page and changes the
// table in a release. LiteLLM is never trusted as is.
//
//	pricedrift -litellm <file>
//
// It prints a Markdown report and exits 0 when LiteLLM lists every table
// model at the table's prices, 1 when a price differs or a model is not
// listed (renamed or dropped: a person looks), 2 when the file cannot be
// read or lists fewer than half the table's models (a changed format or a
// partial download, never "no difference").
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/retain"
)

func main() {
	file := flag.String("litellm", "", "LiteLLM's model_prices_and_context_window.json")
	flag.Parse()
	os.Exit(run(*file, retain.TablePrices(), os.Stdout, os.Stderr))
}

// run compares table with the list in file, writes the report to out and
// returns the exit status.
func run(file string, table map[string]retain.Prices, out, errw io.Writer) int {
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(errw, "pricedrift:", err)
		return 2
	}
	list, err := parse(data)
	if err != nil {
		fmt.Fprintln(errw, "pricedrift:", err)
		return 2
	}
	diffs, missing := compare(table, list)
	if 2*len(missing) > len(table) {
		fmt.Fprintf(errw, "pricedrift: the list holds only %d of the table's %d models: a changed format or a partial file\n", len(table)-len(missing), len(table))
		return 2
	}
	report(out, diffs, missing)
	if len(diffs) > 0 || len(missing) > 0 {
		return 1
	}
	return 0
}

// entry is one LiteLLM model, prices per token; nil: not listed.
type entry struct {
	Input  *float64 `json:"input_cost_per_token"`
	Output *float64 `json:"output_cost_per_token"`
	Cached *float64 `json:"cache_read_input_token_cost"`
	Write  *float64 `json:"cache_creation_input_token_cost"`
}

// parse reads LiteLLM's list. An entry that does not decode (its
// "sample_spec" documents the fields in strings) is skipped; a file that
// is not a JSON object, or holds no priced model, is an error, so a
// broken download never reads as "no difference".
func parse(data []byte) (map[string]entry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("not LiteLLM's price list: %w", err)
	}
	out := map[string]entry{}
	for name, r := range raw {
		var e entry
		if json.Unmarshal(r, &e) == nil && (e.Input != nil || e.Output != nil) {
			out[strings.ToLower(name)] = e
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no priced model in the list")
	}
	return out, nil
}

// prefixes are the provider prefixes LiteLLM may file a model under.
var prefixes = []string{"", "anthropic/", "deepseek/", "openai/"}

type diff struct {
	Model, Price  string
	Table, Listed float64 // per million tokens
}

// compare returns the table's prices that differ from the list's by more
// than 0.5%, and the table models the list does not hold. Write compares
// what Cost charges fresh input, the higher of Input and Write.
func compare(table map[string]retain.Prices, list map[string]entry) (diffs []diff, missing []string) {
	for _, m := range slices.Sorted(maps.Keys(table)) {
		e, ok := lookup(list, m)
		if !ok {
			missing = append(missing, m)
			continue
		}
		p := table[m]
		listed := func(v *float64) (float64, bool) {
			if v == nil {
				return 0, false
			}
			return *v * 1e6, true
		}
		check := func(price string, have float64, v *float64) {
			if want, ok := listed(v); ok && !near(have, want) {
				diffs = append(diffs, diff{Model: m, Price: price, Table: have, Listed: want})
			}
		}
		check("input", p.Input, e.Input)
		check("output", p.Output, e.Output)
		check("cached", p.Cached, e.Cached)
		if w, ok := listed(e.Write); ok {
			in, _ := listed(e.Input)
			// Unless one side charges a write above input, the input row
			// already says it.
			above := w > in || p.Write > p.Input
			if have, want := max(p.Input, p.Write), max(in, w); above && !near(have, want) {
				diffs = append(diffs, diff{Model: m, Price: "cache write", Table: have, Listed: want})
			}
		}
	}
	return diffs, missing
}

func lookup(list map[string]entry, model string) (entry, bool) {
	for _, p := range prefixes {
		if e, ok := list[p+model]; ok {
			return e, true
		}
	}
	return entry{}, false
}

func near(a, b float64) bool { return math.Abs(a-b) <= 0.005*max(math.Abs(a), math.Abs(b)) }

func report(w io.Writer, diffs []diff, missing []string) {
	if len(diffs) == 0 && len(missing) == 0 {
		fmt.Fprintln(w, "LiteLLM lists every model of the built-in table at the table's prices.")
		return
	}
	fmt.Fprintln(w, "The built-in price table (`internal/retain/estimate.go`) differs from LiteLLM's `model_prices_and_context_window.json`. Check each price on the provider's own pricing page before changing the table: LiteLLM is a community list. The table holds DeepSeek's peak price, so a listed off-peak price reads as lower.")
	if len(diffs) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| Model | Price | Table | LiteLLM |")
		fmt.Fprintln(w, "|---|---|---|---|")
		for _, d := range diffs {
			fmt.Fprintf(w, "| `%s` | %s | %g | %g |\n", d.Model, d.Price, d.Table, round(d.Listed))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Not listed by LiteLLM (renamed or dropped?), to check by hand: %s.\n", "`"+strings.Join(missing, "`, `")+"`")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Prices per million tokens.")
}

// round drops the float noise of per-token prices times a million.
func round(v float64) float64 { return math.Round(v*1e6) / 1e6 }
