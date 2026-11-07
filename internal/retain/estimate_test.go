package retain

import (
	"math"
	"testing"

	"github.com/Barben360/backsight/internal/config"
)

func TestEstimatedFactsMatchesEstimateSession(t *testing.T) {
	p := Prices{Input: 1, Output: 2}
	for _, c := range []int64{0, 599, 600, 61234} {
		if got := EstimatedFacts(c); got != c/600 {
			t.Errorf("EstimatedFacts(%d) = %d, want %d", c, got, c/600)
		}
		chunks := (c + 29999) / 30000
		if chunks == 0 {
			chunks = 1
		}
		want := chunks*3300 + int64(float64(c)/3.5) + EstimatedFacts(c)*1600
		if e := EstimateSession(c, p); e.InputTokens != want {
			t.Errorf("EstimateSession(%d).InputTokens = %d, want %d", c, e.InputTokens, want)
		}
	}
}

func TestEstimateDocumentCountsAtLeastOneFact(t *testing.T) {
	p := Prices{Input: 1, Output: 2}
	small := EstimateDocument(100, p)
	if want := EstimateSession(100, p).InputTokens + 1600; small.InputTokens != want {
		t.Errorf("small document input tokens = %d, want %d (one fact)", small.InputTokens, want)
	}
	if big := EstimateDocument(60000, p); big != EstimateSession(60000, p) {
		t.Errorf("large document = %+v, want the session estimate %+v", big, EstimateSession(60000, p))
	}
	if EstimatedDocumentFacts(100) != 1 || EstimatedDocumentFacts(0) != 0 || EstimatedDocumentFacts(1300) != 2 {
		t.Errorf("document facts = %d %d %d", EstimatedDocumentFacts(100), EstimatedDocumentFacts(0), EstimatedDocumentFacts(1300))
	}
}

// Anthropic bills a cache write at 1.25x input and the stats cannot tell
// writes apart, so fresh input is charged at the write price.
func TestAnthropicFreshInputIsChargedAtTheCacheWritePrice(t *testing.T) {
	sonnet := PricesFor("claude-sonnet-5-5", 0, 0, 0, "USD") // input 2, write 2.5, cached 0.10, output 10
	if got, want := sonnet.Cost(1_000_000, 0, 0, 0), 2.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("uncached input cost %v, want %v (1.25x the input price)", got, want)
	}
	// 1M fresh at 2.5 + 1M cached at 0.10, never at the write price.
	if got, want := sonnet.Cost(2_000_000, 1_000_000, 0, 0), 2.6; math.Abs(got-want) > 1e-9 {
		t.Errorf("fresh+cached cost %v, want %v", got, want)
	}
	if got, want := sonnet.Cost(1_000_000, 1_000_000, 0, 0), 0.10; math.Abs(got-want) > 1e-9 {
		t.Errorf("cached-only cost %v, want %v", got, want)
	}
	if got, want := sonnet.Cost(0, 0, 1_000_000, 0), 10.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("output cost %v, want %v", got, want)
	}
}

func TestNoWriteSurchargeOutsideTheAnthropicTable(t *testing.T) {
	ds := PricesFor("deepseek-flash", 0, 0, 0, "USD")
	if got, want := ds.Cost(1_000_000, 0, 0, 0), 0.30; math.Abs(got-want) > 1e-9 {
		t.Errorf("deepseek input cost %v, want %v", got, want)
	}
	// The user's own prices are used as given.
	cfg := PricesFor("claude-sonnet-5-5", 3, 0.3, 15, "EUR")
	if cfg.Write != 0 {
		t.Errorf("configured prices must carry no write surcharge: %+v", cfg)
	}
	if got, want := cfg.Cost(1_000_000, 0, 0, 0), 3.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("configured input cost %v, want %v", got, want)
	}
}

func TestAnthropicEstimateInputIs25PercentHigher(t *testing.T) {
	// 30000 chars: 1 chunk, 50 facts -> 3300 + 8571 + 80000 input tokens.
	e := EstimateSession(30000, PricesFor("claude-sonnet-5-5", 0, 0, 0, "USD"))
	in, out := e.InputTokens, e.OutputTokens
	if want := (float64(in)*2.5 + float64(out)*10) / 1e6; math.Abs(e.Cost-want) > 1e-12 {
		t.Errorf("estimate cost %v, want %v (input at 1.25x)", e.Cost, want)
	}
	plain := EstimateSession(30000, Prices{Input: 2, Output: 10})
	if diff, want := e.Cost-plain.Cost, float64(in)*0.5/1e6; math.Abs(diff-want) > 1e-12 {
		t.Errorf("surcharge %v, want %v", diff, want)
	}
}

// PricesForLLM maps each [llm] field to its price, and falls back to the
// table for the configured model.
func TestPricesForLLM(t *testing.T) {
	got := PricesForLLM(config.LLM{Model: "claude-sonnet-5-5", PriceInput: 3, PriceCached: 0.3, PriceOutput: 15, Currency: "EUR"})
	if want := (Prices{Input: 3, Cached: 0.3, Output: 15, Currency: "EUR"}); got != want {
		t.Errorf("configured: got %+v, want %+v", got, want)
	}
	if got, want := PricesForLLM(config.LLM{Model: "claude-sonnet-5-5", Currency: "USD"}), priceTable["claude-sonnet-5-5"]; got != want {
		t.Errorf("table: got %+v, want %+v", got, want)
	}
	if PricesForLLM(config.LLM{Model: "some-local-model", Currency: "USD"}).Known() {
		t.Error("an unknown model has no price")
	}
}
