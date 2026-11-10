package splash

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/termstyle"
)

// logoColumn is the width the logo takes, its gap included.
const logoColumn = 20

// Seg is a run of panel text in one role; Plain text is not painted.
type Seg struct {
	Role  termstyle.Role
	Plain bool
	Text  string
}

// Line is one panel line.
type Line []Seg

var maskRole = map[rune]termstyle.Role{'i': termstyle.Brand, 'm': termstyle.ArcMiddle, 'o': termstyle.ArcOuter, 'd': termstyle.Dot}

// Render writes the logo beside the panel at lvl, each row at most
// width-1 columns (a panel line too long ends with an ellipsis), then
// one empty line. Without color it is the same text, unpainted.
func Render(w io.Writer, lvl termstyle.Level, width int, asciiOnly bool, logo []ArtLine, panel []Line) {
	pen := termstyle.NewWriter(io.Discard, func() termstyle.Level { return lvl })
	var b strings.Builder
	for i := range max(len(logo), len(panel)) {
		var art ArtLine
		if i < len(logo) {
			art = logo[i]
		}
		b.WriteString(paintArt(pen, art))
		if i < len(panel) {
			b.WriteString(strings.Repeat(" ", max(1, logoColumn-utf8.RuneCountInString(art.Text))))
			b.WriteString(paintLine(pen, fit(panel[i], width-1-logoColumn, asciiOnly)))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	_, _ = io.WriteString(w, b.String())
}

// paintArt paints each run of one mask letter in its arc's shade.
func paintArt(pen io.Writer, l ArtLine) string {
	text, mask := []rune(l.Text), []rune(l.Mask)
	var b strings.Builder
	for i := 0; i < len(text); {
		m := ' '
		if i < len(mask) && text[i] != ' ' {
			m = mask[i]
		}
		j := i + 1
		for j < len(text) {
			n := ' '
			if j < len(mask) && text[j] != ' ' {
				n = mask[j]
			}
			if n != m {
				break
			}
			j++
		}
		if r, ok := maskRole[m]; ok {
			b.WriteString(termstyle.Paint(pen, r, string(text[i:j])))
		} else {
			b.WriteString(string(text[i:j]))
		}
		i = j
	}
	return b.String()
}

func paintLine(pen io.Writer, l Line) string {
	var b strings.Builder
	for _, s := range l {
		if s.Plain {
			b.WriteString(s.Text)
		} else {
			b.WriteString(termstyle.Paint(pen, s.Role, s.Text))
		}
	}
	return b.String()
}

// fit cuts l to at most n runes, ending it with an ellipsis when cut.
func fit(l Line, n int, asciiOnly bool) Line {
	total := 0
	for _, s := range l {
		total += utf8.RuneCountInString(s.Text)
	}
	if total <= n {
		return l
	}
	mark := "…"
	if asciiOnly {
		mark = "..."
	}
	room := max(0, n-utf8.RuneCountInString(mark))
	var out Line
	for _, s := range l {
		r := []rune(s.Text)
		if len(r) >= room {
			s.Text = string(r[:room]) + mark
			return append(out, s)
		}
		room -= len(r)
		out = append(out, s)
	}
	return out
}
