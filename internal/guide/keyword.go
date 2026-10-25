package guide

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Hit is a section a search found, best first.
type Hit struct {
	Section
	Score   float64 `json:"score"`
	Excerpt string  `json:"excerpt"`
}

// headingWeight: a word of a section's heading counts as this many words
// of its body.
const headingWeight = 3

// Keyword ranks the sections against query with BM25 over folded, lightly
// stemmed words (accents and case folded, English and French stop words
// dropped), the heading weighted up. Offline and deterministic: equal
// scores keep the docs' order. Sections matching no word are left out.
func (c Corpus) Keyword(query string, limit int) []Hit {
	q := words(query)
	if len(q) == 0 {
		return nil
	}
	secs := c.Sections()
	docs := make([]map[string]float64, len(secs))
	lens := make([]float64, len(secs))
	df := map[string]int{}
	total := 0.0
	for i, s := range secs {
		tf := map[string]float64{}
		for _, w := range words(s.body) {
			tf[w]++
			lens[i]++
		}
		weight := float64(headingWeight - 1) // the heading line is in the body once already
		if s.Slug == "" {
			weight = 1 // a topic's preamble: its title, not in its body
		}
		for _, w := range words(s.Heading) {
			tf[w] += weight
			lens[i] += weight
		}
		for w := range tf {
			df[w]++
		}
		docs[i] = tf
		total += lens[i]
	}
	avg := total / float64(len(secs))
	const k1, b = 1.2, 0.75
	n := float64(len(secs))
	var hits []Hit
	for i, s := range secs {
		score := 0.0
		for _, w := range q {
			f := docs[i][w]
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[w])+0.5)/(float64(df[w])+0.5))
			score += idf * f * (k1 + 1) / (f + k1*(1-b+b*lens[i]/avg))
		}
		if score > 0 {
			hits = append(hits, Hit{Section: s, Score: math.Round(score*1000) / 1000, Excerpt: Excerpt(s)})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// words is s as search terms: folded, stemmed, without stop words.
func words(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(fold(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(f) < 2 || stop[f] {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

var folds = strings.NewReplacer("à", "a", "â", "a", "ä", "a", "á", "a", "é", "e", "è", "e", "ê", "e", "ë", "e", "î", "i", "ï", "i", "í", "i",
	"ô", "o", "ö", "o", "ó", "o", "ù", "u", "û", "u", "ü", "u", "ú", "u", "ç", "c", "ñ", "n", "œ", "oe", "æ", "ae", "’", "'")

func fold(s string) string { return folds.Replace(strings.ToLower(s)) }

// stem drops the commonest English endings, so restore, restores, restored
// and restoring meet; a stem keeps at least 3 letters.
func stem(w string) string {
	for _, suf := range []string{"ings", "ing", "ies", "ed", "es", "s", "e"} {
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 3 {
			w = strings.TrimSuffix(w, suf)
			if suf == "ies" {
				w += "y"
			}
			break
		}
	}
	return w
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by can do does for from has have how i if in into is it its me my
		not of on or so that the then there this to was what when where which who why will with you your
		au aux avec ce ces comment dans de des du elle en est et il je la le les leur mais me mes mon ne nous on ou où par
		pas pour quand que quel quelle qui sa se ses son sur ta te tes ton tu un une vos votre vous y est-ce`) {
		stop[fold(w)] = true
	}
}

// Excerpt is the start of the section's first paragraph, on one line.
func Excerpt(s Section) string {
	for _, p := range strings.Split(s.Text, "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") || strings.HasPrefix(p, "```") {
			continue
		}
		p = strings.Join(strings.Fields(p), " ")
		if r := []rune(p); len(r) > 200 {
			p = strings.TrimSpace(string(r[:200])) + "…"
		}
		return p
	}
	return ""
}
