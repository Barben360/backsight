package instructions

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const fullID = "7e1f9d03-0000-4000-8000-000000000001"

// v9: every line a tool or a skill asks the agent to print has one shape,
// "> 🧠<emoji> **<label> (<project> · <what>)** — <text>": 🧠 alone for a
// memory read, 🧠 and one emoji for a write or a problem.
func TestFeedbackLines(t *testing.T) {
	for name, c := range map[string]struct{ got, want string }{
		"credit": {CreditLine("billing", []string{"0b4c2a52", "7d1e93a0"}),
			"> 🧠 **From memory (billing · 0b4c2a52, 7d1e93a0)** — <the facts you used, in your words>"},
		"saved": {SavedLine("billing", "decision"),
			"> 🧠💾 **Saved (billing · decision)** — <the fact, in a few words>"},
		"updated": {UpdatedLine("billing", "correction"),
			"> 🧠🔄 **Updated (billing · correction · replaces <id8 of the memory it updates>)** — <the fact, in a few words>"},
		"wrong": {RetiredLine("wrong", "billing", fullID),
			"> 🧠❌ **Wrong, retired (billing · 7e1f9d03)** — <what was wrong, and what shows it> · undo: `backsight forget --restore " + fullID + "`"},
		"outdated": {RetiredLine("outdated", "billing", fullID),
			"> 🧠⌛ **Outdated, retired (billing · 7e1f9d03)** — <what changed, and what shows it> · undo: `backsight forget --restore " + fullID + "`"},
		"hidden": {HiddenLine("outdated", "billing", fullID, false),
			"> 🧠🙈 **Hidden on this PC (billing · 7e1f9d03)** — <what changed, and what shows it> · undo: `backsight forget --restore " + fullID + "`"},
		"hidden, server unread": {HiddenLine("wrong", "billing", fullID, true),
			"> 🧠🙈 **Hidden on this PC (billing · 7e1f9d03)** — <what was wrong, and what shows it> · retire it on the server: `backsight forget " + fullID + "` · undo: `backsight forget --restore " + fullID + "`"},
		"not retired": {NotRetiredLine("billing", fullID, "the server refused it", true),
			"> 🧠⚠️ **Not retired (billing · 7e1f9d03)** — the server refused it · retire it: `backsight forget " + fullID + "`"},
		"not retired, no command": {NotRetiredLine("billing", fullID, "this PC joined another server", false),
			"> 🧠⚠️ **Not retired (billing · 7e1f9d03)** — this PC joined another server"},
		"nothing": {NothingLine, "> 🧠🔍 **Nothing in memory** — <topic>"},
		"retro":   {RetroLine, "> 🧠📚 **Retro (<project>)** — <n> saved, <n> corrected, <n> to promote"},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, c.got, c.want)
		}
		// at most two emoji per line, the status line's rule (issue 7)
		if head, _, _ := strings.Cut(c.got, " **"); !strings.HasPrefix(head, "> 🧠") || utf8.RuneCountInString(strings.TrimPrefix(head, "> ")) > 3 {
			t.Errorf("%s: head %q", name, head)
		}
	}
}

// The lines the agent writes without a tool's answer, where it is told.
func TestNothingAndRetroLinesAreAskedFor(t *testing.T) {
	for name, s := range map[string]string{"guide": Guide, "guide, feedback off": GuideCite, "gate reason": GateReason} {
		if !strings.Contains(s, NothingLine) {
			t.Errorf("%s lacks the nothing line: %s", name, s)
		}
	}
	if !strings.Contains(skillBody("memory-retro"), RetroLine) {
		t.Errorf("retro skill lacks the retro line:\n%s", skillBody("memory-retro"))
	}
	for _, s := range []string{StoreDescription, skillBody("remember")} {
		if !strings.Contains(s, "line") || strings.Contains(s, "say in one line") || strings.Contains(s, "say it updates it") {
			t.Errorf("memory_store's line is not the one asked for: %s", s)
		}
	}
}
