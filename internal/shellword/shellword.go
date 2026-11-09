// Package shellword writes a value as one word of a command a person or an
// agent copies and runs: a project name, a memory id, a path. Values come
// from folder names and servers, so a word must never let the shell run
// part of it.
package shellword

import (
	"runtime"
	"strings"
)

// Word is s as one word of the shell a command shown on this PC is pasted
// in: PowerShell on Windows (PowerShell), POSIX sh elsewhere (POSIX).
//
// The two differ only on words holding a quote, a comma, a backslash or a
// tilde, or starting with @. A
// PowerShell word stays safe in sh (a doubled quote only splits the word in
// two quoted halves), while a POSIX one is not safe in PowerShell (it
// takes the backslash before a quote literally, so the string ends there),
// hence the choice by system.
func Word(s string) string {
	if runtime.GOOS == "windows" {
		return PowerShell(s)
	}
	return POSIX(s)
}

// POSIX is s as one sh word: bare when it holds only characters that mean
// nothing to a shell, else in single quotes (a quote inside written as
// quote, backslash, quote, quote).
func POSIX(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:=@%+,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PowerShell is s as one PowerShell word when it is not one plain word
// there: a Windows path is one (C:\Users\me\x), a leading @ (splatting) or
// ~ is not, nor is a comma (an array), --% (stop parsing) or anything else.
func PowerShell(s string) string {
	if s != "" && s != "--%" && s[0] != '@' && s[0] != '~' &&
		strings.Trim(s, `abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_=./:\+%@~`) == "" {
		return s
	}
	return PowerShellQuote(s)
}

// PowerShellQuote always quotes s for PowerShell: a single-quoted string is
// literal, and a quote inside is doubled. PowerShell takes the typographic
// single quotes for quotes too.
func PowerShellQuote(s string) string {
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
