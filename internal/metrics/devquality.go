package metrics

import (
	"math"
	"sort"
	"strings"
)

// The developer metrics on quality: the corrections that follow memory
// (injection.contradicted, from the telemetry's outcomes) and the recalls the
// learned ranking changed (boost.set_changed, from the journal); and the
// language test of recall.by_lang, which lives in reach.go's pass over the
// prompts. Both quality metrics answer to the `stats` command's figures on the
// same data: its injections line and its Boost line.

// The steps of the quality family that fill the two metrics, and the one
// of stopcheck.block_yield (the end-of-turn blocks, from the telemetry).
const (
	stepContradicted = "contradicted"
	stepSetChanged   = "setchanged"
	stepBlocks       = "blocks"
)

// blockOutcomes are what a held turn's agent can then declare, in the
// order by_outcome lists them; used and confirmed credit a memory.
var blockOutcomes = []string{"used", "confirmed", "corrected", "none", "refused"}

// qualityBlocks fills stopcheck.block_yield: the end-of-turn blocks of the
// window (each written when its turn ended again), and those whose agent
// then credited a memory used or confirmed; by outcome, and by the paths
// shown when it held, each path judged on its own blocks.
func qualityBlocks(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "stopcheck.block_yield"
	rows, err := e.tx.QueryContext(e.ctx, `
SELECT COALESCE(outcome, ''), COALESCE(paths, '') FROM tel_stop_block
WHERE ts >= ? AND ts < ? AND `+projectClause+` AND `+versionClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project, e.q.version, e.q.version)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	byOutcome := make(map[string]int64, len(blockOutcomes))
	for _, o := range blockOutcomes {
		byOutcome[o] = 0
	}
	type acc struct{ num, den int64 }
	byPath := map[string]*acc{}
	var num, den int64
	for rows.Next() {
		var outcome, paths string
		if err := rows.Scan(&outcome, &paths); err != nil {
			return err
		}
		if outcome == "" {
			outcome = unknownKey
		}
		credited := outcome == "used" || outcome == "confirmed"
		den, byOutcome[outcome] = den+1, byOutcome[outcome]+1
		if credited {
			num++
		}
		for _, p := range strings.Split(paths, ",") {
			if p == "" {
				continue
			}
			if byPath[p] == nil {
				byPath[p] = &acc{}
			}
			byPath[p].den++
			if credited {
				byPath[p].num++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	def, _ := DefOf(id)
	paths := make(map[string]pathStat, len(byPath))
	for p, a := range byPath {
		j := Judge(def, Observation{Num: a.num, Den: a.den})
		paths[p] = pathStat{Num: a.num, Den: a.den, Value: ratio(a.num, a.den), Status: j.Status}
	}
	obs[id] = Observation{Num: num, Den: den}
	met[id] = Metric{Num: i64(num), Den: i64(den), Value: ratio(num, den), Extra: map[string]any{"by_outcome": byOutcome, "by_path": paths}}
	return nil
}

// flagIn is the SQL test that a row's comma-joined flags hold flag (the bind
// value is the flag with its commas).
const flagIn = `instr(',' || COALESCE(flags, '') || ',', ?) > 0`

// qualityContradicted fills injection.contradicted: the outcomes of the
// prompts that injected memory (ids_n > 0), and those that read as a
// correction. A recall that injected nothing has no outcome to judge.
func qualityContradicted(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "injection.contradicted"
	var den, num int64
	err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*), COALESCE(SUM(contradicted = 1), 0) FROM tel_outcome
WHERE ts >= ? AND ts < ? AND ids_n > 0 AND `+projectClause+` AND `+versionClause,
		w.Since.UnixMilli(), w.Until.UnixMilli(), e.q.Project, e.q.Project, e.q.version, e.q.version).Scan(&den, &num)
	if err != nil {
		return err
	}
	obs[id] = Observation{Num: num, Den: den}
	met[id] = Metric{Num: i64(num), Den: i64(den), Value: ratio(num, den)}
	return nil
}

// qualitySetChanged fills boost.set_changed: the prompt recalls (a shows
// line of path prompt) and the searches (a recall line) of the window, and
// those whose flags say the learned ranking changed which memories were
// shown. A line that only reordered does not count as changed.
func qualitySetChanged(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "boost.set_changed"
	flag := ",set_changed,"
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	var prompts, promptsChanged, searches, searchesChanged int64
	err := e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(*), COALESCE(SUM(`+flagIn+`), 0) FROM show_sets s
WHERE s.path = 'prompt' AND s.ts >= ? AND s.ts < ? AND `+sessionProject("s")+` AND `+sessionVersion("s"),
		flag, since, until, e.q.Project, e.q.Project, e.q.version, e.q.version).Scan(&prompts, &promptsChanged)
	if err != nil {
		return err
	}
	// A recall line has one row per memory: count the lines.
	err = e.tx.QueryRowContext(e.ctx, `
SELECT COUNT(DISTINCT r.source_file || ':' || r."offset"),
       COUNT(DISTINCT CASE WHEN `+flagIn+` THEN r.source_file || ':' || r."offset" END)
FROM recalls r
WHERE r.path = 'search' AND r.ts >= ? AND r.ts < ? AND `+sessionProject("r")+` AND `+sessionVersion("r"),
		flag, since, until, e.q.Project, e.q.Project, e.q.version, e.q.version).Scan(&searches, &searchesChanged)
	if err != nil {
		return err
	}
	// The shown sets are recorded from the release that added them: shows
	// of the window before the first one have none, so the figure is short
	// until the index is rebuilt.
	var partial bool
	err = e.tx.QueryRowContext(e.ctx, `
SELECT EXISTS (SELECT 1 FROM shows WHERE ts >= ? AND ts < MIN(?, COALESCE((SELECT MIN(ts) FROM show_sets), ?)))`,
		since, until, until).Scan(&partial)
	if err != nil {
		return err
	}
	num, den := promptsChanged+searchesChanged, prompts+searches
	m := Metric{Num: i64(num), Den: i64(den), Value: ratio(num, den), Extra: map[string]any{
		"prompts": prompts, "prompts_set_changed": promptsChanged, "searches": searches, "searches_set_changed": searchesChanged}}
	if partial {
		m.Tags = []string{tagPartial}
	}
	obs[id], met[id] = Observation{Num: num, Den: den}, m
	return nil
}

// devQualityAbsent reports whether the table of a developer step holds no
// row at all (any time, the project's): its source is empty.
func devQualityAbsent(e env, step string) (bool, error) {
	var found bool
	var err error
	switch step {
	case stepContradicted:
		err = e.tx.QueryRowContext(e.ctx, `SELECT EXISTS (SELECT 1 FROM tel_outcome WHERE `+projectClause+`)`,
			e.q.Project, e.q.Project).Scan(&found)
	case stepBlocks:
		err = e.tx.QueryRowContext(e.ctx, `SELECT EXISTS (SELECT 1 FROM tel_stop_block WHERE `+projectClause+`)`,
			e.q.Project, e.q.Project).Scan(&found)
	default:
		err = e.tx.QueryRowContext(e.ctx, `
SELECT EXISTS (SELECT 1 FROM show_sets s WHERE `+sessionProject("s")+`)
    OR EXISTS (SELECT 1 FROM recalls r WHERE `+sessionProject("r")+`)
    OR EXISTS (SELECT 1 FROM shows s WHERE `+sessionProject("s")+`)`,
			e.q.Project, e.q.Project, e.q.Project, e.q.Project, e.q.Project, e.q.Project).Scan(&found)
	}
	return !found, err
}

// langGap is one language of the language test, against the reference.
type langGap struct {
	Lang     string     `json:"lang"`
	Num      int64      `json:"num"`
	Den      int64      `json:"den"`
	Value    float64    `json:"value"`
	Gap      float64    `json:"gap"`      // the reference's share minus this language's
	Interval [2]float64 `json:"interval"` // the gap's 95 % interval
}

// langCount is a language the test left out: it has too few prompts.
type langCount struct {
	Lang string `json:"lang"`
	Num  int64  `json:"num"`
	Den  int64  `json:"den"`
}

// langTest is recall.by_lang's extra "test".
type langTest struct {
	Reference string      `json:"reference"`
	RefNum    int64       `json:"reference_num"`
	RefDen    int64       `json:"reference_den"`
	Gaps      []langGap   `json:"gaps"`
	LeftOut   []langCount `json:"left_out"`
}

// langGapLimit is the gap, in share of prompts, past which a language reads
// as behind.
const langGapLimit = 0.15

// LangGapPoints is that limit in points, for the words that name it.
const LangGapPoints = int(langGapLimit * 100)

// LangBehind reports whether a language's gap to the reference (the
// reference's share minus its own) and the gap's 95 % interval make it read
// as behind or ahead: the gap is over the limit in either direction and the
// interval excludes 0. The language test and the page's watch line both use it.
func LangBehind(gap, lo, hi float64) bool {
	return math.Abs(gap) > langGapLimit && (lo > 0 || hi < 0)
}

// langTestOf compares each language to the one with the most recalls (the
// first by name on a tie). Prompts without a language are not a language:
// they are neither the reference nor tested. A language needs minN recalls of
// its own; the others are listed with their counts. watch is true when some
// gap is over langGapLimit in either direction and its 95 % interval
// (Wald: ±1.96·√(p₁(1−p₁)/n₁ + p₂(1−p₂)/n₂)) excludes 0.
func langTestOf(langs map[string][2]int64, minN int64) (lt langTest, watch bool) {
	names := make([]string, 0, len(langs))
	for l := range langs {
		if l != unknownKey {
			names = append(names, l)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := langs[names[i]][1], langs[names[j]][1]
		if a != b {
			return a > b
		}
		return names[i] < names[j]
	})
	lt = langTest{Gaps: []langGap{}, LeftOut: []langCount{}}
	if len(names) == 0 {
		return lt, false
	}
	ref := langs[names[0]]
	lt.Reference, lt.RefNum, lt.RefDen = names[0], ref[0], ref[1]
	sort.Strings(names[1:])
	p1 := float64(ref[0]) / float64(ref[1])
	for _, l := range names[1:] {
		c := langs[l]
		if c[1] < minN || ref[1] < minN {
			lt.LeftOut = append(lt.LeftOut, langCount{Lang: l, Num: c[0], Den: c[1]})
			continue
		}
		p2 := float64(c[0]) / float64(c[1])
		gap := p1 - p2
		half := 1.96 * math.Sqrt(p1*(1-p1)/float64(ref[1])+p2*(1-p2)/float64(c[1]))
		lo, hi := gap-half, gap+half
		lt.Gaps = append(lt.Gaps, langGap{Lang: l, Num: c[0], Den: c[1], Value: p2, Gap: gap, Interval: [2]float64{lo, hi}})
		if LangBehind(gap, lo, hi) {
			watch = true
		}
	}
	return lt, watch
}
