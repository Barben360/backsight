package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

// MockProvider is Hindsight's test LLM: it echoes its input, so the
// language check cannot apply (CI's sandbox and fresh-install proof).
const MockProvider = "mock"

// CheckProvider proves auth, model, structured output and language end to
// end with a dry-run extraction on bank (nothing is stored). With the mock
// provider only the shape of the facts is checked.
func CheckProvider(ctx context.Context, c *hindsight.Client, bank, provider string) (ProviderResult, error) {
	var res ProviderResult
	out, err := c.DryRunExtract(ctx, bank, providerSample, "conversation", sampleTime)
	if err != nil {
		return res, providerError(provider, err)
	}
	if len(out.Facts) == 0 {
		return res, fmt.Errorf("the extraction LLM returned no fact from the sample: check the model and its structured-output support")
	}
	for _, f := range out.Facts {
		if strings.TrimSpace(f.Text) == "" || !factTypes[f.FactType] {
			return res, fmt.Errorf("the extraction LLM returned a malformed fact (type %q, text %q)", f.FactType, f.Text)
		}
		if provider != MockProvider && !looksEnglish(f.Text) {
			return res, fmt.Errorf("fact not in English (backsight asks the extraction LLM for English facts): %q", f.Text)
		}
		res.Sample = append(res.Sample, f.Text)
	}
	res.Facts, res.InputTokens, res.OutputTokens = len(out.Facts), out.Usage.InputTokens, out.Usage.OutputTokens
	return res, nil
}

// ProviderError is a provider check the LLM failed: KeyRejected when the
// provider refused llm_api_key, else Reason says what it answered (never
// the raw HTTP body).
type ProviderError struct {
	Provider    string
	KeyRejected bool
	Reason      string
	err         error
}

func (e *ProviderError) Error() string {
	if e.KeyRejected {
		return "the " + e.Provider + " API rejected the key in llm_api_key"
	}
	return "the extraction LLM (" + e.Provider + ") failed: " + e.Reason + ": check the model name, the base URL and your account's credit with the provider"
}

func (e *ProviderError) Unwrap() error { return e.err }

// keyRejected matches a provider's refusal of the key, which Hindsight
// passes on in an HTTP 500's detail.
var keyRejected = regexp.MustCompile(`(?i)authenticat|unauthori[sz]ed|\b401\b|(invalid|incorrect|wrong|missing)[ _-]?api[ _-]?key|invalid_api_key`)

// detailRE takes Hindsight's {"detail": "…"}, cut or not.
var detailRE = regexp.MustCompile(`"detail"\s*:\s*"((?:[^"\\]|\\.)*)`)

func providerError(provider string, err error) error {
	pe := &ProviderError{Provider: provider, err: err}
	var ae *hindsight.APIError
	if !errors.As(err, &ae) {
		pe.Reason = "no answer (" + err.Error() + ")"
		return pe
	}
	if ae.Status == 401 || ae.Status == 403 || keyRejected.MatchString(ae.Body) {
		pe.KeyRejected = true
		return pe
	}
	reason := strings.TrimSpace(ae.Body)
	cut := len(reason) >= 300 // the client keeps the first 300 bytes
	if m := detailRE.FindStringSubmatch(reason); m != nil {
		reason = strings.ReplaceAll(m[1], `\"`, `"`)
	}
	reason = strings.TrimPrefix(reason, "Fact extraction failed: ")
	const most = 160
	if len(reason) > most {
		reason, cut = reason[:most], true
	}
	if cut { // never end on a cut word
		if i := strings.LastIndexAny(reason, " ,;:"); i > 0 {
			reason = reason[:i]
		}
		reason = strings.TrimRight(reason, " ,;:") + "…"
	}
	if reason == "" {
		reason = fmt.Sprintf("HTTP %d", ae.Status)
	}
	pe.Reason = reason
	return pe
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
