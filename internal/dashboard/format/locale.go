// Package format writes the dashboard's numbers, deltas and times for a
// page, in the language of the browser's Accept-Language. The command line
// keeps internal/views and its own formats.
package format

import (
	"strconv"
	"strings"
)

// Locale is how a page writes numbers. An empty Group or Decimal is
// English's, so the zero Locale writes English.
type Locale struct{ Tag, Group, Decimal string }

// English is the default locale.
var English = Locale{Tag: "en", Group: ",", Decimal: "."}

const (
	nnbsp = " "
	nbsp  = " "
)

// known maps a primary subtag (or language-region) to its separators.
var known = map[string][2]string{
	"en": {",", "."}, "ja": {",", "."}, "zh": {",", "."}, "ko": {",", "."},
	"fr": {nnbsp, ","},
	"de": {".", ","}, "es": {".", ","}, "it": {".", ","}, "nl": {".", ","},
	"pt": {".", ","}, "da": {".", ","}, "id": {".", ","},
	"pl": {nbsp, ","}, "ru": {nbsp, ","}, "sv": {nbsp, ","}, "nb": {nbsp, ","},
	"nn": {nbsp, ","}, "fi": {nbsp, ","}, "cs": {nbsp, ","}, "uk": {nbsp, ","},
	"de-ch": {"’", "."}, "it-ch": {"’", "."},
}

// maxEntries is how many comma-separated entries of the header are read.
const maxEntries = 32

func (l Locale) sepGroup() string {
	if l.Group == "" {
		return English.Group
	}
	return l.Group
}

func (l Locale) sepDecimal() string {
	if l.Decimal == "" {
		return English.Decimal
	}
	return l.Decimal
}

// FromAcceptLanguage picks the first tag by q (ties: header order) whose
// primary subtag is known. q=0 is refused, an entry with a bad q is
// skipped, at most 32 entries are read; anything else gives English.
func FromAcceptLanguage(h string) Locale {
	best, bestQ := English, 0.0
	found := false
	for i := 0; i < maxEntries && h != ""; i++ {
		var entry string
		entry, h, _ = strings.Cut(h, ",")
		tag, q, ok := parseEntry(entry)
		if !ok || q <= 0 {
			continue
		}
		loc, ok := lookup(tag)
		if !ok {
			continue
		}
		if !found || q > bestQ {
			best, bestQ, found = loc, q, true
		}
	}
	return best
}

// parseEntry splits "fr-FR;q=0.8" into its lowercase tag and q (1 when
// absent); ok is false for a q that is not a number from 0 to 1.
func parseEntry(entry string) (tag string, q float64, ok bool) {
	parts := strings.Split(entry, ";")
	tag = strings.ToLower(strings.TrimSpace(parts[0]))
	q = 1
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if len(p) < 2 || (p[0] != 'q' && p[0] != 'Q') {
			continue
		}
		v := strings.TrimSpace(p[1:])
		if !strings.HasPrefix(v, "=") {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v[1:]), 64)
		if err != nil || !(f >= 0 && f <= 1) {
			return "", 0, false
		}
		q = f
	}
	return tag, q, tag != ""
}

func lookup(tag string) (Locale, bool) {
	primary, rest, _ := strings.Cut(tag, "-")
	if len(primary) < 2 || len(primary) > 3 {
		return Locale{}, false
	}
	if region, _, _ := strings.Cut(rest, "-"); region == "ch" {
		if s, ok := known[primary+"-ch"]; ok {
			return Locale{Tag: primary + "-CH", Group: s[0], Decimal: s[1]}, true
		}
	}
	s, ok := known[primary]
	if !ok {
		return Locale{}, false
	}
	return Locale{Tag: primary, Group: s[0], Decimal: s[1]}, true
}
