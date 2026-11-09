package termstyle

import (
	"strconv"
	"strings"
)

// Role is what a piece of text is; the palette picks its color.
type Role int

const (
	Brand     Role = iota // the word backsight
	Accent                // the logo's orange dot, an update
	OK                    // a passing status
	Warn                  // a warning status
	Fail                  // a failure or an error
	Label                 // a line's label column
	Secondary             // ids, paths, times
	Command               // a command to run, a URL
)

// shade is one role's look at each level. rgb and idx are unset (rgb nil)
// for roles that are only bold or dim.
type shade struct {
	bold, dim bool
	rgb       []int // truecolor
	idx       int   // 256-color index
	basic     int   // 16-color SGR code
}

// palette: approved by the author on 2026-11-09 after renders on dark and
// light themes (https://github.com/Barben360/backsight/issues/258). Text
// shades keep at least 3.5:1 contrast on white and on a dark background;
// the logo's dot keeps the logo orange #E8913A (phase 2).
var palette = map[Role]shade{
	Brand:     {bold: true, rgb: []int{201, 80, 122}, idx: 168, basic: 35},
	Accent:    {rgb: []int{204, 108, 20}, idx: 166, basic: 33},
	OK:        {rgb: []int{56, 150, 60}, idx: 28, basic: 32},
	Warn:      {rgb: []int{181, 120, 0}, idx: 136, basic: 33},
	Fail:      {rgb: []int{220, 70, 60}, idx: 167, basic: 31},
	Label:     {bold: true},
	Secondary: {dim: true},
	Command:   {rgb: []int{30, 140, 175}, idx: 31, basic: 36},
}

// sgr is the escape that starts role r at lvl ("" when off).
func sgr(r Role, lvl Level) string {
	s, ok := palette[r]
	if !ok || lvl == None {
		return ""
	}
	var params []string
	if s.bold {
		params = append(params, "1")
	}
	if s.dim {
		params = append(params, "2")
	}
	if s.rgb != nil {
		switch lvl {
		case TrueColor:
			params = append(params, "38;2;"+strconv.Itoa(s.rgb[0])+";"+strconv.Itoa(s.rgb[1])+";"+strconv.Itoa(s.rgb[2]))
		case Ansi256:
			params = append(params, "38;5;"+strconv.Itoa(s.idx))
		default:
			params = append(params, strconv.Itoa(s.basic))
		}
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}
