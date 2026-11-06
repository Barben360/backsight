package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
)

func confirmed(evidence string, ids ...string) map[string]any {
	a := map[string]any{"ids": ids, "verdict": "confirmed"}
	if evidence != "" {
		a["evidence"] = evidence
	}
	return a
}

// A confirmed verdict hands back its own line, not the used one, with
// or without evidence, and journals nothing itself (the hook does).
func TestCreditConfirmedReturnsItsOwnLine(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA, creditB)
	before := len(journalEvents(t, home))
	for _, ev := range []string{"", "the user: rtk was removed"} {
		out, isErr := call(t, cs, "memory_credit", confirmed(ev, "aaaaaaaa"))
		if isErr || !strings.HasPrefix(out, "Confirmed.") || !strings.Contains(out, "> 🧠✔ **Confirmed by memory (proj · aaaaaaaa)** — ") ||
			strings.Contains(out, "From memory") || !strings.Contains(out, "final message") {
			t.Errorf("evidence %q: %v\n%s", ev, isErr, out)
		}
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
	withCorrection := confirmed("", "aaaaaaaa")
	withCorrection["correction"] = "Billing runs PostgreSQL 16."
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"unknown id":       {confirmed("", "cccccccc"), "cccccccc: not shown in this session"},
		"secret evidence":  {confirmed("the key is AKIAIOSFODNN7EXAMPLE", "aaaaaaaa"), "secret"},
		"long evidence":    {confirmed(strings.Repeat("é", 301), "aaaaaaaa"), "300"},
		"correction":       {withCorrection, "correction"},
		"no ids":           {map[string]any{"verdict": "confirmed"}, "ids"},
		"eleven ids":       {confirmed("", eleven...), "1 to 10"},
		"one unknown of 2": {confirmed("", "aaaaaaaa", "cccccccc"), "cccccccc: not shown"},
	} {
		out, isErr := call(t, cs, "memory_credit", c.args)
		if !isErr || !strings.Contains(out, c.want) || strings.Contains(out, "Confirmed by memory") {
			t.Errorf("%s: %v %s", name, isErr, out)
		}
	}
	// 300 characters is the limit, not over it
	if out, isErr := call(t, cs, "memory_credit", confirmed(strings.Repeat("é", 300), "aaaaaaaa")); isErr || !strings.HasPrefix(out, "Confirmed.") {
		t.Errorf("300 characters: %v %s", isErr, out)
	}
}

// Ids of two projects: one confirmed line each.
func TestCreditConfirmedNamesEachProject(t *testing.T) {
	cs, home := creditSession(t)
	showItems(t, home, "S", "search", "billing", feedback.Item{ID: creditB, Rank: 1, Project: "infra"}, feedback.Item{ID: creditA, Rank: 2, Project: "billing"})
	out, isErr := call(t, cs, "memory_credit", confirmed("", creditB, "aaaaaaaa"))
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

// creditedInTurn makes turn the current turn of session S and journals what the
// hook would: a credit event of verdict in it, by agent.
func creditedInTurn(t *testing.T, home, turn, agent, verdict string, ids ...string) {
	t.Helper()
	dir := pathsOf(home).FeedbackDir()
	if err := feedback.SetTurn(dir, "S", feedback.Turn{ID: turn, N: 1}); err != nil {
		t.Fatal(err)
	}
	if verdict == "" {
		return
	}
	j := feedback.NewJournal(dir, feedback.Common{Session: "S", Project: "proj"})
	if err := j.Append(feedback.Event{Kind: "credit", Turn: turn, Agent: agent, IDs: ids, Verdict: verdict}); err != nil {
		t.Fatal(err)
	}
}

// none contradicts a credit of the same turn: refused. A new turn, a
// subagent's credit or an earlier none leaves it accepted.
func TestCreditNoneAfterACreditIsRefused(t *testing.T) {
	for _, v := range []string{"used", "confirmed", "wrong", "outdated"} {
		cs, home := creditSession(t)
		showIn(t, home, "S", creditA)
		creditedInTurn(t, home, "p-2", "", v, creditA)
		out, isErr := call(t, cs, "memory_credit", map[string]any{"verdict": "none"})
		if !isErr || !strings.Contains(out, "verdict none after a credit in this turn") {
			t.Errorf("none after %s: %v %q", v, isErr, out)
		}
	}
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	creditedInTurn(t, home, "p-2", "", "used", creditA)
	creditedInTurn(t, home, "p-3", "", "") // a fresh turn
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"verdict": "none"}); isErr || out != "Noted: no memory used." {
		t.Errorf("none in a fresh turn: %v %q", isErr, out)
	}
	creditedInTurn(t, home, "p-4", "", "none") // none first, a credit after
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}}); isErr || !strings.HasPrefix(out, "Credited.") {
		t.Errorf("credit after none: %v %q", isErr, out)
	}
	creditedInTurn(t, home, "p-5", "a7", "used", creditA) // another agent's credit
	if out, isErr := call(t, cs, "memory_credit", map[string]any{"verdict": "none"}); isErr {
		t.Errorf("none after a subagent's credit: %q", out)
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
