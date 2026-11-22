package metrics

import (
	"slices"
	"strings"
)

// used.by_source (issue 348): credit per show by where the memory came
// from. Unlike used.by_path, it reads no settle: a show is a memory's
// first prompt or search show of its session (any agent), credited when a
// credit of that session marks it used or confirmed at or after that
// show, in any turn. Measured by hand from the journal for the efficiency
// audit's acceptance check (#230, check 4b), it is what fell outside the
// backsight project while used.by_path stayed flat.

// stepBySource is the quality step that fills used.by_source.
const stepBySource = "bysource"

// bySourceTopRank is the rank the headline stops at: the 8 hits a search
// returned before it returned every hit above its floor, so a window
// before and one after compare on the same ranks.
const bySourceTopRank = 8

// The source buckets of used.by_source, from a show's source words
// (feedback.JoinSources): one word, both a conversation's and a note's
// facts, any subagent's report, anything else, none recorded.
const (
	srcConversation     = "conversation"
	srcNote             = "note"
	srcConversationNote = "conversation+note"
	srcSubagent         = "subagent"
	srcOther            = "other"
)

// sourceBucket is the bucket of a show's source words.
func sourceBucket(words string) string {
	switch words {
	case "":
		return unknownKey
	case "session":
		return srcConversation
	case "note":
		return srcNote
	case "note+session":
		return srcConversationNote
	}
	if slices.Contains(strings.Split(words, "+"), "subagent") {
		return srcSubagent
	}
	return srcOther
}

// sourceStat is one cell of used.by_source: the shows credited, the shows,
// their share, the smallest change two windows of that many shows can
// show (nil below the metric's 20), and the cell's status.
type sourceStat struct {
	Num    int64    `json:"num"`
	Den    int64    `json:"den"`
	Value  *float64 `json:"value"`
	MDC    *float64 `json:"mdc"`
	Status Status   `json:"status"`
}

// sourcePath is one source's shows on one path: at all ranks, and at ranks
// 1 to bySourceTopRank.
type sourcePath struct {
	All  sourceStat `json:"all"`
	Top8 sourceStat `json:"top8"`
}

// creditAcc counts shows and those credited.
type creditAcc struct{ num, den int64 }

func (a *creditAcc) add(credited bool) {
	a.den++
	if credited {
		a.num++
	}
}

func (a creditAcc) stat(d Def) sourceStat {
	s := sourceStat{Num: a.num, Den: a.den, Value: ratio(a.num, a.den), Status: Judge(d, Observation{Num: a.num, Den: a.den}).Status}
	if a.den > 0 && a.den >= int64(d.MinN) {
		v := MDC(a.den)
		s.MDC = &v
	}
	return s
}

// qualityBySource fills used.by_source over the first shows of w. A
// show's source is its own, else that of the session's recall line
// nearest in time for the same memory (a search's show is read back from
// its text, the MCP server's recall line holds the hits' sources); none:
// unknown. Its project is the session's (its turn's), as every quality
// row's.
func qualityBySource(e env, w Window, obs map[string]Observation, met map[string]Metric) error {
	const id = "used.by_source"
	since, until := w.Since.UnixMilli(), w.Until.UnixMilli()
	rows, err := e.tx.QueryContext(e.ctx, `
WITH f AS (
  SELECT s.session, s.turn, s.memory_id, s.path, s.rank, s.ts, s.source,
         ROW_NUMBER() OVER (PARTITION BY s.session, s.memory_id ORDER BY s.ts, s.source_file, s."offset", s.pos) AS rn
  FROM shows s
  WHERE s.path IN ('prompt', 'search') AND s.memory_id IS NOT NULL AND `+notPage("s.memory_id")+`
    AND s.session IN (SELECT w.session FROM shows w WHERE w.ts >= ? AND w.ts < ? AND w.path IN ('prompt', 'search')))
SELECT f.path, COALESCE(f.rank, 0),
       COALESCE(f.source, (SELECT r.source FROM recalls r WHERE r.session = f.session AND r.memory_id = f.memory_id AND r.source IS NOT NULL
                           ORDER BY abs(r.ts - f.ts), r.source_file, r."offset", r.pos LIMIT 1), ''),
       COALESCE((SELECT tp.project FROM turns tp WHERE tp.session = f.session AND tp.turn = f.turn AND tp.project IS NOT NULL
                 ORDER BY tp.ts, tp.source_file, tp."offset" LIMIT 1), ''),
       EXISTS (SELECT 1 FROM credits c WHERE c.session = f.session AND c.memory_id = f.memory_id
               AND c.verdict IN ('used', 'confirmed') AND c.ts >= f.ts)
FROM f
WHERE f.rn = 1 AND f.ts >= ? AND f.ts < ? AND `+sessionProject("f")+` AND `+sessionVersion("f"),
		since, until, since, until, e.q.Project, e.q.Project, e.q.version, e.q.version)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type cellAcc struct{ all, top creditAcc }
	bySrc := map[string]map[string]*cellAcc{}
	byProject := map[string]*creditAcc{}
	var all, top creditAcc
	for rows.Next() {
		var (
			path, source, project string
			rank                  int64
			credited              bool
		)
		if err := rows.Scan(&path, &rank, &source, &project, &credited); err != nil {
			return err
		}
		b := sourceBucket(source)
		if bySrc[b] == nil {
			bySrc[b] = map[string]*cellAcc{}
		}
		if bySrc[b][path] == nil {
			bySrc[b][path] = &cellAcc{}
		}
		c := bySrc[b][path]
		c.all.add(credited)
		all.add(credited)
		if rank < 1 || rank > bySourceTopRank {
			continue
		}
		c.top.add(credited)
		top.add(credited)
		if project == "" {
			project = unknownKey
		}
		if byProject[project] == nil {
			byProject[project] = &creditAcc{}
		}
		byProject[project].add(credited)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	def, _ := DefOf(id)
	sources := make(map[string]map[string]sourcePath, len(bySrc))
	for b, paths := range bySrc {
		sources[b] = make(map[string]sourcePath, len(paths))
		for p, c := range paths {
			sources[b][p] = sourcePath{All: c.all.stat(def), Top8: c.top.stat(def)}
		}
	}
	projects := make(map[string]sourceStat, len(byProject))
	for p, a := range byProject {
		projects[p] = a.stat(def)
	}
	obs[id] = Observation{Num: top.num, Den: top.den}
	met[id] = Metric{Num: i64(top.num), Den: i64(top.den), Value: ratio(top.num, top.den),
		Extra: map[string]any{"by_source": sources, "by_project": projects, "all_ranks": all.stat(def), "top_rank": int64(bySourceTopRank)}}
	return nil
}
