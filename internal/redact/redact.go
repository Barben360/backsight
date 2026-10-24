// Package redact removes secrets from text before it leaves the machine
// (SPEC §13). It favours precision on ordinary prose: a pattern must look
// like a real credential, not merely mention one.
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
}

var rules = []rule{
	{"private_key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`), 0},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`), 0},
	{"openai_key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}`), 0},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`), 0},
	{"slack_token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), 0},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), 0},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), 0},
	{"bearer", regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{16,}=*)`), 1},
	{"url_password", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@`), 1},
	// A value assigned to a secret-named key: FOO_TOKEN=…, "apiToken": "…", password: ….
	{"secret_value", regexp.MustCompile(`(?i)["']?\b[a-z0-9_.-]*(?:secret|token|passw(?:or)?d|api_?key|apikey|private_?key|access_?key|credentials?)\b["']?\s*[:=]\s*["']?([^\s"',;}]{8,})`), 1},
}

// credentialLike: letters with digits or symbols, or a long token; plain
// words ("token : Paramètres") and regex fragments are prose.
func credentialLike(v string) bool {
	if strings.ContainsAny(v, `*\^`) {
		return false
	}
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

// placeholder values that only name a secret.
func placeholder(v string) bool {
	if strings.HasPrefix(v, "$") || strings.HasPrefix(v, "<") || strings.HasPrefix(v, "{{") || strings.HasPrefix(v, "[redacted") ||
		strings.HasPrefix(v, "***") || strings.HasPrefix(v, "%") {
		return true
	}
	digits := true
	for _, r := range v {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	return digits
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
			if r.kind == "secret_value" && (placeholder(v) || !credentialLike(v)) {
				return m
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
