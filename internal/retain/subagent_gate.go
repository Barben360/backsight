package retain

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The subagent report gate (decision 0083 C3): measured on a real bank,
// 1,274 of 1,406 subagent reports were under 150 characters ("Keep going
// with block 2", "Reading x.go"); they cost an extraction each, and the
// observations drawn only from them were credited 1 time in 112 shows. A
// report is kept when it is long enough, or when it names something to
// find again (a URL, an issue or a pull request, a commit), whatever its
// length.

// subagentGateRule names the rule of KeepsSubagentReport; it changes
// whenever the rule does, so that jobs maintain sweeps again.
const subagentGateRule = "v2"

var (
	// A URL, or a GitHub path without its scheme.
	reportURL = regexp.MustCompile(`\bhttps?://[^\s<>()\[\]]+|\bgithub\.com/[\w.-]+`)
	// #123 standing alone (not &#123; nor x#123), owner/repo#123, or a
	// named one: PR 205, PR#205, pull 3, issue 12, GH-7.
	reportRef = regexp.MustCompile(`(?:^|[^\w&#/])#[0-9]+\b|\b[\w.-]+/[\w.-]+#[0-9]+\b|(?i:\b(?:PR|pull|issue|GH)[ #-]*#?[0-9]+\b)`)
	// A commit: 7 to 40 hex characters, with a digit and a letter (so
	// neither a word like "deadbeef" nor a plain number).
	reportHex = regexp.MustCompile(`\b[0-9a-fA-F]{7,40}\b`)
)

// holdsCommit: text names a commit hash.
func holdsCommit(text string) bool {
	for _, h := range reportHex.FindAllString(text, -1) {
		if strings.ContainsAny(h, "0123456789") && strings.ContainsAny(strings.ToLower(h), "abcdef") {
			return true
		}
	}
	return false
}

// KeepsSubagentReport: the report, as SubagentReport keeps it, passes the
// gate: at least minChars characters once trimmed, or a URL (github.com/
// paths without a scheme too), an issue or pull request reference (#123,
// owner/repo#123, PR 205, issue 12, GH-7), or a commit hash. An empty
// report never passes; minChars 0 keeps every other one.
func KeepsSubagentReport(text string, minChars int) bool {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return false
	case utf8.RuneCountInString(text) >= minChars:
		return true
	}
	return reportURL.MatchString(text) || reportRef.MatchString(text) || holdsCommit(text)
}

// SubagentGate is the gate's version with its parameters, as the sweep's
// marker records it: a different value sweeps again.
func SubagentGate(minChars int) string {
	return fmt.Sprintf("%s:min_chars=%d", subagentGateRule, minChars)
}

// ReportLengthBucket is the telemetry bucket of a report's length, in
// characters once trimmed.
func ReportLengthBucket(text string) string {
	n := utf8.RuneCountInString(strings.TrimSpace(text))
	for _, b := range []struct {
		below int
		name  string
	}{{50, "<50"}, {150, "50-149"}, {400, "150-399"}, {1000, "400-999"}} {
		if n < b.below {
			return b.name
		}
	}
	return "1000+"
}
