package guide

import (
	"context"
	"errors"
	"slices"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

// Bank is the docs bank of the memory bank bank: one document per section,
// rebuilt from the binary at will. Nothing else ever reads it (recall,
// doctor, bank apply, stats and projects read bank alone).
func Bank(bank string) string { return bank + "-docs" }

// docsRetrieval ranks the docs by embeddings and the reranker alone
// (experiment E31): links between chunks of documentation mean nothing,
// dates neither, and the keyword arm, on English text, crowds a question
// in another language out of the reranker's candidates.
var docsRetrieval = map[string]any{"enable_graph_retrieval": false, "enable_temporal_retrieval": false, "enable_text_search": false}

// sendBatch: sections per synchronous retain. In chunks mode a retain only
// embeds, locally.
const sendBatch = 16

// IndexResult says what Index did to the docs bank.
type IndexResult struct {
	Bank      string `json:"bank"`
	DocsHash  string `json:"docs_hash"`
	Sections  int    `json:"sections"`
	Sent      int    `json:"sent"`      // new or edited sections, retained
	Retagged  int    `json:"retagged"`  // unchanged sections moved to this docs version's tag
	Deleted   int    `json:"deleted"`   // documents of sections these docs no longer have
	Unchanged int    `json:"unchanged"` // sections already there, tag included
}

// plan is the work to bring the docs bank to c.
type plan struct {
	send, retag []Section
	remove      []string
	unchanged   int
}

func (p plan) fresh() bool { return len(p.send)+len(p.retag) == 0 }

// diff compares the bank's documents with c: a section whose hash matches
// is not sent again, only retagged when its tag is another version's.
func diff(c Corpus, have []hindsight.DocumentItem) plan {
	byID := map[string]hindsight.DocumentItem{}
	for _, d := range have {
		byID[d.ID] = d
	}
	var p plan
	want := map[string]bool{}
	for _, s := range c.Sections() {
		want[s.ID()] = true
		d, ok := byID[s.ID()]
		switch {
		case !ok || d.Metadata["section_hash"] != s.Hash:
			p.send = append(p.send, s)
		case !slices.Equal(d.Tags, []string{c.Tag()}):
			p.retag = append(p.retag, s)
		default:
			p.unchanged++
		}
	}
	for _, d := range have {
		if !want[d.ID] {
			p.remove = append(p.remove, d.ID)
		}
	}
	return p
}

// Index brings bank's docs bank to c: the bank is set up quiet first (0
// LLM calls: chunks only, no observations, as the control bank), new and
// edited sections are retained, unchanged ones keep their embeddings (a
// tag change at most), and documents of removed sections are deleted.
func Index(ctx context.Context, cl *hindsight.Client, bank string, c Corpus) (IndexResult, error) {
	db := Bank(bank)
	res := IndexResult{Bank: db, DocsHash: c.Hash, Sections: len(c.Sections())}
	if err := retain.QuietBank(ctx, cl, db, "backsight docs", "backsight's documentation, one document per section, for `backsight guide search`; no memory."); err != nil {
		return res, err
	}
	if err := cl.PatchBankConfig(ctx, db, docsRetrieval); err != nil {
		return res, err
	}
	have, err := cl.ListDocuments(ctx, db)
	if err != nil {
		return res, err
	}
	return apply(ctx, cl, c, diff(c, have), res)
}

func apply(ctx context.Context, cl *hindsight.Client, c Corpus, p plan, res IndexResult) (IndexResult, error) {
	db := res.Bank
	res.Unchanged = p.unchanged
	for i := 0; i < len(p.send); i += sendBatch {
		var items []hindsight.RetainItem
		for _, s := range p.send[i:min(i+sendBatch, len(p.send))] {
			items = append(items, hindsight.RetainItem{
				Content:    s.Text,
				DocumentID: s.ID(),
				Tags:       []string{c.Tag()},
				Metadata:   map[string]string{"topic": s.Topic, "slug": s.Slug, "heading": s.Heading, "section_hash": s.Hash, "docs_hash": c.Hash},
			})
		}
		if err := cl.RetainNow(ctx, db, items); err != nil {
			return res, err
		}
		res.Sent += len(items)
	}
	for _, s := range p.retag {
		if err := cl.SetDocumentTags(ctx, db, s.ID(), []string{c.Tag()}); err != nil {
			return res, err
		}
		res.Retagged++
	}
	for _, id := range p.remove {
		if err := cl.DeleteDocument(ctx, db, id); err != nil && !notFound(err) { // gone already: another client's refresh
			return res, err
		}
		res.Deleted++
	}
	return res, nil
}

// SearchError is a semantic search that could not answer, and at which
// stage: "check" (reading the docs bank), "refresh" (bringing it to this
// binary's docs) or "recall".
type SearchError struct {
	Stage string
	Err   error
}

func (e *SearchError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *SearchError) Unwrap() error { return e.Err }

// Search recalls the sections of c matching query from bank's docs bank,
// one hit per section, best first. A bank missing, or on another docs
// version (a restore, another client of a shared server), is brought to c
// first; refreshed says what that did.
func Search(ctx context.Context, cl *hindsight.Client, bank string, c Corpus, query string, limit int) (hits []Hit, refreshed *IndexResult, err error) {
	db := Bank(bank)
	have, err := cl.ListDocuments(ctx, db)
	switch {
	case notFound(err):
		res, err := Index(ctx, cl, bank, c)
		if err != nil {
			return nil, nil, &SearchError{"refresh", err}
		}
		refreshed = &res
	case err != nil:
		return nil, nil, &SearchError{"check", err}
	default:
		if p := diff(c, have); !p.fresh() {
			// The full Index: it sets the bank up quiet again, in case
			// someone changed its config.
			res, err := Index(ctx, cl, bank, c)
			if err != nil {
				return nil, nil, &SearchError{"refresh", err}
			}
			refreshed = &res
		}
	}
	raw, err := cl.Recall(ctx, db, hindsight.RecallRequest{Query: query, Budget: "mid", MaxTokens: 4096, Tags: []string{c.Tag()}, TagsMatch: "any_strict"})
	if err != nil {
		return nil, refreshed, &SearchError{"recall", err}
	}
	byID := map[string]Section{}
	for _, s := range c.Sections() {
		byID[s.ID()] = s
	}
	seen := map[string]bool{}
	for _, h := range raw { // best first; a section's later chunks add nothing
		s, ok := byID[h.DocumentID]
		if !ok || seen[h.DocumentID] {
			continue
		}
		seen[h.DocumentID] = true
		hits = append(hits, Hit{Section: s, Score: score(h), Excerpt: Excerpt(s)})
		if limit > 0 && len(hits) == limit {
			break
		}
	}
	return hits, refreshed, nil
}

func notFound(err error) bool {
	var apiErr *hindsight.APIError
	return errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound
}

// score is the reranker's relevance (0-1), else the final score.
func score(h hindsight.RecallHit) float64 {
	if h.Scores.Reranker > 0 {
		return float64(int(h.Scores.Reranker*1000+0.5)) / 1000
	}
	return float64(int(h.Scores.Final*1000+0.5)) / 1000
}
