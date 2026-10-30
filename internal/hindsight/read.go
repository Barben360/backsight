package hindsight

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RecallRequest is POST /memories/recall. Tags empty: the whole bank.
type RecallRequest struct {
	Query     string   `json:"query"`
	Types     []string `json:"types,omitempty"`
	Budget    string   `json:"budget,omitempty"` // low | mid | high
	MaxTokens int      `json:"max_tokens,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"` // any_strict: tagged with one of Tags
	// SourceFacts asks the same call for the facts each observation was
	// consolidated from: Recall then fills RecallHit.SourceTags. The
	// response grows with the facts' text, so only searches shown to a
	// reader ask for it.
	SourceFacts bool `json:"-"`
}

// MarshalJSON adds include: entities null leaves out the entity payload
// backsight never reads (−40 % of the response, same ranking: E30 V4).
func (r RecallRequest) MarshalJSON() ([]byte, error) {
	type plain RecallRequest
	include := map[string]any{"entities": nil}
	if r.SourceFacts {
		include["source_facts"] = map[string]any{"max_tokens": SourceFactsTokens}
	}
	return json.Marshal(struct {
		plain
		Include map[string]any `json:"include"`
	}{plain(r), include})
}

// SourceFactsTokens caps the source facts of one recall response (the
// server's default is 4096). A fact past the cap comes without tags, so its
// observation shows no kinds.
const SourceFactsTokens = 8192

// RecallHit is one recalled memory.
type RecallHit struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Type        string   `json:"type"`
	Tags        []string `json:"tags"`
	MentionedAt string   `json:"mentioned_at"`
	DocumentID  string   `json:"document_id"`
	Scores      struct {
		Reranker float64 `json:"reranker"`
		Final    float64 `json:"final"`
	} `json:"scores"`
	SourceFactIDs []string `json:"source_fact_ids"`
	// SourceTags: the tags of the source facts the response carried
	// (RecallRequest.SourceFacts), each once, in order of appearance.
	SourceTags []string `json:"-"`
}

// Recall returns the memories matching req, best first.
func (c *Client) Recall(ctx context.Context, bank string, req RecallRequest) ([]RecallHit, error) {
	var out struct {
		Results     []RecallHit `json:"results"`
		SourceFacts map[string]struct {
			Tags []string `json:"tags"`
		} `json:"source_facts"`
	}
	err := c.do(ctx, http.MethodPost, bankPath(bank, "/memories/recall"), nil, req, &out)
	for i, h := range out.Results {
		seen := map[string]bool{}
		for _, id := range h.SourceFactIDs {
			for _, t := range out.SourceFacts[id].Tags {
				if !seen[t] {
					seen[t] = true
					out.Results[i].SourceTags = append(out.Results[i].SourceTags, t)
				}
			}
		}
	}
	return out.Results, err
}

// MemoryDetail is one memory with the raw facts it was consolidated from.
type MemoryDetail struct {
	ID            string   `json:"id"`
	Text          string   `json:"text"`
	Type          string   `json:"type"`
	Tags          []string `json:"tags"`
	MentionedAt   string   `json:"mentioned_at"`
	OccurredStart string   `json:"occurred_start"`
	OccurredEnd   string   `json:"occurred_end"`
	DocumentID    string   `json:"document_id"`
	State         string   `json:"state"`
	Sources       []struct {
		ID         string   `json:"id"`
		Text       string   `json:"text"`
		Type       string   `json:"type"`
		DocumentID string   `json:"document_id"`
		Tags       []string `json:"tags"`
	} `json:"source_memories"`
}

// GetMemory reads one memory; an *APIError of kind NotFound when absent.
func (c *Client) GetMemory(ctx context.Context, bank, id string) (MemoryDetail, error) {
	var m MemoryDetail
	return m, c.get(ctx, bankPath(bank, "/memories/"+url.PathEscape(id)), nil, &m)
}

// HistoryEntry is one change the server made to an observation.
type HistoryEntry struct {
	ChangedAt time.Time
}

// History lists an observation's changes, oldest first (none for a raw
// fact); an *APIError of kind NotFound when the memory is gone.
func (c *Client) History(ctx context.Context, bank, id string) ([]HistoryEntry, error) {
	var raw []struct {
		ChangedAt string `json:"changed_at"`
	}
	if err := c.get(ctx, bankPath(bank, "/memories/"+url.PathEscape(id)+"/history"), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]HistoryEntry, 0, len(raw))
	for _, h := range raw {
		out = append(out, HistoryEntry{ChangedAt: serverTime(h.ChangedAt)})
	}
	return out, nil
}

// TagCount is one tag and how many memories carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Tags lists the bank's tags starting with prefix q, every page. The
// server's q is a wildcard pattern (an exact match without "*").
func (c *Client) Tags(ctx context.Context, bank, q string) ([]TagCount, error) {
	var all []TagCount
	offset := 0 // counts every item the server returned, matching or not
	for page := 0; page < maxPages; page++ {
		var out struct {
			Items []TagCount `json:"items"`
			Total int        `json:"total"`
		}
		query := url.Values{"q": {q + "*"}, "limit": {"200"}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, bankPath(bank, "/tags"), query, &out); err != nil {
			return all, err
		}
		offset += len(out.Items)
		for _, t := range out.Items {
			if strings.HasPrefix(t.Tag, q) {
				all = append(all, t)
			}
		}
		if len(out.Items) == 0 || offset >= out.Total {
			break
		}
	}
	return all, nil
}

// Entity is a named entity and its mention count.
type Entity struct {
	Name     string `json:"canonical_name"`
	Mentions int    `json:"mention_count"`
}

// Entities lists up to limit entities of the memories tagged tag.
func (c *Client) Entities(ctx context.Context, bank, tag string, limit int) ([]Entity, error) {
	var out struct {
		Items []Entity `json:"items"`
	}
	query := url.Values{"tags": {tag}, "tags_match": {"any_strict"}, "limit": {strconv.Itoa(limit)}}
	return out.Items, c.get(ctx, bankPath(bank, "/entities"), query, &out)
}

// Recent lists up to limit memories tagged tag, latest first by event time
// (mentioned_at): after a backfill, updated_at is when the server
// consolidated, all on one day. Memories with no event time are listed by
// updated_at when none has one. factType "" lists every type; tag "" every
// memory of the bank, whatever its tags (an empty tag filter would match
// none: ruling R14).
func (c *Client) Recent(ctx context.Context, bank, tag, factType string, limit int) ([]Memory, error) {
	list := func(field string) ([]Memory, error) {
		var out struct {
			Items []Memory `json:"items"`
		}
		q := url.Values{"time_field": {field}, "limit": {strconv.Itoa(limit)}}
		if tag != "" {
			q.Set("tags", tag)
			q.Set("tags_match", "any_strict")
		}
		if factType != "" {
			q.Set("type", factType)
		}
		return out.Items, c.get(ctx, bankPath(bank, "/memories/list"), q, &out)
	}
	items, err := list("mentioned_at")
	if err != nil || len(items) > 0 {
		return items, err
	}
	return list("updated_at")
}

// Invalidate soft-retires (or with valid=true restores) a memory: excluded
// from recall and consolidation, reversible (SPEC §4 Corrections).
func (c *Client) Invalidate(ctx context.Context, bank, id, reason string, valid bool) error {
	body := map[string]string{"state": "invalidated", "reason": reason}
	if valid {
		body = map[string]string{"state": "valid", "reason": reason}
	}
	return c.do(ctx, http.MethodPatch, bankPath(bank, "/memories/"+url.PathEscape(id)), nil, body, nil)
}

// ClearObservations deletes the observations derived from a memory.
func (c *Client) ClearObservations(ctx context.Context, bank, id string) error {
	return c.do(ctx, http.MethodDelete, bankPath(bank, "/memories/"+url.PathEscape(id)+"/observations"), nil, nil, nil)
}

// DeleteDocument deletes a document and the memories extracted from it.
func (c *Client) DeleteDocument(ctx context.Context, bank, id string) error {
	return c.do(ctx, http.MethodDelete, bankPath(bank, "/documents/"+strings.ReplaceAll(url.PathEscape(id), ":", "%3A")), nil, nil, nil)
}

// MentalModel is a page the server writes and refreshes from the memories
// in its tags' scope.
type MentalModel struct {
	ID              string   `json:"id"`
	Content         string   `json:"content"`
	Tags            []string `json:"tags"`
	LastRefreshedAt string   `json:"last_refreshed_at"`
}

// GetMentalModel reads one mental model (cached content, no LLM); an
// *APIError of kind NotFound when absent.
func (c *Client) GetMentalModel(ctx context.Context, bank, id string) (MentalModel, error) {
	var m MentalModel
	return m, c.get(ctx, bankPath(bank, "/mental-models/"+url.PathEscape(id)), nil, &m)
}

// CreateMentalModel creates a mental model; the server refreshes it in the
// background, then again after each consolidation that touches its tags.
func (c *Client) CreateMentalModel(ctx context.Context, bank, id, name, query string, tags []string, maxTokens int) error {
	body := map[string]any{"id": id, "name": name, "source_query": query, "tags": tags, "max_tokens": maxTokens,
		"trigger": map[string]any{"budget": "low", "mode": "full", "refresh_after_consolidation": true}}
	return c.do(ctx, http.MethodPost, bankPath(bank, "/mental-models"), nil, body, nil)
}

// MentalModels lists the bank's mental models.
func (c *Client) MentalModels(ctx context.Context, bank string) ([]MentalModel, error) {
	var out struct {
		Items []MentalModel `json:"items"`
	}
	return out.Items, c.get(ctx, bankPath(bank, "/mental-models"), nil, &out)
}

// DeleteMentalModel removes a mental model.
func (c *Client) DeleteMentalModel(ctx context.Context, bank, id string) error {
	return c.do(ctx, http.MethodDelete, bankPath(bank, "/mental-models/"+url.PathEscape(id)), nil, nil, nil)
}

// ReflectAnswer is Hindsight's reflect answer and its token use.
type ReflectAnswer struct {
	Text  string `json:"text"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// Reflect asks the server's LLM a question over the memory (an agentic
// loop over many memories: costly, never on a hook path).
func (c *Client) Reflect(ctx context.Context, bank, query string, tags []string, maxTokens int) (ReflectAnswer, error) {
	body := map[string]any{"query": query, "budget": "low", "max_tokens": maxTokens}
	if len(tags) > 0 {
		body["tags"], body["tags_match"] = tags, "any_strict"
	}
	var a ReflectAnswer
	return a, c.do(ctx, http.MethodPost, bankPath(bank, "/reflect"), nil, body, &a)
}

// DeleteBank deletes bank and everything in it (setup's scratch bank only).
func (c *Client) DeleteBank(ctx context.Context, bank string) error {
	return c.do(ctx, http.MethodDelete, bankPath(bank, ""), nil, nil, nil)
}
