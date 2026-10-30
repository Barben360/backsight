package instructions

import "testing"

func TestStatusOnHasNoWords(t *testing.T) {
	if got := StatusOn("billing"); got != "🧠 backsight · 🗂️ billing" {
		t.Errorf("StatusOn = %q", got)
	}
	if got := StatusNotRecorded(""); got != "⏸️ backsight · recall on, this project is not recorded" {
		t.Errorf("prompt-time line = %q", got)
	}
}

func TestColoredPaintsOnlyTheBrandWord(t *testing.T) {
	const c = "\x1b[1;38;2;201;80;122mbacksight\x1b[0m"
	in := "👋 Memory is on: backsight stores this session\n" +
		"🧠 backsight · 🗂️ backsight\n" +
		"⚠️ Claude Code wiring drifted (plugin) · run `backsight client install`\n" +
		UpdateLine("0.1.0", "0.2.0", 0) + "\n" +
		"🧠 backsight\n" +
		"🧠 backsights · 🗂️ x"
	want := "👋 Memory is on: backsight stores this session\n" +
		"🧠 " + c + " · 🗂️ backsight\n" +
		"⚠️ Claude Code wiring drifted (plugin) · run `backsight client install`\n" +
		"⬆️ " + c + " 0.1.0 → 0.2.0 · update here: `/backsight:update` · or in a terminal: `backsight update apply`\n" +
		"🧠 " + c + "\n" +
		"🧠 backsights · 🗂️ x"
	if got := Colored(in); got != want {
		t.Errorf("Colored\n got %q\nwant %q", got, want)
	}
}

// The update line names both versions and both ways to update; the count
// of newer releases shows from 2 on.
func TestUpdateLineNamesTheVersionsAndTheCommands(t *testing.T) {
	cases := []struct {
		newer int
		want  string
	}{
		{0, "⬆️ backsight 0.1.0 → 0.3.0 · update here: `/backsight:update` · or in a terminal: `backsight update apply`"},
		{1, "⬆️ backsight 0.1.0 → 0.3.0 · update here: `/backsight:update` · or in a terminal: `backsight update apply`"},
		{3, "⬆️ backsight 0.1.0 → 0.3.0 (3 newer) · update here: `/backsight:update` · or in a terminal: `backsight update apply`"},
	}
	for _, c := range cases {
		if got := UpdateLine("0.1.0", "0.3.0", c.newer); got != c.want {
			t.Errorf("UpdateLine(newer %d) = %q, want %q", c.newer, got, c.want)
		}
	}
}
