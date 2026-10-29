// Package recall searches the memory across scopes (SPEC §4 Read scopes,
// §10.2): the current project with the transverse aliases and the projects
// its .backsight.toml relates, the whole bank
// and related projects, queried as separate calls so project memories are
// not crowded out, then merged by best score with a boost for the current
// project.
package recall

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

const (
	// ProjectBoost is added to the score of the current project's memories.
	ProjectBoost = 0.05
	// MaxRelated caps the related projects searched (SPEC §4).
	MaxRelated = 6
	// MaxAsked caps the projects a caller asks for explicitly.
	MaxAsked  = 10
	maxTokens = 1500 // per call, as the reference hook (C12)
)

// Engine runs searches against one bank.
type Engine struct {
	Client     *hindsight.Client
	Bank       string
	Transverse []string      // aliases; the first is canonical
	Timeout    time.Duration // the whole search: every call and the raw fallback
	Now        func() time.Time
	Boost      float64 // current-project boost; 0 = ProjectBoost (A.1 recall.project_boost)
	// Adjust is what this machine learned of a memory: a boost added to
	// its score, or hidden to drop it. Nil: neither.
	Adjust func(id string) (boost float64, hidden bool)
}

// Query is one search.
type Query struct {
	Queries     []string
	Current     string   // the current project
	Linked      []string // the repository's .backsight.toml related: searched with the current project
	Projects    []string // projects asked for explicitly (all searched, up to MaxAsked)
	Related     []string // projects matched in the entity index (up to MaxRelated)
	All         bool     // the whole bank only, no project scopes
	Limit       int      // 0: no limit
	MinScore    float64  // on the boosted score
	MinReranker float64  // on the server's score (prompt recall: SPEC §10.2)
	// GlobalMinReranker: hits of projects that are neither the current one,
	// a transverse alias nor linked/asked/related need this score (0046 poisoning
	// defence: recall.global_min_score).
	GlobalMinReranker float64
	// Kinds asks the server, in the same calls, for the source facts of each
	// observation to fill Hit.Kinds: for searches a reader sees, not for the
	// prompt hook (larger responses).
	Kinds bool
}

// Hit is one merged result.
type Hit struct {
	ID       string  `json:"id"`
	Text     string  `json:"text"`
	Project  string  `json:"project"`
	Type     string  `json:"type"`
	Scope    string  `json:"scope"`
	Score    float64 `json:"score"`    // reranker + current-project boost + Boost
	Reranker float64 `json:"reranker"` // the server's score
	// Boost: what Engine.Adjust added to Score. RankWithoutBoost: the
	// hit's place without it, from 1, 0 when only the boost let it in;
	// only when the engine has an Adjust.
	Boost            float64   `json:"boost,omitempty"`
	RankWithoutBoost int       `json:"rank_without_boost,omitempty"`
	Date             time.Time `json:"date"`
	Age              string    `json:"age"`
	// Kinds: the kinds (decision, pitfall...) of the facts the memory was
	// built from, or of the fact itself for a raw hit; only on a search that
	// asked for them (Query.Kinds). Subagent: some of those facts came from
	// a subagent's report.
	Kinds    []string `json:"kinds,omitempty"`
	Subagent bool     `json:"subagent,omitempty"`
}

// ScopeMeta is how one scope's calls went (telemetry, A.4).
type ScopeMeta struct {
	OK  bool   `json:"ok"`
	MS  int    `json:"ms"`
	Raw int    `json:"raw"` // results before the score floor
	Err string `json:"err,omitempty"`
	// Unauthorized: the server refused the API key.
	Unauthorized bool `json:"-"`
}

// Result of a search. Raw: some hits are raw facts, standing in for a scope
// that holds no observation yet (a project not consolidated yet, SPEC §4
// Reads).
type Result struct {
	Hits   []Hit                `json:"hits"`
	Scopes map[string]ScopeMeta `json:"scopes"`
	Raw    bool                 `json:"raw"`
}

type scope struct {
	name string
	tags []string
}

func (e *Engine) scopes(q Query) []scope {
	if q.All {
		return []scope{{name: "global"}}
	}
	own := []string{q.Current}
	for _, list := range [][]string{e.Transverse, q.Linked} {
		for _, p := range list {
			if p != "" && !contains(own, p) {
				own = append(own, p)
			}
		}
	}
	out := []scope{{name: "project", tags: projectTags(own)}, {name: "global"}}
	var related []string
	add := func(list []string, limit int) {
		n := 0
		for _, p := range list {
			if n < limit && p != "" && !contains(own, p) && !contains(related, p) {
				related, n = append(related, p), n+1
			}
		}
	}
	add(q.Projects, MaxAsked)
	add(q.Related, MaxRelated)
	if len(related) > 0 {
		out = append(out, scope{name: "related", tags: projectTags(related)})
	}
	return out
}

// Search runs every (scope, query) pair in parallel within one budget and
// merges the hits. A scope that answered without any observation is asked
// again for raw facts, still within the budget.
func (e *Engine) Search(ctx context.Context, q Query) Result {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	scopes := e.scopes(q)
	answers := e.run(ctx, scopes, q.Queries, []string{"observation"}, q.Kinds)
	res := Result{Scopes: map[string]ScopeMeta{}}
	var empty []scope
	for _, s := range scopes {
		if m := meta(answers, s.name); m.OK && m.Raw == 0 {
			empty = append(empty, s)
		}
	}
	if len(empty) > 0 && ctx.Err() == nil {
		raw := e.run(ctx, empty, q.Queries, []string{"world", "experience"}, false)
		for _, a := range raw {
			if len(a.hits) > 0 {
				res.Raw = true
			}
		}
		answers = append(answers, raw...)
	}
	best := map[string]Hit{}
	for _, a := range answers {
		m, seen := res.Scopes[a.scope]
		if !seen {
			m.OK = true
		}
		m.MS = max(m.MS, a.ms)
		if a.err != nil {
			var apiErr *hindsight.APIError
			m.OK, m.Err = false, a.err.Error()
			m.Unauthorized = errors.As(a.err, &apiErr) && apiErr.Kind == hindsight.Unauthorized
		}
		for _, r := range a.hits {
			m.Raw++
			if r.ID == "" {
				continue
			}
			h := e.hit(r, a.scope, q.Current)
			if q.Kinds {
				h.Kinds, h.Subagent = KindsOf(r.Tags, r.SourceTags)
			}
			if q.GlobalMinReranker > 0 && h.Reranker < q.GlobalMinReranker && !e.near(q, h.Project) {
				continue
			}
			if prev, ok := best[h.ID]; h.Reranker >= q.MinReranker && (!ok || h.Score > prev.Score) {
				best[h.ID] = h
			}
		}
		res.Scopes[a.scope] = m
	}
	res.Hits = e.adjust(best, q.MinScore)
	if q.Limit > 0 && len(res.Hits) > q.Limit {
		res.Hits = res.Hits[:q.Limit]
	}
	return res
}

// adjust applies Engine.Adjust to the merged hits, after the floors on
// the server's score: hidden ones are dropped, each boost is added to its
// score, which MinScore then sees. The hits come best first. With an
// Adjust, each carries its RankWithoutBoost: its place in the list
// returned without any boost (MinScore on the unboosted score), 0 for a
// hit the boost lifted over MinScore.
func (e *Engine) adjust(best map[string]Hit, minScore float64) []Hit {
	type scored struct {
		hit  Hit
		base float64 // the score without the boost
	}
	var all []scored
	for _, h := range best {
		base := h.Score
		if e.Adjust != nil {
			b, hidden := e.Adjust(h.ID)
			if hidden {
				continue
			}
			if b = round4(b); b != 0 {
				h.Boost, h.Score = b, round4(h.Score+b)
			}
		}
		all = append(all, scored{h, base})
	}
	byScore := func(score func(scored) float64) func(a, b scored) int {
		return func(a, b scored) int {
			if c := cmp.Compare(score(b), score(a)); c != 0 {
				return c
			}
			return strings.Compare(a.hit.ID, b.hit.ID)
		}
	}
	if e.Adjust != nil {
		slices.SortFunc(all, byScore(func(s scored) float64 { return s.base }))
		n := 0
		for i := range all {
			if all[i].base >= minScore {
				n++
				all[i].hit.RankWithoutBoost = n
			}
		}
	}
	slices.SortFunc(all, byScore(func(s scored) float64 { return s.hit.Score }))
	var hits []Hit
	for _, s := range all {
		if s.hit.Score >= minScore {
			hits = append(hits, s.hit)
		}
	}
	return hits
}

// round4 rounds v to 4 decimals; + 0 turns a negative zero into 0.
func round4(v float64) float64 { return math.Round(v*10000)/10000 + 0 }

type answer struct {
	scope string
	hits  []hindsight.RecallHit // non-empty text only
	err   error
	ms    int
}

// meta sums the answers of one scope: ok unless a call failed, raw =
// results returned.
func meta(answers []answer, name string) ScopeMeta {
	m := ScopeMeta{OK: true}
	for _, a := range answers {
		if a.scope == name {
			m.Raw += len(a.hits)
			if a.err != nil {
				m.OK = false
			}
		}
	}
	return m
}

func (e *Engine) run(ctx context.Context, scopes []scope, queries, types []string, sources bool) []answer {
	type job struct {
		scope scope
		query string
	}
	var jobs []job
	for _, s := range scopes {
		for _, text := range queries {
			if text = strings.TrimSpace(text); text != "" {
				jobs = append(jobs, job{s, text})
			}
		}
	}
	answers := make([]answer, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			req := hindsight.RecallRequest{Query: j.query, Types: types, Budget: "low", MaxTokens: maxTokens, SourceFacts: sources}
			if len(j.scope.tags) > 0 {
				req.Tags, req.TagsMatch = j.scope.tags, "any_strict"
			}
			got, err := e.Client.Recall(ctx, e.Bank, req)
			var hits []hindsight.RecallHit
			for _, h := range got {
				if strings.TrimSpace(h.Text) != "" {
					hits = append(hits, h)
				}
			}
			answers[i] = answer{scope: j.scope.name, hits: hits, err: err, ms: int(time.Since(start).Milliseconds())}
		}()
	}
	wg.Wait()
	return answers
}

// near: the current project, a transverse alias, or linked/asked/related.
func (e *Engine) near(q Query, project string) bool {
	return project == q.Current || contains(e.Transverse, project) || contains(q.Linked, project) || contains(q.Projects, project) || contains(q.Related, project)
}

func (e *Engine) hit(r hindsight.RecallHit, scope, current string) Hit {
	h := Hit{ID: r.ID, Text: strings.TrimSpace(r.Text), Project: ProjectOf(r.Tags), Type: r.Type, Scope: scope, Reranker: r.Scores.Reranker}
	h.Score = h.Reranker
	if h.Project != "" && h.Project == current {
		if e.Boost > 0 {
			h.Score += e.Boost
		} else {
			h.Score += ProjectBoost
		}
	}
	h.Score = float64(int(h.Score*10000+0.5)) / 10000
	if t, err := time.Parse(time.RFC3339Nano, r.MentionedAt); err == nil {
		h.Date = t
		h.Age = Age(e.now().Sub(t))
	}
	return h
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// KindsOf lists the kinds (the kind:<k> tags) found in the tags given, each
// once and sorted, and whether one is tagged source:subagent. A raw fact
// passes its own tags; an observation, its source facts'.
func KindsOf(tagSets ...[]string) (kinds []string, subagent bool) {
	for _, tags := range tagSets {
		for _, t := range tags {
			if k, ok := strings.CutPrefix(t, "kind:"); ok && k != "" && !contains(kinds, k) {
				kinds = append(kinds, k)
			}
			if t == "source:subagent" {
				subagent = true
			}
		}
	}
	sort.Strings(kinds)
	return kinds, subagent
}

// ProjectOf is the project tag of a memory, "" when it has none.
func ProjectOf(tags []string) string {
	for _, t := range tags {
		if p, ok := strings.CutPrefix(t, "project:"); ok {
			return p
		}
	}
	return ""
}

// Age renders a duration as 5m, 3h, 4d, 2w, 5mo or 1y.
func Age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(0, int(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw", int(d.Hours()/24/7))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

func projectTags(projects []string) []string {
	tags := make([]string, len(projects))
	for i, p := range projects {
		tags[i] = "project:" + p
	}
	return tags
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
