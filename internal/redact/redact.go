// Package redact removes secrets from text before it leaves the machine
// (SPEC §13). Credential shapes and secret-named values are caught; plain
// prose mentioning a secret is kept.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

type rule struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch replaced (0: the whole match).
	group int
	// keep decides, for a matched value, that it is not a secret.
	keep func(key, value string) bool
}

// keyRE is a secret-named key: any identifier containing one of the
// keywords, with prefixes and suffixes (SECRET_KEY, GITHUB_TOKEN_PROD,
// apiToken).
const keyRE = `[a-zA-Z0-9_.-]*(?:secret|token|passw(?:or)?d|pwd|api_?key|apikey|private_?key|access_?key|credentials?|auth_?key)[a-zA-Z0-9_.-]*`

var rules = []rule{
	{kind: "private_key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)},
	{kind: "anthropic_key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{kind: "openai_key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}`)},
	{kind: "stripe_key", re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{kind: "github_token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{kind: "npm_token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`)},
	{kind: "slack_token", re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{kind: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "google_api_key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{kind: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{kind: "bearer", re: regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{16,}=*)`), group: 1},
	{kind: "basic_auth", re: regexp.MustCompile(`(?i)\bbasic\s+([A-Za-z0-9+/]{12,}=*)`), group: 1},
	// The password of user:pass@host runs to the last @ before whitespace.
	{kind: "url_password", re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:(\S+)@[^\s@]+`), group: 1},
	{kind: "cli_password", re: regexp.MustCompile(`(?:\s|^)(?:-u|--user)\s+[^\s:-][^\s:]*:(\S+)`), group: 1},
	{kind: "cli_password", re: regexp.MustCompile(`--password[=\s]+(\S+)`), group: 1},
	{kind: "cli_password", re: regexp.MustCompile(`\b(?:mysql|mysqldump|mariadb|mysqladmin)\b[^\n]*?\s-p(\S{4,})`), group: 1},
	// A value assigned to a secret-named key: quoted (spaces allowed) or bare.
	{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + keyRE + `)\b["']?\s*[:=]\s*"([^"\n]{6,})"`), group: 2, keep: keepValue},
	{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + keyRE + `)\b["']?\s*[:=]\s*'([^'\n]{6,})'`), group: 2, keep: keepValue},
	{kind: "secret_value", re: regexp.MustCompile(`(?i)["']?\b(` + keyRE + `)\b["']?\s*[:=]\s*([^\s"',;}]{6,})`), group: 2, keep: keepValue},
	// A long mixed-case alphanumeric token anywhere (hex, UUIDs, digests and
	// identifiers made of words are kept).
	{kind: "high_entropy", re: regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`), keep: func(_, v string) bool { return !highEntropy(v) }},
}

var passwordKeys = regexp.MustCompile(`(?i)passw(?:or)?d|pwd|secret`)

// strongKeys name a secret on their own: any 6+ character value is one.
var strongKeys = regexp.MustCompile(`(?i)passw(?:or)?d|pwd|secret|token|private_?key|credentials?`)

// keepValue decides that a secret-named key's value is not a secret.
func keepValue(key, v string) bool {
	v = strings.Trim(v, "`") // markdown code spans in prose
	if len(v) < 6 {
		return true
	}
	if strings.Contains(v, "://") {
		return true // a URL named after a token page, not a token
	}
	if strings.HasPrefix(v, "$") || strings.HasPrefix(v, "<") || strings.HasPrefix(v, "{{") || strings.HasPrefix(v, "[redacted") ||
		strings.HasPrefix(v, "***") || strings.HasPrefix(v, "%") || strings.ContainsAny(v, `*\^`) {
		return true
	}
	if numeric(v) {
		// Counters and timings such as max_tokens: 50000 or per_token_ms: 41.2;
		// a numeric password still goes.
		return !passwordKeys.MatchString(key)
	}
	lower := strings.ToLower(key)
	if strings.HasSuffix(lower, "tokens") || strings.Contains(lower, "token_count") {
		return true // token counts, not tokens
	}
	for _, r := range v {
		if r > 127 {
			return true // accented words are prose ("token : Paramètres")
		}
	}
	if strongKeys.MatchString(key) && (credentialLike(v) || len(v) >= 12) {
		return false // a short plain word after "token :" is prose
	}
	return !credentialLike(v)
}

func numeric(v string) bool {
	digit := false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.' || r == '-' || r == '+' || r == 'e' || r == 'E':
		default:
			return false
		}
	}
	return digit
}

// credentialLike: letters with digits or symbols, or a long token; plain
// words are prose.
func credentialLike(v string) bool {
	var letter, digit, symbol bool
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letter = true
		case r > 127:
			return false // accented words are prose
		default:
			symbol = true
		}
	}
	return letter && (digit || symbol || len(v) >= 24)
}

// highEntropy: upper, lower and digits mixed with frequent changes of
// character class, as in random tokens; identifiers made of words
// (expediteurFingerprint, Barben360-ref-stack) change class rarely.
func highEntropy(v string) bool {
	class := func(r rune) int {
		switch {
		case r >= 'A' && r <= 'Z':
			return 1
		case r >= 'a' && r <= 'z':
			return 2
		case r >= '0' && r <= '9':
			return 3
		}
		return 0
	}
	var counts [4]int
	changes, prev := 0, -1
	for _, r := range v {
		c := class(r)
		counts[c]++
		if c == 0 {
			continue // separators split words, they are not randomness
		}
		if prev >= 0 && c != prev {
			changes++
		}
		prev = c
	}
	if counts[1] < 2 || counts[2] < 2 || counts[3] < 2 {
		return false
	}
	return float64(changes)/float64(len(v)) >= 0.3
}

// Text returns s with secrets replaced by [redacted:<kind>] and the sorted
// kinds found.
func Text(s string) (string, []string) {
	found := map[string]bool{}
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			sub := r.re.FindStringSubmatchIndex(m)
			start, end := sub[2*r.group], sub[2*r.group+1]
			if start < 0 {
				return m
			}
			v := m[start:end]
			if r.keep != nil {
				key := ""
				if r.group == 2 && sub[2] >= 0 {
					key = m[sub[2]:sub[3]]
				}
				if r.keep(key, v) {
					return m
				}
			}
			found[r.kind] = true
			return m[:start] + "[redacted:" + r.kind + "]" + m[end:]
		})
	}
	kinds := make([]string, 0, len(found))
	for k := range found {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return s, kinds
}
