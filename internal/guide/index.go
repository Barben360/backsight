package guide

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

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

// keepVersions docs versions at most share the bank: the running one and
// the others first indexed last. An older one's tag goes, and a section
// no kept version holds is deleted (issue 279).
const keepVersions = 5

// VersionsID is the docs bank's record of when each docs version was
// first indexed: one metadata entry `docs-version-at:<hash16>` = Unix
// seconds per kept version, no tag (a search, filtered on its version's
// tag, never finds it). It orders the versions for keepVersions. Its id
// starts with `docs:` (the bank's marker) and no docs file may take it.
const VersionsID = "docs:_versions"

// versionAtPrefix starts each entry of VersionsID's metadata.
const versionAtPrefix = "docs-version-at:"

// now is the clock of the first-index times; tests set it.
var now = time.Now

// IndexResult says what Index did to the docs bank.
type IndexResult struct {
	Bank      string `json:"bank"`
	DocsHash  string `json:"docs_hash"`
	Sections  int    `json:"sections"`
	Sent      int    `json:"sent"`      // new or edited sections, retained
	Retagged  int    `json:"retagged"`  // unchanged sections given this docs version's tag too
	Untagged  int    `json:"untagged"`  // documents of sections these docs lack, kept for another version, this one's tag removed
	Pruned    int    `json:"pruned"`    // documents kept for another version, only dropped versions' tags removed
	Deleted   int    `json:"deleted"`   // documents no kept docs version holds any more
	Unchanged int    `json:"unchanged"` // sections already there, tag included
	// DroppedVersions are the docs versions beyond the last keepVersions,
	// oldest first, whose tags were removed.
	DroppedVersions []string `json:"dropped_versions"`
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
	Prune     []string `json:"prune"`     // documents losing only dropped versions' tags, kept for another
	Delete    []string `json:"delete"`    // documents no kept version holds any more
	Unchanged int      `json:"unchanged"` // sections left as they are
	// DropVersions are the docs versions beyond the last keepVersions,
	// oldest first, whose tags would be removed.
	DropVersions []string `json:"drop_versions"`
	// RecordVersions: VersionsID would be written (this version's first
	// index time added, or dropped versions' removed).
	RecordVersions bool `json:"record_versions"`
}

// retag is a document's new tag list.
type retag struct {
	id   string
	tags []string
}

// plan is the work to bring the docs bank to c.
type plan struct {
	send                []Section       // new and edited sections, in the docs' order
	isNew               map[string]bool // the sections of send the bank lacks
	retag, untag, prune []retag
	remove              []string
	unchanged           int
	dropped             []string          // docs versions dropped, oldest first
	versions            map[string]string // VersionsID's new metadata; nil: left as it is
	recorded            bool              // c's first index time is in VersionsID already
}

// fresh: c finds every section of its own, and its first index time is
// recorded. Dropping older versions waits for the next index.
func (p plan) fresh() bool { return len(p.send)+len(p.retag) == 0 && p.recorded }

// versionsKept reads VersionsID among have and ranks the docs versions the
// bank's `docs:` documents carry, with c's: c's and the keepVersions-1
// others first indexed last are kept, ties (and versions with no record,
// the oldest) by tag. It returns the kept set, the dropped versions
// (oldest first), VersionsID's metadata as it should be, and whether c's
// time was recorded already.
func versionsKept(c Corpus, have []hindsight.DocumentItem, at time.Time) (keep map[string]bool, dropped []string, record map[string]string, recorded bool, current map[string]string) {
	own := c.Tag()
	firstAt := map[string]int64{}
	present := map[string]bool{own: true}
	current = map[string]string{}
	for _, d := range have {
		switch {
		case d.ID == VersionsID:
			for k, v := range d.Metadata {
				if h, ok := strings.CutPrefix(k, versionAtPrefix); ok {
					current[k] = v
					if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
						firstAt[versionPrefix+h] = n
					}
				}
			}
		case strings.HasPrefix(d.ID, Marker.IDPrefix):
			for _, t := range d.Tags {
				if strings.HasPrefix(t, versionPrefix) {
					present[t] = true
				}
			}
		}
	}
	recorded = firstAt[own] > 0
	if !recorded {
		firstAt[own] = at.Unix()
	}
	var others []string
	for t := range present {
		if t != own {
			others = append(others, t)
		}
	}
	slices.SortFunc(others, func(a, b string) int { // newest first
		return cmp.Or(cmp.Compare(firstAt[b], firstAt[a]), strings.Compare(b, a))
	})
	keep = map[string]bool{own: true}
	for i, t := range others {
		if i < keepVersions-1 {
			keep[t] = true
		} else {
			dropped = append(dropped, t)
		}
	}
	slices.Reverse(dropped)
	record = map[string]string{}
	for t := range keep {
		if firstAt[t] > 0 {
			record[versionAtPrefix+strings.TrimPrefix(t, versionPrefix)] = strconv.FormatInt(firstAt[t], 10)
		}
	}
	return keep, dropped, record, recorded, current
}

// diff compares the bank's documents with c, at the time at. Several docs
// versions can share the bank (clients of one server, a restore): each
// document carries the tags of every version holding it, and the last
// keepVersions of them are kept (versionsKept). A section whose hash
// matches is not sent again, only given c's tag; a document of a section
// c lacks loses c's tag; a dropped version's tag goes from every document;
// a document is deleted only when no kept version tag remains. Documents
// whose id does not start with `docs:`, and VersionsID, are never
// retagged or deleted.
func diff(c Corpus, have []hindsight.DocumentItem, at time.Time) plan {
	byID := map[string]hindsight.DocumentItem{}
	for _, d := range have {
		byID[d.ID] = d
	}
	own := c.Tag()
	keep, dropped, record, recorded, current := versionsKept(c, have, at)
	p := plan{isNew: map[string]bool{}, dropped: dropped, recorded: recorded}
	if !maps.Equal(record, current) {
		p.versions = record
	}
	kept := func(tags []string) []string {
		return slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return strings.HasPrefix(t, versionPrefix) && !keep[t] })
	}
	want := map[string]bool{}
	for _, s := range c.Sections() {
		want[s.ID()] = true
		d, ok := byID[s.ID()]
		tags := kept(d.Tags)
		switch {
		case !ok || d.Metadata["section_hash"] != s.Hash: // this version's text, its tag alone
			p.send = append(p.send, s)
			p.isNew[s.ID()] = !ok
		case !slices.Contains(tags, own):
			p.retag = append(p.retag, retag{s.ID(), append(tags, own)})
		case len(tags) < len(d.Tags):
			p.prune = append(p.prune, retag{s.ID(), tags})
		default:
			p.unchanged++
		}
	}
	for _, d := range have {
		if want[d.ID] || d.ID == VersionsID || !strings.HasPrefix(d.ID, Marker.IDPrefix) {
			continue
		}
		rest := slices.DeleteFunc(kept(d.Tags), func(t string) bool { return t == own })
		switch {
		case !slices.ContainsFunc(rest, func(t string) bool { return strings.HasPrefix(t, versionPrefix) }):
			p.remove = append(p.remove, d.ID)
		case slices.Contains(d.Tags, own):
			p.untag = append(p.untag, retag{d.ID, rest})
		case len(rest) < len(d.Tags):
			p.prune = append(p.prune, retag{d.ID, rest})
		}
	}
	return p
}

// Index brings bank's docs bank to c: the bank is set up quiet first (0
// LLM calls: chunks only, no observations, as the control bank), c's
// first index time is recorded, new and edited sections are retained,
// unchanged ones keep their embeddings (a tag more at most), versions
// older than the last keepVersions lose their tags, and documents no kept
// docs version holds are deleted. A
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
	return apply(ctx, cl, c, diff(c, have, now()), res)
}

// PlanIndex says what Index would do, reading only; it refuses a docs bank
// that is not backsight's as Index does.
func PlanIndex(ctx context.Context, cl *hindsight.Client, bank string, c Corpus) (IndexPlan, error) {
	db := Bank(bank)
	pl := IndexPlan{Bank: db, DocsHash: c.Hash, Sections: len(c.Sections()), Create: []string{}, Resend: []string{}, Retag: []string{}, Untag: []string{},
		Prune: []string{}, Delete: []string{}, DropVersions: []string{}}
	have, exists, err := retain.OwnedBank(ctx, cl, db, Marker)
	if err != nil {
		return pl, err
	}
	p := diff(c, have, now())
	pl.Exists, pl.Unchanged, pl.Delete = exists, p.unchanged, append(pl.Delete, p.remove...)
	pl.DropVersions, pl.RecordVersions = append(pl.DropVersions, p.dropped...), p.versions != nil
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
	for _, r := range p.prune {
		pl.Prune = append(pl.Prune, r.id)
	}
	return pl, nil
}

func apply(ctx context.Context, cl *hindsight.Client, c Corpus, p plan, res IndexResult) (IndexResult, error) {
	db := res.Bank
	res.Unchanged, res.DroppedVersions = p.unchanged, append([]string{}, p.dropped...)
	// The record first: an index cut short still dates this version from
	// its first try.
	if p.versions != nil {
		if err := cl.RetainNow(ctx, db, []hindsight.RetainItem{versionsItem(p.versions)}); err != nil {
			return res, err
		}
	}
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
	for _, r := range p.prune {
		if err := cl.SetDocumentTags(ctx, db, r.id, r.tags); err != nil && !notFound(err) {
			return res, err
		}
		res.Pruned++
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

// versionsItem is VersionsID holding record: no tag, the entries in its
// metadata and, as text, the same list (chunks mode embeds it, no LLM).
func versionsItem(record map[string]string) hindsight.RetainItem {
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(record)) {
		lines = append(lines, fmt.Sprintf("%s%s = %s", versionPrefix, strings.TrimPrefix(k, versionAtPrefix), record[k]))
	}
	return hindsight.RetainItem{
		Content:    "backsight docs versions kept in this bank, with the Unix time each was first indexed:\n" + strings.Join(lines, "\n") + "\n",
		DocumentID: VersionsID,
		Tags:       []string{},
		Metadata:   record,
	}
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

// ErrNotIndexed: the docs bank is missing or holds none of c's sections,
// and Search was not to refresh it.
var ErrNotIndexed = errors.New("docs bank not indexed for this docs version")

// Search recalls the sections of c matching query from bank's docs bank,
// one hit per section, best first. With refresh, a bank missing, or on
// another docs version (a restore), is brought to c first; refreshed says
// what that did. Without (a client of a shared server: the bank is its
// host's), Search only reads, and a bank missing or without c's tag is
// ErrNotIndexed.
func Search(ctx context.Context, cl *hindsight.Client, bank string, c Corpus, query string, limit int, refresh bool) (hits []Hit, refreshed *IndexResult, err error) {
	db := Bank(bank)
	have, exists, err := retain.OwnedBank(ctx, cl, db, Marker)
	if err != nil {
		return nil, nil, &SearchError{"check", err}
	}
	if !refresh {
		if !exists || !tagged(have, c.Tag()) {
			return nil, nil, ErrNotIndexed
		}
	} else if !exists || !diff(c, have, now()).fresh() {
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

// tagged: one of have's sections carries tag.
func tagged(have []hindsight.DocumentItem, tag string) bool {
	return slices.ContainsFunc(have, func(d hindsight.DocumentItem) bool {
		return strings.HasPrefix(d.ID, Marker.IDPrefix) && slices.Contains(d.Tags, tag)
	})
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
