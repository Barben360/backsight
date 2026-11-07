package doctor

import (
	"fmt"
	"net/netip"
	"net/url"

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
	if !p.Known() && localEndpoint(l.BaseURL) {
		// LM Studio, vLLM, llama.cpp behind an OpenAI-compatible
		// provider: a price would be made up.
		c.Status, c.Summary = Info, "not billed per token: a local endpoint ("+l.BaseURL+")"
		return c
	}
	if !p.Known() {
		c.Status = Warn
		c.Summary = "no price known for " + l.Model
		c.Detail = "backsight stats shows its tokens without a cost, a backfill or an import is capped by tokens, not by " + l.Currency + ", and retain backfill --max-cost is refused"
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

// localEndpoint reports whether baseURL is this machine, its Docker host
// or a private address: a model there is not billed per token.
func localEndpoint(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	switch h := u.Hostname(); h {
	case "localhost", "host.docker.internal":
		return true
	default:
		a, err := netip.ParseAddr(h)
		return err == nil && (a.IsLoopback() || a.IsPrivate())
	}
}
