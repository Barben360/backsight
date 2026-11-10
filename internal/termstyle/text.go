package termstyle

import (
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Style paints text for one stream's color level. The zero Style paints
// nothing: every method then returns the plain text.
type Style struct{ level Level }

// StyleOf is the Style of w's stream (off for any writer that is not a
// *Writer, or whose color is off).
func StyleOf(w io.Writer) Style { return Style{LevelOf(w)} }

// StyleAt is the Style of level l.
func StyleAt(l Level) Style { return Style{l} }

// On says whether this Style adds color.
func (s Style) On() bool { return s.level != None }

// Paint wraps text in role r's color, or returns it unchanged when the
// Style is off or text is empty. Strip(s.Paint(r, t)) == t.
func (s Style) Paint(r Role, text string) string {
	if text == "" {
		return text
	}
	start := sgr(r, s.level)
	if start == "" {
		return text
	}
	return start + text + reset
}

// Cell is text painted in role r, then padded with spaces to width runes:
// the padding stays outside the escape, so the column lines up as the
// plain text's does (fmt's %-*s would count the escape bytes).
func (s Style) Cell(r Role, text string, width int) string {
	pad := width - utf8.RuneCountInString(text)
	if pad < 0 {
		pad = 0
	}
	return s.Paint(r, text) + strings.Repeat(" ", pad)
}

// labelColumn is a label at a line's start: words separated by single
// spaces, then two spaces or more.
var labelColumn = regexp.MustCompile(`(?m)^([A-Za-z][^ \n]*(?: [^ \n]+)*)( {2,})`)

// Labels paints the label column of each line that has one (decision
// 0084 D5's opt-in rule): only a command whose lines are "Label   value"
// rows calls it, never a pass over every output.
func (s Style) Labels(text string) string {
	if !s.On() {
		return text
	}
	return labelColumn.ReplaceAllStringFunc(text, func(m string) string {
		sub := labelColumn.FindStringSubmatch(m)
		return s.Paint(Label, sub[1]) + sub[2]
	})
}
