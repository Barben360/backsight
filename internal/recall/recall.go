// Package recall searches the memory across scopes (SPEC §4 Read scopes,
// §10.2): the current project with the transverse aliases, the whole bank
// and related projects, queried as separate calls so project memories are
// not crowded out, then merged by best score with a boost for the current
// project.
package recall

import (
	"context"
	"fmt"
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
	maxTokens  = 1500 // per call, as the reference hook (C12)
)

// Engine runs searches against one bank.
type Engine struct {
	Client     *hindsight.Client
	Bank       string
	Transverse []string      // aliases; the first is canonical
	Timeout    time.Duration // per call
	Now        func() time.Time
}

// Query is one search.
type Query struct {
	Queries  []string
	Current  string   // the current project
	Projects []string // related projects (asked for, or matched in the entity index)
	All      bool     // the whole bank only, no project scopes
	Limit    int      // 0: no limit
	MinScore float64  // on the boosted score
}

// Hit is one merged result.
type Hit struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Project  string    `json:"project"`
	Type     string    `json:"type"`
	Scope    string    `json:"scope"`
	Score    float64   `json:"score"`    // reranker + current-project boost
	Reranker float64   `json:"reranker"` // the server's score
	Date     time.Time `json:"date"`
	Age      string    `json:"age"`
}

// ScopeMeta is how one scope's calls went (telemetry, A.4).
type ScopeMeta struct {
	OK  bool   `json:"ok"`
	MS  int    `json:"ms"`
	Raw int    `json:"raw"` // results before the score floor
	Err string `json:"err,omitempty"`
}

// Result of a search. Raw: no observation existed in any scope, so raw
// facts stand in (a bank not consolidated yet, SPEC §4 Reads).
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
	for _, t := range e.Transverse {
		if t != q.Current {
			own = append(own, t)
		}
	}
	out := []scope{{name: "project", tags: projectTags(own)}, {name: "global"}}
	var related []string
	for _, p := range q.Projects {
		if p != "" && !contains(own, p) && !contains(related, p) && len(related) < MaxRelated {
			related = append(related, p)
		}
	}
	if len(related) > 0 {
		out = append(out, scope{name: "related", tags: projectTags(related)})
	}
	return out
}

// Search runs every (scope, query) pair in parallel and merges the hits.
func (e *Engine) Search(ctx context.Context, q Query) Result {
	res := e.search(ctx, q, []string{"observation"})
	if !res.Raw { // Raw marks here that no observation came back at all
		return res
	}
	for _, m := range res.Scopes {
		if !m.OK {
			res.Raw = false
			return res // a failure, not an empty bank
		}
	}
	raw := e.search(ctx, q, []string{"world", "experience"})
	raw.Raw = true
	return raw
}

func (e *Engine) search(ctx context.Context, q Query, types []string) Result {
	type job struct {
		scope scope
		query string
	}
	var jobs []job
	for _, s := range e.scopes(q) {
		for _, text := range q.Queries {
			if text = strings.TrimSpace(text); text != "" {
				jobs = append(jobs, job{s, text})
			}
		}
	}
	type answer struct {
		hits []hindsight.RecallHit
		err  error
		ms   int
	}
	answers := make([]answer, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, e.Timeout)
			defer cancel()
			start := time.Now()
			req := hindsight.RecallRequest{Query: j.query, Types: types, Budget: "low", MaxTokens: maxTokens}
			if len(j.scope.tags) > 0 {
				req.Tags, req.TagsMatch = j.scope.tags, "any_strict"
			}
			h, err := e.Client.Recall(cctx, e.Bank, req)
			answers[i] = answer{h, err, int(time.Since(start).Milliseconds())}
		}()
	}
	wg.Wait()

	res := Result{Scopes: map[string]ScopeMeta{}, Raw: true}
	best := map[string]Hit{}
	for i, a := range answers {
		name := jobs[i].scope.name
		m, seen := res.Scopes[name]
		if !seen {
			m.OK = true
		}
		m.MS = max(m.MS, a.ms)
		if a.err != nil {
			m.OK, m.Err = false, a.err.Error()
			res.Scopes[name] = m
			continue
		}
		for _, r := range a.hits {
			if strings.TrimSpace(r.Text) == "" {
				continue
			}
			m.Raw++
			res.Raw = false
			h := e.hit(r, name, q.Current)
			if h.Score >= q.MinScore && h.Score > best[h.ID].Score {
				best[h.ID] = h
			}
		}
		res.Scopes[name] = m
	}
	for _, h := range best {
		res.Hits = append(res.Hits, h)
	}
	sort.Slice(res.Hits, func(i, j int) bool {
		if res.Hits[i].Score != res.Hits[j].Score {
			return res.Hits[i].Score > res.Hits[j].Score
		}
		return res.Hits[i].ID < res.Hits[j].ID
	})
	if q.Limit > 0 && len(res.Hits) > q.Limit {
		res.Hits = res.Hits[:q.Limit]
	}
	return res
}

func (e *Engine) hit(r hindsight.RecallHit, scope, current string) Hit {
	h := Hit{ID: r.ID, Text: strings.TrimSpace(r.Text), Project: ProjectOf(r.Tags), Type: r.Type, Scope: scope, Reranker: r.Scores.Reranker}
	h.Score = h.Reranker
	if h.Project != "" && h.Project == current {
		h.Score += ProjectBoost
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
