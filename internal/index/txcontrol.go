package index

import "strings"

// txControl reports the first statement of sql that controls a
// transaction (BEGIN, COMMIT, END, ROLLBACK, SAVEPOINT, RELEASE), case
// aside. Comments, string literals and quoted identifiers are skipped,
// and a trigger's BEGIN … END body is not one.
func txControl(sql string) (string, bool) {
	var (
		start   = true  // the next word starts a statement
		trigger = false // this statement is CREATE [TEMP] TRIGGER
		body    = false // inside the trigger's BEGIN … END
		words   []string
	)
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '-' && strings.HasPrefix(sql[i:], "--"):
			i = skipTo(sql, i+2, "\n")
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			i = skipTo(sql, i+2, "*/")
		case c == '\'' || c == '"' || c == '`':
			i = skipQuoted(sql, i, c)
			start = false
		case c == '[':
			i = skipTo(sql, i+1, "]")
			start = false
		case c == ';':
			i++
			if body {
				start = true // a statement of the trigger's body
				continue
			}
			start, trigger, words = true, false, nil
		case isWordByte(c):
			j := i
			for j < len(sql) && isWordByte(sql[j]) {
				j++
			}
			w := strings.ToUpper(sql[i:j])
			i = j
			atStart := start
			start = false
			if !trigger && len(words) < 3 {
				words = append(words, w)
				trigger = isCreateTrigger(words)
			}
			switch {
			case body && atStart && w == "END":
				body = false
			case body:
			case trigger && w == "BEGIN":
				body, start = true, true
			case atStart && len(words) == 1:
				switch w {
				case "BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE":
					return w, true
				}
			}
		default:
			i++
		}
	}
	return "", false
}

func isCreateTrigger(w []string) bool {
	if len(w) < 2 || w[0] != "CREATE" {
		return false
	}
	return w[1] == "TRIGGER" || (len(w) == 3 && (w[1] == "TEMP" || w[1] == "TEMPORARY") && w[2] == "TRIGGER")
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// skipTo is the index just past the next end at or after i (len: none).
func skipTo(s string, i int, end string) int {
	if k := strings.Index(s[i:], end); k >= 0 {
		return i + k + len(end)
	}
	return len(s)
}

// skipQuoted is the index just past the quote opened at i; a doubled
// quote is part of the text.
func skipQuoted(s string, i int, q byte) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == q {
			if j+1 < len(s) && s[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}
