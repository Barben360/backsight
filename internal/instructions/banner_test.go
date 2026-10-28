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
		"⬆️ backsight update 0.2.0 available · see `backsight changelog`\n" +
		"🧠 backsight\n" +
		"🧠 backsights · 🗂️ x"
	want := "👋 Memory is on: backsight stores this session\n" +
		"🧠 " + c + " · 🗂️ backsight\n" +
		"⚠️ Claude Code wiring drifted (plugin) · run `backsight client install`\n" +
		"⬆️ " + c + " update 0.2.0 available · see `backsight changelog`\n" +
		"🧠 " + c + "\n" +
		"🧠 backsights · 🗂️ x"
	if got := Colored(in); got != want {
		t.Errorf("Colored\n got %q\nwant %q", got, want)
	}
}
