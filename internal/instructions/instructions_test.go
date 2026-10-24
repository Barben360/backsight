package instructions

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Size limits from SPEC §6.2 and §8: hook outputs must stay far below
// Claude Code's 10 000-char cap and inside the session budget.
func TestSizes(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		max  int
	}{
		{"guide (E6: near 700)", Guide, 900},
		{"short guide", ShortGuide, 1000},
		{"reminder", Reminder, 300},
		{"nudge with terms", Nudge([]string{"PostgreSQL", "Grafana", "backfill", "observation scopes"}), 300},
		{"gate reason", GateReason, 400},
		{"subagent guide", SubagentGuide, 200},
		{"search description", SearchDescription, 1000},
		{"store description", StoreDescription, 1200},
		{"read description", ReadDescription, 300},
	} {
		if n := utf8.RuneCountInString(c.text); n > c.max || n == 0 {
			t.Errorf("%s: %d chars (max %d)", c.name, n, c.max)
		}
	}
}

func TestCreditIsQuotedVerbatimWhereClaudeIsAskedToCite(t *testing.T) {
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide, "reminder": Reminder} {
		if !strings.Contains(s, Credit) {
			t.Errorf("%s lacks the exact credit format", name)
		}
	}
	if Credit != "> 🧠 **From Hindsight memory (<project>)** — <facts>" {
		t.Errorf("credit changed: %q", Credit)
	}
}

func TestGuideCoversTheSpecPoints(t *testing.T) {
	for _, want := range []string{"memory_search", "1-4 short keyword queries", "verify", "nothing in memory", "memory_store",
		"transverse", "override", "user's language", "test"} {
		if !strings.Contains(Guide, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}

func TestStoreDescriptionCarriesTheRubric(t *testing.T) {
	for _, want := range []string{"decision, correction, convention, pitfall, state, lesson", "one fact per call", "why", "absolute dates",
		"no secrets", "transverse", "Examples:", "Non-examples:", "3 most similar"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
}

func TestNudgeNamesTermsOnlyWhenKnown(t *testing.T) {
	if strings.Contains(Nudge(nil), "knows about") || !strings.Contains(Nudge([]string{"Grafana"}), "knows about: Grafana") {
		t.Errorf("%q / %q", Nudge(nil), Nudge([]string{"Grafana"}))
	}
}
