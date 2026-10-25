package hindsight

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DocumentItem is one row of a bank's document listing (no text).
type DocumentItem struct {
	ID       string
	Tags     []string
	Metadata map[string]string // string values only
}

// ListDocuments lists every document of bank, page by page; an *APIError
// of kind NotFound when the bank does not exist.
func (c *Client) ListDocuments(ctx context.Context, bank string) ([]DocumentItem, error) {
	var all []DocumentItem
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return all, fmt.Errorf("listing the documents of %s: more than %d pages, stopping", bank, maxPages)
		}
		var page struct {
			Items []struct {
				ID       string         `json:"id"`
				Tags     []string       `json:"tags"`
				Metadata map[string]any `json:"document_metadata"`
			} `json:"items"`
			Total int `json:"total"`
		}
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, bankPath(bank, "/documents"), q, &page); err != nil {
			return all, err
		}
		for _, it := range page.Items {
			d := DocumentItem{ID: it.ID, Tags: it.Tags, Metadata: map[string]string{}}
			for k, v := range it.Metadata {
				if s, ok := v.(string); ok {
					d.Metadata[k] = s
				}
			}
			all = append(all, d)
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			return all, nil
		}
	}
}

// SetDocumentTags replaces a document's tags (and its memories'), without
// processing its text again.
func (c *Client) SetDocumentTags(ctx context.Context, bank, id string, tags []string) error {
	return c.do(ctx, http.MethodPatch, bankPath(bank, "/documents/"+strings.ReplaceAll(url.PathEscape(id), ":", "%3A")), nil, map[string]any{"tags": tags}, nil)
}
