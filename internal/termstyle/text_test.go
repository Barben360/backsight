package termstyle

import (
	"strings"
	"testing"
)

func TestStylePaintOffIsIdentity(t *testing.T) {
	var s Style
	if s.On() {
		t.Fatal("the zero Style is on")
	}
	if got := s.Paint(Fail, "boom"); got != "boom" {
		t.Errorf("off Paint = %q", got)
	}
	if got := s.Cell(OK, "ok", 6); got != "ok    " {
		t.Errorf("off Cell = %q", got)
	}
	if got := s.Labels("Server       x\n"); got != "Server       x\n" {
		t.Errorf("off Labels = %q", got)
	}
}

func TestStyleOfReadsTheWritersLevel(t *testing.T) {
	w := NewWriter(&strings.Builder{}, func() Level { return Basic })
	if got := StyleOf(w).Paint(Fail, "x"); got != "\x1b[31mx\x1b[0m" {
		t.Errorf("StyleOf(basic).Paint = %q", got)
	}
	if StyleOf(&strings.Builder{}).On() {
		t.Error("a plain writer gave a colored Style")
	}
}

func TestCellPadsOutsideEscapes(t *testing.T) {
	s := StyleAt(Basic)
	got := s.Cell(OK, "ok", 6)
	if got != "\x1b[32mok\x1b[0m    " {
		t.Errorf("Cell = %q, want the padding after the reset", got)
	}
	if got := s.Cell(OK, "toolong", 3); Strip(got) != "toolong" {
		t.Errorf("a cell longer than its width = %q", got)
	}
	if got := s.Cell(OK, "é", 3); Strip(got) != "é  " {
		t.Errorf("width counts runes: %q", Strip(got))
	}
}

func TestLabelsBoldsTheLabelColumn(t *testing.T) {
	s := StyleAt(Basic)
	in := "Server       answering\nHook failures none\n  indented   value\nBy language  French\n\nno  label\n"
	got := s.Labels(in)
	want := "\x1b[1mServer\x1b[0m       answering\nHook failures none\n  indented   value\n\x1b[1mBy language\x1b[0m  French\n\n\x1b[1mno\x1b[0m  label\n"
	if got != want {
		t.Errorf("Labels =\n%q\nwant\n%q", got, want)
	}
}

func FuzzLabels(f *testing.F) {
	for _, s := range []string{"Server       x\n", "a  b\nc d\n", "\n\n", "Label  ", "x\x1b[0m  y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.Contains(s, "\x1b") {
			return
		}
		if got := StyleAt(TrueColor).Labels(s); Strip(got) != s {
			t.Errorf("Strip(Labels(%q)) = %q", s, Strip(got))
		}
	})
}

func FuzzCell(f *testing.F) {
	f.Add("ok", 6)
	f.Add("", 3)
	f.Fuzz(func(t *testing.T, s string, w int) {
		if strings.Contains(s, "\x1b") || w > 200 {
			return
		}
		if got, want := Strip(StyleAt(Ansi256).Cell(Warn, s, w)), (Style{}).Cell(Warn, s, w); got != want {
			t.Errorf("Strip(Cell(%q, %d)) = %q, want %q", s, w, got, want)
		}
	})
}
