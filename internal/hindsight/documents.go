package hindsight

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DocumentItem is one row of a bank's document listing (no text).
type DocumentItem struct {
	ID        string
	Tags      []string
	Metadata  map[string]string // string values only
	CreatedAt time.Time         // when the document first arrived; zero when the server gave none
}

// listItem is a document row as the server sends it.
type listItem struct {
	ID        string         `json:"id"`
	Tags      []string       `json:"tags"`
	Metadata  map[string]any `json:"document_metadata"`
	CreatedAt string         `json:"created_at"`
}

func (it listItem) item() DocumentItem {
	d := DocumentItem{ID: it.ID, Tags: it.Tags, Metadata: map[string]string{}, CreatedAt: serverTime(it.CreatedAt)}
	for k, v := range it.Metadata {
		if s, ok := v.(string); ok {
			d.Metadata[k] = s
		}
	}
	return d
}

// ServerTime is serverTime for callers outside the package: the zero time
// when s is not one of the server's ISO times.
func ServerTime(s string) time.Time { return serverTime(s) }

// serverTime reads the server's ISO times, with or without a zone (UTC).
func serverTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// taggedSince is the server-side filter of the documents carrying every
// tag and created at or after since.
func taggedSince(since time.Time, tags []string) url.Values {
	return url.Values{"tags": tags, "tags_match": {"all_strict"}, "time_field": {"created_at"}, "start_date": {since.UTC().Format(time.RFC3339)}}
}

// TaggedDocumentsSince lists the documents carrying every tag created at
// or after since, newest first, page by page.
func (c *Client) TaggedDocumentsSince(ctx context.Context, bank string, since time.Time, tags ...string) ([]DocumentItem, error) {
	var all []DocumentItem
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return all, fmt.Errorf("listing the documents of %s: more than %d pages, stopping", bank, maxPages)
		}
		var page struct {
			Items []listItem `json:"items"`
			Total int        `json:"total"`
		}
		q := taggedSince(since, tags)
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		if err := c.get(ctx, bankPath(bank, "/documents"), q, &page); err != nil {
			return all, err
		}
		for _, it := range page.Items {
			all = append(all, it.item())
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			break
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	return all, nil
}

// CountTaggedDocumentsSince counts the documents TaggedDocumentsSince
// would list, in one request.
func (c *Client) CountTaggedDocumentsSince(ctx context.Context, bank string, since time.Time, tags ...string) (int, error) {
	var page struct {
		Total int `json:"total"`
	}
	q := taggedSince(since, tags)
	q.Set("limit", "1")
	err := c.get(ctx, bankPath(bank, "/documents"), q, &page)
	return page.Total, err
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
			Items []listItem `json:"items"`
			Total int        `json:"total"`
		}
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, bankPath(bank, "/documents"), q, &page); err != nil {
			return all, err
		}
		for _, it := range page.Items {
			all = append(all, it.item())
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

// BankIDs lists the id of every bank on the server, page by page.
func (c *Client) BankIDs(ctx context.Context) ([]string, error) {
	var ids []string
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return ids, fmt.Errorf("listing the banks: more than %d pages, stopping", maxPages)
		}
		var page struct {
			Banks []Bank `json:"banks"`
			Total int    `json:"total"`
		}
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, "/v1/default/banks", q, &page); err != nil {
			return ids, err
		}
		for _, b := range page.Banks {
			ids = append(ids, b.ID)
		}
		offset += len(page.Banks)
		if len(page.Banks) == 0 || offset >= page.Total {
			return ids, nil
		}
	}
}

// FindBank reads bank's entry in the bank listing (its name: the profile
// read endpoint is gone in 1.1.0), searching by id page by page; found is
// false when the server has no such bank.
func (c *Client) FindBank(ctx context.Context, bank string) (b Bank, found bool, err error) {
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return Bank{}, false, fmt.Errorf("looking for bank %s: more than %d pages, stopping", bank, maxPages)
		}
		var page struct {
			Banks []Bank `json:"banks"`
			Total int    `json:"total"`
		}
		// q matches ids and names alike, as a substring: the exact id is
		// picked out here.
		q := url.Values{"q": {bank}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, "/v1/default/banks", q, &page); err != nil {
			return Bank{}, false, err
		}
		for _, it := range page.Banks {
			if it.ID == bank {
				return it, true, nil
			}
		}
		offset += len(page.Banks)
		if len(page.Banks) == 0 || offset >= page.Total {
			return Bank{}, false, nil
		}
	}
}

// DocumentsCreatedSince lists every document of bank created at or after
// since (the server's clock), page by page, whatever its tags.
func (c *Client) DocumentsCreatedSince(ctx context.Context, bank string, since time.Time) ([]DocumentItem, error) {
	var all []DocumentItem
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return all, fmt.Errorf("listing the documents of %s: more than %d pages, stopping", bank, maxPages)
		}
		var page struct {
			Items []listItem `json:"items"`
			Total int        `json:"total"`
		}
		q := url.Values{"time_field": {"created_at"}, "start_date": {since.UTC().Format(time.RFC3339)},
			"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, bankPath(bank, "/documents"), q, &page); err != nil {
			return all, err
		}
		for _, it := range page.Items {
			all = append(all, it.item())
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			return all, nil
		}
	}
}

// NewestDocument counts the documents of bank and reads the creation time
// of the newest (zero when there is none), in one request.
func (c *Client) NewestDocument(ctx context.Context, bank string) (int, time.Time, error) {
	var page struct {
		Items []listItem `json:"items"`
		Total int        `json:"total"`
	}
	// A time field with no dates orders by it, newest first (Hindsight
	// 1.1.0 build_time_clause).
	if err := c.get(ctx, bankPath(bank, "/documents"), url.Values{"time_field": {"created_at"}, "limit": {"1"}}, &page); err != nil {
		return 0, time.Time{}, err
	}
	var newest time.Time
	if len(page.Items) > 0 {
		newest = serverTime(page.Items[0].CreatedAt)
	}
	return page.Total, newest, nil
}
