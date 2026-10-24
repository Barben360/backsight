package server

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/hindsight"
)

// providerSample is a short bilingual session with one durable decision:
// the extractor must return at least one English fact from it.
const providerSample = `user: On garde PostgreSQL 16 pour le service billing, pas MySQL : les index partiels sont indispensables.
assistant: Understood. I pinned postgres:16 in billing/compose.yaml and recorded the reason in docs/decisions.md.
user: Parfait, merci.`

var sampleTime = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

var factTypes = map[string]bool{"world": true, "experience": true, "opinion": true, "observation": true}

// ProviderResult summarises a passing provider check.
type ProviderResult struct {
	Facts        int      `json:"facts"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	Sample       []string `json:"sample_facts"`
}

// CheckProvider proves auth, model, structured output and language end to
// end with a dry-run extraction on bank (nothing is stored).
func CheckProvider(ctx context.Context, c *hindsight.Client, bank string) (ProviderResult, error) {
	var res ProviderResult
	out, err := c.DryRunExtract(ctx, bank, providerSample, "conversation", sampleTime)
	if err != nil {
		return res, fmt.Errorf("the extraction LLM failed: %w", err)
	}
	if len(out.Facts) == 0 {
		return res, fmt.Errorf("the extraction LLM returned no fact from the sample: check the model and its structured-output support")
	}
	for _, f := range out.Facts {
		if strings.TrimSpace(f.Text) == "" || !factTypes[f.FactType] {
			return res, fmt.Errorf("the extraction LLM returned a malformed fact (type %q, text %q)", f.FactType, f.Text)
		}
		if !looksEnglish(f.Text) {
			return res, fmt.Errorf("fact not in English (decision 0039): %q", f.Text)
		}
		res.Sample = append(res.Sample, f.Text)
	}
	res.Facts, res.InputTokens, res.OutputTokens = len(out.Facts), out.Usage.InputTokens, out.Usage.OutputTokens
	return res, nil
}

var frenchWords = map[string]bool{
	"le": true, "la": true, "les": true, "des": true, "une": true, "est": true, "pour": true, "pas": true,
	"avec": true, "dans": true, "sur": true, "que": true, "qui": true, "du": true, "aux": true, "sont": true,
	"nous": true, "mais": true, "donc": true, "cette": true, "ces": true, "garde": true, "décision": true,
}

// looksEnglish allows one French-looking word (a name, a path segment).
func looksEnglish(text string) bool {
	n := 0
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if frenchWords[w] {
			n++
		}
	}
	return n <= 1
}
