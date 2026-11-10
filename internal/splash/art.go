// Package splash is the neofetch-style splash a full backsight command
// prints first at a terminal: the logo beside a panel about backsight
// itself, never the project (decision 0084 D3, D4).
package splash

import "strings"

// ArtLine is one row of a logo. Mask holds, rune for rune, which part
// each character draws: i, m, o the inner, middle and outer arc, d the
// dot; a space or a missing rune is not painted.
type ArtLine struct{ Text, Mask string }

// The art approved in https://github.com/Barben360/backsight/issues/258:
// braille (B, the default since v0.1.36-m7), blocks (A, `[terminal] logo
// = "blocks"`), ascii (D, for a console that is not UTF-8).
var (
	blocks = []ArtLine{
		{"     ▄▄▀▀▀", "     ooooo"},
		{"   ▄▀   ▄▄▀▀", "   oo   mmmm"},
		{"  █   ▄▀   ▄▀▀", "  o   mm   iii"},
		{"  █   █   █    ●", "  o   m   i    d"},
		{"  █   ▀▄   ▀▄▄", "  o   mm   iii"},
		{"   ▀▄   ▀▀▄▄", "   oo   mmmm"},
		{"     ▀▀▄▄▄", "     ooooo"},
	}
	braille = []ArtLine{
		{"     ⢀⣴⠟", "     ooo"},
		{"    ⢠⡿⠁⢀⣾⠃", "    ooommm"},
		{"    ⣾⠁⢀⡿⠁⢀⡶⠂", "    oommmiii"},
		{"    ⣿ ⢸⡇ ⢸⡇  ●", "    o mm ii  d"},
		{"    ⣿⡀⠈⣷⡀⠈⠷⠄", "    oommmiii"},
		{"    ⠘⣷⡀⠈⢷⡄", "    ooommm"},
		{"     ⠈⠻⣦", "     ooo"},
	}
	ascii = []ArtLine{
		{"     .--'", "     oooo"},
		{"   .'  .-'", "   oo  mmm"},
		{"  /  .'  .'", "  o  mm  ii"},
		{" |  |   |   o", " o  m   i   d"},
		{"  \\  `.  `.", "  o  mm  ii"},
		{"   `.  `-.", "   oo  mmm"},
		{"     `--.", "     oooo"},
	}
)

// Logo is the art for the configured kind ("blocks", else braille), or
// the ASCII one when the console is not UTF-8.
func Logo(kind string, utf8 bool) []ArtLine {
	switch {
	case !utf8:
		return ascii
	case kind == "blocks":
		return blocks
	}
	return braille
}

// localeUTF8: the locale that decides the character set (the first set
// of LC_ALL, LC_CTYPE, LANG) is a UTF-8 one. None set is the C locale.
func localeUTF8(getenv func(string) string) bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(getenv(k)); v != "" {
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}
