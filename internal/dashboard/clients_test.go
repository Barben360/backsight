package dashboard

import (
	"strings"
	"testing"
)

// clientHarness is a data harness whose history gives some sessions to
// Codex, and its session cookie.
func clientHarness(t *testing.T) (*harness, []string) {
	t.Helper()
	old := dataSpec
	dataSpec.Codex = true
	t.Cleanup(func() { dataSpec = old })
	h := dataHarness(t)
	dataSpec = old
	return h, cookieHdr(h.login())
}

// On a PC where both tools were active, the scope line offers every tool or
// one, and the Overview splits the window by tool. Choosing Codex counts
// its activity alone: the choice says so, every link of the page keeps it
// (the choice's own links aside), the forms carry it, the commands name it,
// and there is no split. A tool the dashboard does not know is refused.
func TestClientFilterOnThePages(t *testing.T) {
	h, ck := clientHarness(t)
	get := func(path string, want int) string {
		t.Helper()
		resp := h.do("GET", path, ck...)
		b := body(t, resp)
		if resp.StatusCode != want {
			t.Fatalf("GET %s: %d, want %d\n%s", path, resp.StatusCode, want, b)
		}
		return b
	}
	page := get("/?days=90&dev=0", 200)
	for _, want := range []string{`aria-label="Which coding tool"`, `data-client="every tool" aria-current="true">every tool</a>`,
		`href="/?days=90&amp;dev=0&amp;client=codex" data-client="Codex">Codex</a>`, "Which coding tool used memory?", `>Claude Code</a></td>`} {
		if !strings.Contains(page, want) {
			t.Errorf("combined overview lacks %q", want)
		}
	}
	page = get("/?days=90&dev=0&client=codex", 200)
	for _, want := range []string{`data-client="Codex" aria-current="true">Codex</a>`, `href="/quality?client=codex"`,
		`<input type="hidden" name="client" value="codex">`, "--client codex"} {
		if !strings.Contains(page, want) {
			t.Errorf("Codex overview lacks %q", want)
		}
	}
	if strings.Contains(page, "Which coding tool used memory?") || strings.Contains(page, "client=codex&amp;client=codex") {
		t.Error("the filtered overview splits by tool, or a link names the tool twice")
	}
	if !strings.Contains(get("/sessions?days=90&client=claude-code", 200), `data-client="Claude Code" aria-current="true"`) {
		t.Error("the sessions page does not say it counts Claude Code")
	}
	get("/quality?client=cursor", 400)
}

// A PC with one tool reads as before: no choice, no split.
func TestOneToolPCHasNoClientChoice(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	b := body(t, h.do("GET", "/?days=90&dev=0", ck...))
	if strings.Contains(b, "Which coding tool") || strings.Contains(b, "data-client") {
		t.Error("a Claude Code only history offers a tool choice or a split")
	}
}

// The System page of Codex's activity names the update skill as a Codex
// session invokes it; every other page names Claude Code's, as before.
func TestSystemUpdateSkillPerClient(t *testing.T) {
	for _, c := range []struct{ client, want, not string }{
		{"", "/backsight:update", "$backsight:update"},
		{"claude-code", "/backsight:update", "$backsight:update"},
		{"codex", "$backsight:update", "/backsight:update"},
	} {
		n := pageNode(t, "system", false, func(d *PageData) {
			richSystem(d.V["system"])
			d.Frame.Client = c.client
		})
		card := cardOf(t, n, "w-this-pc")
		if !hasCode(card, c.want) || hasCode(card, c.not) {
			t.Errorf("client %q: the update command is not %s", c.client, c.want)
		}
	}
}
