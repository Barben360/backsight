package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
)

func TestPriceWarnsForAnUnpricedModel(t *testing.T) {
	c := checkPrice(config.LLM{Provider: "openai", Model: "gpt-5.4-mini", Currency: "USD"}, "/home/u/.config/backsight/config.toml", nil)
	if c.Status != Warn || c.Summary != "no price known for gpt-5.4-mini" {
		t.Fatalf("got %+v", c)
	}
	if !strings.Contains(c.Detail, "without a cost") || !strings.Contains(c.Detail, "capped by tokens") {
		t.Errorf("detail must say what is missing: %q", c.Detail)
	}
	for _, want := range []string{"price_input", "price_cached", "price_output", "[llm]", "/home/u/.config/backsight/config.toml", "openai"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix %q lacks %q", c.Fix, want)
		}
	}
}

func TestPriceOKFromTableOrConfig(t *testing.T) {
	c := checkPrice(config.LLM{Provider: "anthropic", Model: "claude-sonnet-5-5", Currency: "USD"}, "cfg", nil)
	if c.Status != OK || c.Summary != "claude-sonnet-5-5 priced from the built-in table: 2 input, 0.1 cached, 10 output USD per million tokens" {
		t.Errorf("table: %+v", c)
	}
	// The user's own prices make an unknown model priced, and are named as theirs.
	c = checkPrice(config.LLM{Provider: "openai", Model: "gpt-5.4-mini", PriceInput: 0.75, PriceCached: 0.075, PriceOutput: 4.5, Currency: "EUR"}, "cfg", nil)
	if c.Status != OK || c.Summary != "gpt-5.4-mini priced from config.toml: 0.75 input, 0.075 cached, 4.5 output EUR per million tokens" {
		t.Errorf("config: %+v", c)
	}
}

func TestPriceNotAskedWhereNothingIsBilledPerToken(t *testing.T) {
	for _, l := range []config.LLM{
		{Provider: "claude-code", Model: "claude-haiku-4-5"},
		{Provider: "ollama", Model: "qwen3:8b"},
	} {
		if c := checkPrice(l, "cfg", nil); c.Status != Info || !strings.HasPrefix(c.Summary, "not billed per token") {
			t.Errorf("%s: %+v", l.Provider, c)
		}
	}
	if c := checkPrice(config.LLM{Provider: "openai", Model: "unknown"}, "cfg", errors.New("bad toml")); c.Status != Info || !strings.HasPrefix(c.Summary, "skipped") {
		t.Errorf("config error: %+v", c)
	}
}

// A model served on this machine or the LAN has no price to set: a warning
// there could never be cleared honestly.
func TestPriceNotAskedOfALocalEndpoint(t *testing.T) {
	for _, u := range []string{"http://localhost:1234/v1", "http://127.0.0.1:8000/v1", "http://host.docker.internal:11434/v1", "http://192.168.1.20:8080/v1", "http://[::1]:8000/v1"} {
		c := checkPrice(config.LLM{Provider: "openai", Model: "qwen3-8b", BaseURL: u, Currency: "USD"}, "cfg", nil)
		if c.Status != Info || c.Summary != "not billed per token: a local endpoint ("+u+")" {
			t.Errorf("%s: %+v", u, c)
		}
	}
	// A public endpoint stays a warning, and a priced local model is still
	// reported with its price.
	for _, u := range []string{"https://api.openai.com/v1", "https://openrouter.ai/api/v1", "", "::bad"} {
		if c := checkPrice(config.LLM{Provider: "openai", Model: "qwen3-8b", BaseURL: u, Currency: "USD"}, "cfg", nil); c.Status != Warn {
			t.Errorf("%q: %+v", u, c)
		}
	}
	if c := checkPrice(config.LLM{Provider: "openai", Model: "qwen3-8b", BaseURL: "http://localhost:1234/v1", PriceOutput: 0.2, Currency: "USD"}, "cfg", nil); c.Status != OK {
		t.Errorf("priced local model: %+v", c)
	}
}

// Only an output price set: still the user's prices, input at 0.
func TestPriceOutputOnlyIsTheConfigs(t *testing.T) {
	c := checkPrice(config.LLM{Provider: "openai", Model: "gpt-5.4-mini", PriceOutput: 4.5, Currency: "USD"}, "cfg", nil)
	if c.Status != OK || c.Summary != "gpt-5.4-mini priced from config.toml: 0 input, 0 cached, 4.5 output USD per million tokens" {
		t.Errorf("%+v", c)
	}
}
