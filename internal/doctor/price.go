package doctor

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

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
	if host, ok := localEndpoint(l.BaseURL); !p.Known() && ok {
		// LM Studio, vLLM, llama.cpp behind an OpenAI-compatible
		// provider: a price would be made up. A local proxy to a billed
		// model reads the same; its prices go in config.toml. Only the
		// host is shown: a base_url can carry a key.
		c.Status, c.Summary = Info, "not billed per token: a local endpoint ("+host+")"
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

// tailnet is Tailscale's address range (CGNAT), a private network that
// netip's IsPrivate leaves out.
var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// localEndpoint reports whether baseURL is this machine, its Docker host,
// a private or tailnet address, and returns its host: a model there is not
// billed per token.
func localEndpoint(baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case h == "":
		return "", false
	case h == "localhost", h == "host.docker.internal", strings.HasSuffix(h, ".localhost"):
		return h, true
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return "", false
	}
	a = a.Unmap()
	return h, a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsLinkLocalUnicast() || tailnet.Contains(a)
}
