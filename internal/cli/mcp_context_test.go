package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/instructions"
)

// staticContext is what every session re-reads on each API call (decision
// 0083 C7): the SessionStart guide, the MCP server's instructions, and
// each tool's description and parameter descriptions, as served.
func staticContext(t *testing.T, feedback bool) string {
	t.Helper()
	extra := ""
	if !feedback {
		extra = "\n[feedback]\nenabled = false\n"
	}
	cs := mcpSessionWith(t, extra)
	parts := []string{instructions.For(feedback).Guide, cs.InitializeResult().Instructions}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		parts = append(parts, tl.Description)
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for _, p := range schema.Properties {
			parts = append(parts, p.Description)
		}
	}
	return strings.Join(parts, "\n")
}

// Every rule the static context teaches, by a phrase of its own: each must
// be there exactly once (none lost, none repeated across the guide, the
// MCP instructions and the tool texts).
var contextRules = []string{
	// when and how to search
	searchTrigger, "BEFORE asking the user a factual question or a preference", "recommending an architecture, tool or convention", "starting a task or topic",
	"writing a test or changing something shared", "the user refers to the past", "kip it for trivial edits", "a fact already found this session",
	"1-4 short keyword queries", "one per angle", "topic + intent word", "never a kind word alone", "a copy of the user's sentence", "date or version",
	"topic + pitfall / correction", "topic + decision", "topic + convention / correction", "exact names as listed at session start", "name it in `projects`", "Hits show their kinds",
	"verify them against the code", "read it first, then search its content", "User-wide rules override conflicting project facts",
	"Tags name each memory's source", "a note or an import is a claim to verify",
	"except a project's own standing convention, decision or correction, which wins there; a past event never does",
	"auto (current project", "default 20", "relevance floor",
	// what to store, and how
	"Store decisions, corrections, conventions, pitfalls and lessons", "decision, correction, convention, pitfall, state or lesson",
	"one durable fact per call", "split a fact that covers two topics", "1-3 sentences: the fact, why, absolute dates", `never "today"`, "exact identifiers",
	"no code dumps, no secrets, no plans or hypotheses", "no merges, releases or CI status (the conversation has them)",
	"a delivered state only with a reason or location the conversation lacks", `"transverse" only for a user-wide rule`, "default: the current project", "it reaches every project",
	"Example: \"Decision", "state the fact itself", "Close the loop", "a recalled open item is settled (shipped, abandoned, root cause found)",
	"contradicted by the code or the user", "names the old claim in its own words", "kind correction for a contradiction", "parallel fact",
	"Updated if the fact updates the memory it shows",
	// read
	"source facts",
	// the session
	"Answer in the user's language", "Never ask the user for a key, token or password", "`backsight secret set`",
	"An empty search: one line «" + instructions.NothingLine + "»",
}

// searchTrigger is the one search trigger the MCP instructions keep: a
// client that defers the tools' descriptions (or a compacted context)
// still has it.
const searchTrigger = "Search before a factual question, a recommendation or a new task."

// The MCP instructions themselves, not only the static context, carry the
// search trigger, with feedback on and off.
func TestMCPInstructionsKeepTheSearchTrigger(t *testing.T) {
	for _, extra := range []string{"", "\n[feedback]\nenabled = false\n"} {
		if got := mcpSessionWith(t, extra).InitializeResult().Instructions; !strings.Contains(got, searchTrigger) {
			t.Errorf("MCP instructions (config %q) lack the search trigger: %s", extra, got)
		}
	}
}

// The credit rules, only while memory_credit is offered.
var creditRules = []string{
	instructions.CreditAsk, "a search sent in the same message must return its result first", "returns the line to print in your final message",
	"none with verdict none", "1-10 memory ids", "established independently of it", "Never as a default, never for a memory you did not check",
	"it takes no evidence", "one call per verdict", "Use verdict wrong when this session shows the memory is false, outdated when it was true but no longer is",
	"give the right fact as correction", "used (default), confirmed, none, wrong or outdated", "1-3 sentences, as for memory_store",
	"at most 300 characters", `"verdict": "wrong"`,
}

func TestStaticContextTeachesEachRuleOnce(t *testing.T) {
	for _, feedback := range []bool{true, false} {
		text := staticContext(t, feedback)
		rules := append([]string{}, contextRules...)
		if feedback {
			rules = append(rules, creditRules...)
		} else {
			rules = append(rules, "cite it exactly: «"+instructions.Credit+"»")
			if strings.Contains(text, "memory_credit") {
				t.Errorf("feedback off: the static context names memory_credit:\n%s", text)
			}
		}
		for _, r := range rules {
			if n := strings.Count(text, r); n != 1 {
				t.Errorf("feedback %v: %q is there %d times, want once", feedback, r, n)
			}
		}
	}
}

// Decision 0083 C7: the static context, re-read on every API call, was
// 7 231 characters in v14 with the parameters; 6 500 was asked, v15 holds
// it under 5 000.
func TestStaticContextBudget(t *testing.T) {
	for _, feedback := range []bool{true, false} {
		if n := utf8.RuneCountInString(staticContext(t, feedback)); n > 5100 { // 0089 P2: + the source words
			t.Errorf("feedback %v: static context %d chars (max 5 100)", feedback, n)
		}
	}
}
