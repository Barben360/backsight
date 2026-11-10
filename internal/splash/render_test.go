package splash

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/termstyle"
)

var testPanel = []Line{
	{{Role: termstyle.Brand, Text: "backsight"}, {Plain: true, Text: " 1.0.0 · Hindsight 1.2.0"}},
	{{Role: termstyle.Secondary, Text: "────"}},
	{{Role: termstyle.Label, Text: "Setup   "}, {Plain: true, Text: "host · docker"}},
}

func TestRenderPlain(t *testing.T) {
	var b bytes.Buffer
	Render(&b, termstyle.None, 80, false, blocks[:4], testPanel)
	want := "     ▄▄▀▀▀          backsight 1.0.0 · Hindsight 1.2.0\n" +
		"   ▄▀   ▄▄▀▀        ────\n" +
		"  █   ▄▀   ▄▀▀      Setup   host · docker\n" +
		"  █   █   █    ●\n" +
		"\n"
	if b.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", b.String(), want)
	}
}

func TestRenderColorStripsToPlain(t *testing.T) {
	for _, lvl := range []termstyle.Level{termstyle.Basic, termstyle.Ansi256, termstyle.TrueColor} {
		var plain, color bytes.Buffer
		Render(&plain, termstyle.None, 80, false, blocks, testPanel)
		Render(&color, lvl, 80, false, blocks, testPanel)
		if !strings.Contains(color.String(), "\x1b[") {
			t.Fatalf("level %v: no color", lvl)
		}
		if termstyle.Strip(color.String()) != plain.String() {
			t.Fatalf("level %v: stripped color differs from plain", lvl)
		}
	}
}

func TestRenderTruncatesToWidth(t *testing.T) {
	long := []Line{{{Plain: true, Text: strings.Repeat("x", 200)}}}
	for _, ascii := range []bool{false, true} {
		var b bytes.Buffer
		Render(&b, termstyle.TrueColor, 72, ascii, blocks, long)
		row := strings.SplitN(termstyle.Strip(b.String()), "\n", 2)[0]
		if n := utf8.RuneCountInString(row); n > 71 {
			t.Fatalf("ascii=%v: row of %d runes at width 72", ascii, n)
		}
		mark := "…"
		if ascii {
			mark = "..."
		}
		if !strings.HasSuffix(row, mark) {
			t.Fatalf("ascii=%v: %q does not end with %q", ascii, row, mark)
		}
	}
}

func TestRenderASCIIHasNoHighByte(t *testing.T) {
	var b bytes.Buffer
	Render(&b, termstyle.TrueColor, 80, true, ascii, []Line{{{Plain: true, Text: strings.Repeat("y", 100)}}})
	for i := 0; i < b.Len(); i++ {
		if b.Bytes()[i] >= 0x80 {
			t.Fatalf("byte %d is %#x", i, b.Bytes()[i])
		}
	}
}
