// Package staleref finds, in a memory's text, the GitHub pull requests and
// issues it names and whether it says they are still open: the facts that
// go stale as soon as the item is merged or closed (issue 331). Words
// only, no network: the caller checks the refs against GitHub.
package staleref

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Ref is a pull request or issue a text names: Owner and Repo (lower case)
// from a GitHub URL or an owner/repo#N, else bare (PR #N), with the word
// just before it when that word may be a repository's name (Qualifier,
// lower case: "backsight" in "backsight PR #189").
type Ref struct {
	Owner, Repo string
	Qualifier   string
	Number      int
}

// Bare is a ref with no owner and repository.
func (r Ref) Bare() bool { return r.Repo == "" }

var (
	urlRef  = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9][\w.-]*)/([\w.-]+?)/(?:pull|issues)/(\d+)\b`)
	slugRef = regexp.MustCompile(`\b([A-Za-z0-9][\w.-]*)/([\w.-]+)#(\d+)\b`)
	bareRef = regexp.MustCompile(`(?i)(?:\b([a-z][\w.-]*)[ \t]+)?\b(?:PR|pull request|issue)[ \t]*#?(\d+)\b`)
)

// notQualifier: words that come before "PR #N" without naming a repository.
var notQualifier = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "for": true, "its": true, "this": true, "that": true, "and": true,
	"or": true, "to": true, "in": true, "on": true, "by": true, "with": true, "from": true, "as": true, "is": true,
	"was": true, "his": true, "her": true, "their": true, "your": true, "my": true, "our": true, "same": true,
	"new": true, "draft": true, "open": true, "merged": true, "closed": true, "fix": true, "via": true, "see": true,
	"then": true, "after": true, "before": true, "into": true, "each": true, "every": true, "one": true, "which": true,
}

// Refs lists the pull requests and issues text names, in order, each
// once. A bare ref whose number a URL or owner/repo#N ref already has is
// that ref named again.
func Refs(text string) []Ref {
	type at struct {
		pos int
		ref Ref
	}
	var found []at
	full := map[int]bool{}
	for _, re := range []*regexp.Regexp{urlRef, slugRef} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			n, err := strconv.Atoi(text[m[6]:m[7]])
			if err != nil {
				continue
			}
			found = append(found, at{m[0], Ref{Owner: strings.ToLower(text[m[2]:m[3]]), Repo: strings.ToLower(text[m[4]:m[5]]), Number: n}})
			full[n] = true
		}
	}
	for _, m := range bareRef.FindAllStringSubmatchIndex(text, -1) {
		n, err := strconv.Atoi(text[m[4]:m[5]])
		if err != nil || full[n] {
			continue
		}
		r := Ref{Number: n}
		if m[2] >= 0 {
			if q := strings.ToLower(text[m[2]:m[3]]); !notQualifier[q] {
				r.Qualifier = q
			}
		}
		found = append(found, at{m[0], r})
	}
	slices.SortStableFunc(found, func(a, b at) int { return a.pos - b.pos })
	var out []Ref
	for _, f := range found {
		if !slices.Contains(out, f.ref) {
			out = append(out, f.ref)
		}
	}
	return out
}

var openClaims = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:is|was|are|were|still|remains?|remained|stays?|stayed|kept|left)[ \t]+(?:[a-z]+[ \t]+)?open\b`),
	regexp.MustCompile(`(?i)\bnot[ \t]+(?:yet[ \t]+)?(?:been[ \t]+)?merged\b`),
	regexp.MustCompile(`(?i)\bunmerged\b`),
	regexp.MustCompile(`(?i)\bready to merge\b`),
	regexp.MustCompile(`(?i)\bawait(?:s|ing)?\b`),
	regexp.MustCompile(`(?i)\bpending[ \t]+(?:the user's[ \t]+|a[ \t]+|your[ \t]+)?(?:review|merge|approval|ci)\b`),
}

// ClaimsOpen is whether text says something is not done yet: open, not
// merged, waiting for a review or a merge.
func ClaimsOpen(text string) bool {
	for _, re := range openClaims {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

var doneClaims = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:was|were|been|is|are|got|later|then|since|now|finally|already)[ \t]+(?:squash-)?merged\b`),
	regexp.MustCompile(`(?i)\bsquash-merged\b`),
	regexp.MustCompile(`(?i)\bmerged[ \t]+(?:into|to)[ \t]+\w`),
	regexp.MustCompile(`(?i)\bmerged[ \t]+as[ \t]+(?:commit[ \t]+)?[0-9a-f]{7,}\b`),
	regexp.MustCompile(`(?i)\bno longer open\b`),
	regexp.MustCompile(`(?i)\b(?:was|were|been|is|are|now|later)[ \t]+closed\b`),
	regexp.MustCompile(`(?i)\bclosed[ \t]+(?:by|as|on|via)\b`),
	regexp.MustCompile(`(?i)\bdelivered[ \t]+(?:by|in)\b`),
}

var negation = regexp.MustCompile(`(?i)(?:\b(?:not|never|nothing|no)\b|n't)`)

// SaysDone is whether text also says the item was merged, closed or
// delivered since, not negated by the three words before ("has not been
// merged", "nothing has been merged"): such a fact holds its own update.
func SaysDone(text string) bool {
	for _, re := range doneClaims {
		for _, m := range re.FindAllStringIndex(text, -1) {
			words := strings.Fields(text[:m[0]])
			if len(words) > 3 {
				words = words[len(words)-3:]
			}
			if !negation.MatchString(strings.Join(words, " ")) {
				return true
			}
		}
	}
	return false
}

// Names lists the projects of projects that text names as a whole word
// (a letter, digit, "_" or "-" on neither side), case-insensitive,
// sorted. Names starting with a dot (the transverse pool's) never count.
func Names(text string, projects []string) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, p := range projects {
		lp := strings.ToLower(p)
		if lp == "" || strings.HasPrefix(lp, ".") || slices.Contains(out, lp) {
			continue
		}
		for i := 0; ; {
			j := strings.Index(lower[i:], lp)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(lp)
			if !nameByte(lower, start-1) && !nameByte(lower, end) {
				out = append(out, lp)
				break
			}
			i = start + 1
		}
	}
	slices.Sort(out)
	return out
}

func nameByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c >= 0x80
}
