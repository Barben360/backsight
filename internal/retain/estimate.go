package retain

import (
	"math"
	"strings"
	"time"
)

// Prices per million tokens.
type Prices struct {
	Input, Cached, Output float64
	Currency              string
}

// Known reports whether a cost can be computed.
func (p Prices) Known() bool { return p.Input > 0 || p.Output > 0 }

// priceTable holds defaults for models whose prices were checked
// (decision 0051); config values override them.
var priceTable = map[string]Prices{
	"deepseek-v4-flash": {Input: 0.14, Cached: 0.0028, Output: 0.28, Currency: "USD"},
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

// Cost of measured tokens: cached input at the cached price, thoughts as
// output.
func (p Prices) Cost(input, cached, output, thoughts int64) float64 {
	fresh := input - cached
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*p.Input + float64(cached)*p.Cached + float64(output+thoughts)*p.Output) / 1e6
}

// Estimate is the expected spend of sending some content (decision 0051).
type Estimate struct {
	Chars        int64         `json:"chars"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	Cost         float64       `json:"cost"`
	Duration     time.Duration `json:"duration_ns"`
}

// EstimateSession estimates one session of chars characters.
func EstimateSession(chars int64, p Prices) Estimate {
	chunks := int64(math.Ceil(float64(chars) / 30000))
	if chunks == 0 {
		chunks = 1
	}
	facts := chars / 600
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
