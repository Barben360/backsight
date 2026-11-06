package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
)

func confirmed(ids ...string) map[string]any {
	return map[string]any{"ids": ids, "verdict": "confirmed"}
}

// A confirmed verdict hands back its own line, not the used one, and
// journals nothing itself (the hook does).
func TestCreditConfirmedReturnsItsOwnLine(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	before := len(journalEvents(t, home))
	out, isErr := call(t, cs, "memory_credit", confirmed("aaaaaaaa"))
	if isErr || !strings.HasPrefix(out, "Confirmed.") || !strings.Contains(out, "> 🧠✔ **Confirmed by memory (proj · aaaaaaaa)** — ") ||
		strings.Contains(out, "From memory") || !strings.Contains(out, "final message") {
		t.Errorf("%v\n%s", isErr, out)
	}
	if n := len(journalEvents(t, home)); n != before {
		t.Errorf("the server journaled %d events: only the hook does", n-before)
	}
	if !creditEvent(t, home, `"verdict":"confirmed"`, `"ids_n":1`, `"ok":true`) || creditEvent(t, home, `"verdict":"used"`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, home))
	}
}

func TestCreditConfirmedRefusals(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = creditA
	}
	withCorrection := confirmed("aaaaaaaa")
	withCorrection["correction"] = "Billing runs PostgreSQL 16."
	withEvidence := confirmed("aaaaaaaa")
	withEvidence["evidence"] = "the user: rtk was removed"
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"unknown id":       {confirmed("cccccccc"), "cccccccc: not shown in this session"},
		"evidence":         {withEvidence, "evidence: only with wrong or outdated"},
		"correction":       {withCorrection, "correction"},
		"no ids":           {map[string]any{"verdict": "confirmed"}, "ids"},
		"eleven ids":       {confirmed(eleven...), "1 to 10"},
		"one unknown of 2": {confirmed("aaaaaaaa", "cccccccc"), "cccccccc: not shown"},
	} {
		out, isErr := call(t, cs, "memory_credit", c.args)
		if !isErr || !strings.Contains(out, c.want) || strings.Contains(out, "Confirmed by memory") {
			t.Errorf("%s: %v %s", name, isErr, out)
		}
	}
}

// Ids of two projects: one confirmed line each.
func TestCreditConfirmedNamesEachProject(t *testing.T) {
	cs, home := creditSession(t)
	showItems(t, home, "S", "search", "billing", feedback.Item{ID: creditB, Rank: 1, Project: "infra"}, feedback.Item{ID: creditA, Rank: 2, Project: "billing"})
	out, isErr := call(t, cs, "memory_credit", confirmed(creditB, "aaaaaaaa"))
	want := "Confirmed. Print these lines in your final message:\n" +
		"> 🧠✔ **Confirmed by memory (infra · bbbbbbbb)** — <what this session confirmed, in your words>\n" +
		"> 🧠✔ **Confirmed by memory (billing · aaaaaaaa)** — <what this session confirmed, in your words>"
	if isErr || out != want {
		t.Errorf("%v\n%s", isErr, out)
	}
}

// An unknown verdict is still refused and still telemetered as invalid.
func TestCreditUnknownVerdictStaysInvalid(t *testing.T) {
	cs, home := creditSession(t)
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"verdict": "maybe"}); !isErr || !strings.Contains(out, `"confirmed"`) {
		t.Errorf("%v %s", isErr, out)
	}
	if !creditEvent(t, home, `"verdict":"invalid"`) {
		t.Errorf("telemetry:\n%s", telemetryText(t, home))
	}
}

// The PostToolUse hook journals a confirmed answer under its own verdict.
func TestPostToolUseJournalsAConfirmedCredit(t *testing.T) {
	home := hookHome(t)
	full := "0b4c2a52-0000-4000-8000-000000000001"
	showItems(t, home, "s1", "search", "billing", feedback.Item{ID: full, Rank: 1, Project: "billing"})
	fire(t, "post-tool-use", map[string]any{"session_id": "s1", "prompt_id": "p-2", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
		"tool_input":    map[string]any{"ids": []string{"0b4c2a52"}, "verdict": "confirmed"},
		"tool_response": []map[string]string{{"type": "text", "text": "Confirmed. Print this line in your final message:\n> 🧠✔ **Confirmed by memory (billing · 0b4c2a52)** — x"}}})
	credits := eventsOf(journalEvents(t, home), "credit")
	if len(credits) != 1 || credits[0].Verdict != "confirmed" || strings.Join(credits[0].IDs, ",") != full {
		t.Errorf("credits %+v", credits)
	}
}
