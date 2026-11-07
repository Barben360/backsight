package doctor

import (
	"fmt"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/retain"
)

// checkPrice reports whether the configured model has a price (issue 143):
// without one `stats` shows tokens without a cost and a backfill or import
// is capped by tokens, not money, and nothing else says so.
func checkPrice(l config.LLM, cfgFile string, cfgErr error) Check {
	c := Check{ID: "price"}
	switch {
	case cfgErr != nil:
		c.Status, c.Summary = Info, "skipped: no usable config.toml"
		return c
	case l.Provider == "claude-code":
		c.Status, c.Summary = Info, "not billed per token: the claude-code provider uses your Claude subscription"
		return c
	case l.Provider == "ollama":
		c.Status, c.Summary = Info, "not billed per token: a local Ollama model"
		return c
	}
	p := retain.PricesForLLM(l)
	if !p.Known() {
		c.Status = Warn
		c.Summary = "no price known for " + l.Model
		c.Detail = "backsight stats shows its tokens without a cost, and a backfill or an import is capped by tokens, not by " + l.Currency
		c.Fix = fmt.Sprintf("set price_input, price_cached and price_output (per million tokens, from %s's pricing page) under [llm] in %s", l.Provider, cfgFile)
		return c
	}
	from := "the built-in table"
	if l.PriceInput > 0 || l.PriceOutput > 0 {
		from = "config.toml"
	}
	c.Status = OK
	c.Summary = fmt.Sprintf("%s priced from %s: %g input, %g cached, %g output %s per million tokens", l.Model, from, p.Input, p.Cached, p.Output, p.Currency)
	return c
}
