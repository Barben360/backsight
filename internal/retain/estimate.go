package retain

import (
	"math"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/config"
)

// Prices per million tokens. Write is the price of an input token written to
// the provider's cache (Anthropic bills it above Input); zero means no
// surcharge.
type Prices struct {
	Input, Cached, Output, Write float64
	Currency                     string
}

// Known reports whether a cost can be computed.
func (p Prices) Known() bool { return p.Input > 0 || p.Output > 0 }

// priceTable holds defaults for models whose prices were checked
// (decision 0051); config values override them.
//
// DeepSeek bills half price off-peak and double at peak (01:00-04:00 and
// 06:00-10:00 UTC on weekdays). Nothing here models the time of day, and
// the spending cap must never under-count, so the table holds the peak
// prices (checked 2026-10-26, api-docs.deepseek.com/quick_start/pricing):
// estimates may over-state an off-peak spend by up to 2x, never the reverse.
// The retired name deepseek-v4-flash is still accepted by DeepSeek, served
// and billed as deepseek-flash, so existing installs keep a price.
// gpt-6-luna, setup's openai default, at OpenAI's standard tier (checked
// 2026-10-27, developers.openai.com/api/docs/pricing).
// Anthropic models at the standard API price, cached = cache hit (checked
// 2026-11-01, platform.claude.com/docs/en/about-claude/pricing). Haiku 5.5
// is billed higher for a prompt over 100,000 tokens ($0.50 input, $0.05
// cached, $2.50 output); the table holds the lower tier because extraction
// requests are far below that size (one chunk is about 30,000 characters).
// Anthropic bills a 5-minute cache write at 1.25x the input price, and
// Hindsight's Anthropic provider caches that way but reports writes inside
// the input count, with no separate figure. Cost therefore charges fresh
// input (input minus cached) at Write, 1.25x Input: it over-states by at
// most 25% the fresh input never written to the cache, never the reverse.
// Neither the batch discount nor the US data-residency multiplier is modelled.
var priceTable = map[string]Prices{
	"deepseek-flash":    {Input: 0.30, Cached: 0.006, Output: 1.20, Currency: "USD"},
	"deepseek-v4-flash": {Input: 0.30, Cached: 0.006, Output: 1.20, Currency: "USD"},
	"gpt-6-luna":        {Input: 0.10, Cached: 0.01, Output: 0.50, Currency: "USD"},
	"claude-haiku-5-5":  {Input: 0.10, Write: 0.125, Cached: 0.01, Output: 0.50, Currency: "USD"},
	"claude-haiku-4-5":  {Input: 1, Write: 1.25, Cached: 0.10, Output: 5, Currency: "USD"},
	"claude-sonnet-5-5": {Input: 2, Write: 2.5, Cached: 0.10, Output: 10, Currency: "USD"},
	"claude-opus-5-5":   {Input: 4, Write: 5, Cached: 0.20, Output: 20, Currency: "USD"},
	"claude-fable-5-1":  {Input: 10, Write: 12.5, Cached: 0.25, Output: 50, Currency: "USD"},
}

// PricesForLLM is PricesFor with [llm]'s model and prices: the one way
// commands and metrics get the prices they charge.
func PricesForLLM(l config.LLM) Prices {
	return PricesFor(l.Model, l.PriceInput, l.PriceCached, l.PriceOutput, l.Currency)
}

// PricesFor returns the configured prices, else the table's for model.
func PricesFor(model string, input, cached, output float64, currency string) Prices {
	if input > 0 || output > 0 {
		if cached == 0 {
			cached = input // unknown cache discount: charge cached tokens as input
		}
		return Prices{Input: input, Cached: cached, Output: output, Currency: currency}
	}
	if p, ok := priceTable[strings.ToLower(model)]; ok {
		return p
	}
	return Prices{Currency: currency}
}

// Cost of measured tokens: cached input at the cached price, fresh input at
// the higher of Input and Write, thoughts as output.
func (p Prices) Cost(input, cached, output, thoughts int64) float64 {
	fresh := input - cached
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*max(p.Input, p.Write) + float64(cached)*p.Cached + float64(output+thoughts)*p.Output) / 1e6
}

// Estimate is the expected spend of sending some content (decision 0051).
type Estimate struct {
	Chars        int64         `json:"chars"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	Cost         float64       `json:"cost"`
	Duration     time.Duration `json:"duration_ns"`
}

// EstimatedFacts is the number of facts extraction is expected to keep
// from chars characters of content: one per 600 (decision 0051).
func EstimatedFacts(chars int64) int64 { return chars / 600 }

// EstimateSession estimates one session of chars characters.
func EstimateSession(chars int64, p Prices) Estimate {
	return estimate(chars, EstimatedFacts(chars), p)
}

// EstimatedDocumentFacts is EstimatedFacts with at least one fact for a
// non-empty document: an imported note shorter than 600 characters
// usually holds one.
func EstimatedDocumentFacts(chars int64) int64 {
	if chars > 0 {
		return max(1, EstimatedFacts(chars))
	}
	return 0
}

// EstimateDocument estimates one imported file of chars characters.
func EstimateDocument(chars int64, p Prices) Estimate {
	return estimate(chars, EstimatedDocumentFacts(chars), p)
}

func estimate(chars, facts int64, p Prices) Estimate {
	chunks := int64(math.Ceil(float64(chars) / 30000))
	if chunks == 0 {
		chunks = 1
	}
	in := chunks*3300 + int64(float64(chars)/3.5) + facts*1600
	out := in * 15 / 100
	e := Estimate{Chars: chars, InputTokens: in, OutputTokens: out}
	e.Cost = p.Cost(in, 0, out, 0)
	e.Duration = time.Duration(float64(chars)/30000*float64(time.Minute)) + time.Duration(facts)*500*time.Millisecond
	return e
}

// Add sums estimates.
func (e Estimate) Add(o Estimate) Estimate {
	return Estimate{Chars: e.Chars + o.Chars, InputTokens: e.InputTokens + o.InputTokens, OutputTokens: e.OutputTokens + o.OutputTokens,
		Cost: e.Cost + o.Cost, Duration: e.Duration + o.Duration}
}
