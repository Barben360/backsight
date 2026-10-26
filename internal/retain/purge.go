package retain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/state"
)

// BriefingModelPrefix starts every briefing page's id (decision 0060).
const BriefingModelPrefix = "backsight-brief-"

// BriefingModelID is the project's mental-model id: a readable slug and a
// hash, so that two project names never share a page.
func BriefingModelID(project string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(project) {
		if b.Len() >= 40 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	sum := sha256.Sum256([]byte(project))
	return BriefingModelPrefix + b.String() + "-" + hex.EncodeToString(sum[:4])
}

// PurgeFact is one raw fact a purge removes.
type PurgeFact struct {
	ID    string
	State string // "invalidated": already archived, only its document removes it
}

// PurgeWrites are a purge's writes on the memory bank, from forget --purge
// and from a purge done again (PurgeAgain): each live fact has its
// observations cleared while the API still sees it, then is retired; then
// the documents go, and with them every fact, live or archived, extracted
// from them; last, the briefing page of each project (decision 0060),
// which may quote the purged text, and so may its history. rerun says how
// to finish when a document delete fails. It returns the facts whose
// observations were cleared and the pages deleted.
func PurgeWrites(ctx context.Context, cl *hindsight.Client, bank string, facts []PurgeFact, docs []string, projects map[string]bool, rerun string) (cleared int, pages []string, err error) {
	for _, f := range facts {
		if f.State == "invalidated" {
			continue
		}
		if err := cl.ClearObservations(ctx, bank, f.ID); err != nil {
			return cleared, nil, fmt.Errorf("clearing the observations of %s: %w", f.ID, err)
		}
		cleared++
		if err := cl.Invalidate(ctx, bank, f.ID, "purged by the user (backsight forget --purge)", false); err != nil {
			return cleared, nil, fmt.Errorf("retiring %s: %w", f.ID, err)
		}
	}
	for i, d := range docs {
		if err := cl.DeleteDocument(ctx, bank, d); err != nil {
			// The observation may be gone already (its facts were cleared):
			// say what is left and how to finish.
			left := slicesClone(docs[i:])
			if len(left) > 10 {
				left = append(left[:10], fmt.Sprintf("and %d more", len(left)-10))
			}
			return cleared, nil, fmt.Errorf("deleting document %s: %w; documents left: %s (%s)", d, err, strings.Join(left, ", "), rerun)
		}
	}
	for p := range projects {
		id := BriefingModelID(p)
		var apiErr *hindsight.APIError
		switch err := cl.DeleteMentalModel(ctx, bank, id); {
		case err == nil:
			pages = append(pages, id)
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
		default:
			return cleared, pages, fmt.Errorf("deleting briefing page %s: %w", id, err)
		}
	}
	sort.Strings(pages)
	return cleared, pages, nil
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// Repurged is what PurgeAgain removed.
type Repurged struct {
	Documents    []string `json:"documents,omitempty"`
	Observations int      `json:"observations_cleared,omitempty"` // facts whose observations were cleared
	Pages        []string `json:"pages,omitempty"`
}

// Empty: nothing was purged again.
func (r Repurged) Empty() bool { return len(r.Documents) == 0 }

// String names what went, for a step's detail.
func (r Repurged) String() string {
	s := fmt.Sprintf("%d document(s): %s; observations of %d fact(s) cleared", len(r.Documents), strings.Join(r.Documents, ", "), r.Observations)
	if len(r.Pages) > 0 {
		s += "; briefing page(s) " + strings.Join(r.Pages, ", ")
	}
	return s
}

// PurgeAgain purges again, as forget --purge did (PurgeWrites), each
// document of bank a tombstone names that the server holds: a restore
// brought it back, or a queued retain recreated it (Hindsight 1.1.0
// recreates a deleted document on append). Its facts and their projects
// are read first; the tombstone's project is added (a page may quote a
// document whose facts are not extracted yet).
func PurgeAgain(ctx context.Context, cl *hindsight.Client, bank string, tombs []state.Tombstone) (Repurged, error) {
	var res Repurged
	seen := map[string]bool{}
	for _, t := range tombs {
		if seen[t.Document] || (t.Bank != "" && t.Bank != bank) {
			continue
		}
		seen[t.Document] = true
		_, err := cl.GetDocument(ctx, bank, t.Document)
		var apiErr *hindsight.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound:
			continue
		case err != nil:
			return res, fmt.Errorf("bank %s, document %s: %w", bank, t.Document, err)
		}
		var facts []PurgeFact
		projects := map[string]bool{}
		if t.Project != "" {
			projects[t.Project] = true
		}
		if err := cl.ListMemories(ctx, bank, hindsight.MemoryQuery{DocumentID: t.Document}, func(m hindsight.Memory) error {
			facts = append(facts, PurgeFact{ID: m.ID, State: m.State})
			for _, tag := range m.Tags {
				if p, ok := strings.CutPrefix(tag, "project:"); ok {
					projects[p] = true
				}
			}
			return nil
		}); err != nil {
			return res, fmt.Errorf("bank %s, the facts of document %s: %w", bank, t.Document, err)
		}
		cleared, pages, err := PurgeWrites(ctx, cl, bank, facts, []string{t.Document}, projects, "it is purged again on the next restore or reconcile")
		res.Observations += cleared
		res.Pages = append(res.Pages, pages...)
		if err != nil {
			return res, fmt.Errorf("bank %s: %w", bank, err)
		}
		res.Documents = append(res.Documents, t.Document)
	}
	sort.Strings(res.Pages)
	res.Pages = compactStrings(res.Pages)
	return res, nil
}

func compactStrings(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
