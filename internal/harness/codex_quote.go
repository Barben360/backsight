package harness

import (
	"errors"
	"strings"
	"unicode"
)

// The quoting of the shim in a Codex hook handler's command (codex.go's
// Quoting contract). Block 3's HookCommand calls these; it does not quote
// on its own.

// QuoteSh is s as one word for sh, bash, zsh and fish: in single quotes,
// with each single quote and each backslash written outside them
// (\' and \\), where every one of those shells reads them back as is.
func QuoteSh(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '\\':
			b.WriteString(`'\`)
			b.WriteRune(r)
			b.WriteByte('\'')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// ErrUnquotable: a path cmd cannot be made to read back reliably.
var ErrUnquotable = errors.New("cannot be quoted for cmd: it holds a double quote, a % or a control character")

// QuoteCmd is s in double quotes, for cmd /C "<command>": inside them,
// spaces, &, |, ^, < and > are plain characters. A double quote cannot be
// escaped inside cmd's quotes, and % expands a variable even there: such
// a path (and one with a control character) is refused.
func QuoteCmd(s string) (string, error) {
	if strings.ContainsAny(s, `"%`) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", ErrUnquotable
	}
	return `"` + s + `"`, nil
}
