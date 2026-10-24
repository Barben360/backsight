package hindsight

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// RecallRequest is POST /memories/recall. Tags empty: the whole bank.
type RecallRequest struct {
	Query     string   `json:"query"`
	Types     []string `json:"types,omitempty"`
	Budget    string   `json:"budget,omitempty"` // low | mid | high
	MaxTokens int      `json:"max_tokens,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	TagsMatch string   `json:"tags_match,omitempty"` // any_strict: tagged with one of Tags
}

// RecallHit is one recalled memory.
type RecallHit struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Type        string   `json:"type"`
	Tags        []string `json:"tags"`
	MentionedAt string   `json:"mentioned_at"`
	Scores      struct {
		Reranker float64 `json:"reranker"`
	} `json:"scores"`
}

// Recall returns the memories matching req, best first.
func (c *Client) Recall(ctx context.Context, bank string, req RecallRequest) ([]RecallHit, error) {
	var out struct {
		Results []RecallHit `json:"results"`
	}
	return out.Results, c.do(ctx, http.MethodPost, bankPath(bank, "/memories/recall"), nil, req, &out)
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

// TagCount is one tag and how many memories carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Tags lists the bank's tags matching prefix q, every page.
func (c *Client) Tags(ctx context.Context, bank, q string) ([]TagCount, error) {
	var all []TagCount
	offset := 0 // counts every item the server returned, matching or not
	for page := 0; page < maxPages; page++ {
		var out struct {
			Items []TagCount `json:"items"`
			Total int        `json:"total"`
		}
		query := url.Values{"q": {q}, "limit": {"200"}, "offset": {strconv.Itoa(offset)}}
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
