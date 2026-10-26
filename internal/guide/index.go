package guide

import (
	"context"
	"errors"
	"slices"
	"strings"

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

// Marker is how backsight knows the docs bank as its own: a bank of that
// name holding only `docs:` documents. Any other `<bank>-docs` is left as
// it is (decision 0067, block 4 review).
var Marker = retain.Marker{Name: "backsight docs", IDPrefix: "docs:"}

const mission = "backsight's documentation, one document per section, for `backsight guide search`; no memory."

// versionPrefix starts every docs version tag (Corpus.Tag).
const versionPrefix = "docs-version:"

// IndexResult says what Index did to the docs bank.
type IndexResult struct {
	Bank      string `json:"bank"`
	DocsHash  string `json:"docs_hash"`
	Sections  int    `json:"sections"`
	Sent      int    `json:"sent"`      // new or edited sections, retained
	Retagged  int    `json:"retagged"`  // unchanged sections given this docs version's tag too
	Untagged  int    `json:"untagged"`  // documents of sections these docs lack, kept for another version, this one's tag removed
	Deleted   int    `json:"deleted"`   // documents no docs version holds any more
	Unchanged int    `json:"unchanged"` // sections already there, tag included
}

// IndexPlan is what Index would do, by document id (guide index --plan).
type IndexPlan struct {
	Bank      string   `json:"bank"`
	DocsHash  string   `json:"docs_hash"`
	Sections  int      `json:"sections"`
	Exists    bool     `json:"exists"`    // false: the bank would be created
	Create    []string `json:"create"`    // new sections, sent
	Resend    []string `json:"resend"`    // edited sections, sent again
	Retag     []string `json:"retag"`     // unchanged sections given this version's tag
	Untag     []string `json:"untag"`     // documents losing this version's tag, kept for another
	Delete    []string `json:"delete"`    // documents no version holds any more
	Unchanged int      `json:"unchanged"` // sections left as they are
}

// retag is a document's new tag list.
type retag struct {
	id   string
	tags []string
}

// plan is the work to bring the docs bank to c.
type plan struct {
	send         []Section       // new and edited sections, in the docs' order
	isNew        map[string]bool // the sections of send the bank lacks
	retag, untag []retag
	remove       []string
	unchanged    int
}

func (p plan) fresh() bool { return len(p.send)+len(p.retag) == 0 }

// diff compares the bank's documents with c. Several docs versions can
// share the bank (clients of one server, a restore): each document carries
// the tags of every version holding it. A section whose hash matches is not
// sent again, only given c's tag; a document of a section c lacks loses
// c's tag and is deleted only when no version tag remains. Documents whose
// id does not start with `docs:` are never touched.
func diff(c Corpus, have []hindsight.DocumentItem) plan {
	byID := map[string]hindsight.DocumentItem{}
	for _, d := range have {
		byID[d.ID] = d
	}
	own := c.Tag()
	p := plan{isNew: map[string]bool{}}
	want := map[string]bool{}
	for _, s := range c.Sections() {
		want[s.ID()] = true
		d, ok := byID[s.ID()]
		switch {
		case !ok || d.Metadata["section_hash"] != s.Hash: // this version's text, its tag alone
			p.send = append(p.send, s)
			p.isNew[s.ID()] = !ok
		case !slices.Contains(d.Tags, own):
			p.retag = append(p.retag, retag{s.ID(), append(slices.Clone(d.Tags), own)})
		default:
			p.unchanged++
		}
	}
	for _, d := range have {
		if want[d.ID] || !strings.HasPrefix(d.ID, Marker.IDPrefix) {
			continue
		}
		rest := slices.DeleteFunc(slices.Clone(d.Tags), func(t string) bool { return t == own })
		switch {
		case !slices.ContainsFunc(rest, func(t string) bool { return strings.HasPrefix(t, versionPrefix) }):
			p.remove = append(p.remove, d.ID)
		case len(rest) < len(d.Tags):
			p.untag = append(p.untag, retag{d.ID, rest})
		}
	}
	return p
}

// Index brings bank's docs bank to c: the bank is set up quiet first (0
// LLM calls: chunks only, no observations, as the control bank), new and
// edited sections are retained, unchanged ones keep their embeddings (a
// tag more at most), and documents no docs version holds are deleted. A
// docs bank that is not backsight's is left as it is: *retain.ForeignBank.
func Index(ctx context.Context, cl *hindsight.Client, bank string, c Corpus) (IndexResult, error) {
	db := Bank(bank)
	res := IndexResult{Bank: db, DocsHash: c.Hash, Sections: len(c.Sections())}
	have, err := retain.QuietBank(ctx, cl, db, Marker, mission)
	if err != nil {
		return res, err
	}
	if err := cl.PatchBankConfig(ctx, db, docsRetrieval); err != nil {
		return res, err
	}
	return apply(ctx, cl, c, diff(c, have), res)
}

// PlanIndex says what Index would do, reading only; it refuses a docs bank
// that is not backsight's as Index does.
func PlanIndex(ctx context.Context, cl *hindsight.Client, bank string, c Corpus) (IndexPlan, error) {
	db := Bank(bank)
	pl := IndexPlan{Bank: db, DocsHash: c.Hash, Sections: len(c.Sections()), Create: []string{}, Resend: []string{}, Retag: []string{}, Untag: []string{}, Delete: []string{}}
	have, exists, err := retain.OwnedBank(ctx, cl, db, Marker)
	if err != nil {
		return pl, err
	}
	p := diff(c, have)
	pl.Exists, pl.Unchanged, pl.Delete = exists, p.unchanged, append(pl.Delete, p.remove...)
	for _, s := range p.send {
		if p.isNew[s.ID()] {
			pl.Create = append(pl.Create, s.ID())
		} else {
			pl.Resend = append(pl.Resend, s.ID())
		}
	}
	for _, r := range p.retag {
		pl.Retag = append(pl.Retag, r.id)
	}
	for _, r := range p.untag {
		pl.Untag = append(pl.Untag, r.id)
	}
	return pl, nil
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
	for _, r := range p.retag {
		if err := cl.SetDocumentTags(ctx, db, r.id, r.tags); err != nil {
			return res, err
		}
		res.Retagged++
	}
	for _, r := range p.untag {
		if err := cl.SetDocumentTags(ctx, db, r.id, r.tags); err != nil && !notFound(err) {
			return res, err
		}
		res.Untagged++
	}
	for _, id := range p.remove {
		if !strings.HasPrefix(id, Marker.IDPrefix) { // diff never lists one; the one rule that must hold
			continue
		}
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
	have, exists, err := retain.OwnedBank(ctx, cl, db, Marker)
	if err != nil {
		return nil, nil, &SearchError{"check", err}
	}
	if !exists || !diff(c, have).fresh() {
		// The full Index: it sets the bank up quiet again, in case
		// someone changed its config.
		res, err := Index(ctx, cl, bank, c)
		if err != nil {
			return nil, nil, &SearchError{"refresh", err}
		}
		refreshed = &res
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
