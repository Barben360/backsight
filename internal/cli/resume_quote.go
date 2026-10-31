package cli

import (
	"runtime"
	"strings"
)

// resumeWord quotes s for the shell a resume command is pasted in, when s
// is not one plain word there: POSIX sh on Linux and macOS (shellWord),
// PowerShell on Windows (psWord). Commands run on another host over ssh
// stay POSIX (shellWord).
func resumeWord(s string) string {
	if runtime.GOOS == "windows" {
		return psWord(s)
	}
	return shellWord(s)
}

// resumeQuote always quotes s, for the same shell as resumeWord.
func resumeQuote(s string) string {
	if runtime.GOOS == "windows" {
		return psQuote(s)
	}
	return shellQuote(s)
}

// psQuote quotes s for PowerShell: a single-quoted string is literal, and a
// quote inside is doubled. PowerShell takes the typographic single quotes
// for quotes too.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		b.WriteRune(r)
		switch r {
		case '\'', '\u2018', '\u2019', '\u201a', '\u201b':
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// psWord quotes s for PowerShell when it is not one plain word there: a
// Windows path is one (C:\Users\me\x), a leading @ (splatting) or ~ is
// not, nor is a comma (an array), --% (stop parsing) or anything else.
func psWord(s string) string {
	if s != "" && s != "--%" && s[0] != '@' && s[0] != '~' &&
		strings.Trim(s, `abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=./:\+%@~`) == "" {
		return s
	}
	return psQuote(s)
}
