// Package hindsight is a minimal client for the Hindsight HTTP API (research
// C03). It covers what backsight needs and nothing more.
package hindsight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnreachable wraps connection failures (server down, refused, DNS).
var ErrUnreachable = errors.New("hindsight server unreachable")

// Kind classifies HTTP errors so callers can pick a fix.
type Kind string

const (
	Unauthorized Kind = "unauthorized"
	NotFound     Kind = "not_found"
	BadRequest   Kind = "bad_request"
	Unavailable  Kind = "unavailable" // 503: migrating or shedding load
	ServerError  Kind = "server_error"
)

// APIError is a non-2xx response. It never carries the token.
type APIError struct {
	Method, Path string
	Status       int
	Kind         Kind
	RetryAfter   time.Duration
	Body         string // first 300 bytes
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d (%s): %s", e.Method, e.Path, e.Status, e.Kind, e.Body)
}

// Client talks to one Hindsight server. Every call has a total deadline.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New returns a client for baseURL (http or https) with a per-call timeout.
func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid Hindsight URL %q", baseURL)
	}
	return &Client{base: u, token: token, http: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := *c.base // keeps a base path such as a reverse-proxy prefix
	u.RawPath = strings.TrimSuffix(c.base.EscapedPath(), "/") + path
	u.Path, _ = url.PathUnescape(u.RawPath)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return fmt.Errorf("GET %s: timed out after %s", path, c.http.Timeout)
		}
		return fmt.Errorf("%w: GET %s: %w", ErrUnreachable, path, errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		e := &APIError{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Body: string(body)}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			e.Kind = Unauthorized
		case resp.StatusCode == http.StatusNotFound:
			e.Kind = NotFound
		case resp.StatusCode == http.StatusServiceUnavailable:
			e.Kind = Unavailable
		case resp.StatusCode >= 500:
			e.Kind = ServerError
		default:
			e.Kind = BadRequest
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return e
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: invalid JSON: %w", path, err)
	}
	return nil
}

// bankPath escapes the bank id fully: ids like "coding-agent::memoire"
// contain colons, which PathEscape leaves as they are.
func bankPath(bank, suffix string) string {
	return "/v1/default/banks/" + strings.ReplaceAll(url.PathEscape(bank), ":", "%3A") + suffix
}

type Health struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.get(ctx, "/health", nil, &h)
}

type Version struct {
	APIVersion string          `json:"api_version"`
	Features   map[string]bool `json:"features"`
}

func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	return v, c.get(ctx, "/version", nil, &v)
}

type Bank struct {
	ID   string `json:"bank_id"`
	Name string `json:"name"`
}

func (c *Client) Banks(ctx context.Context) ([]Bank, error) {
	var out struct {
		Banks []Bank `json:"banks"`
	}
	return out.Banks, c.get(ctx, "/v1/default/banks", nil, &out)
}

type BankStats struct {
	BankID               string         `json:"bank_id"`
	TotalNodes           int            `json:"total_nodes"`
	TotalDocuments       int            `json:"total_documents"`
	NodesByFactType      map[string]int `json:"nodes_by_fact_type"`
	PendingOperations    int            `json:"pending_operations"`
	FailedOperations     int            `json:"failed_operations"`
	PendingConsolidation int            `json:"pending_consolidation"`
	FailedConsolidation  int            `json:"failed_consolidation"`
	LastConsolidatedAt   time.Time      `json:"last_consolidated_at"`
	LastMemoryWriteAt    time.Time      `json:"last_memory_write_at"`
}

func (c *Client) BankStats(ctx context.Context, bank string) (BankStats, error) {
	var s BankStats
	return s, c.get(ctx, bankPath(bank, "/stats"), nil, &s)
}

// BankConfig returns the bank's effective configuration as raw JSON.
func (c *Client) BankConfig(ctx context.Context, bank string) (map[string]any, error) {
	var out struct {
		Config map[string]any `json:"config"`
	}
	return out.Config, c.get(ctx, bankPath(bank, "/config"), nil, &out)
}

type Memory struct {
	ID              string    `json:"id"`
	Text            string    `json:"text"`
	FactType        string    `json:"fact_type"`
	Tags            []string  `json:"tags"`
	DocumentID      string    `json:"document_id"`
	State           string    `json:"state"`
	UpdatedAt       time.Time `json:"updated_at"`
	SourceMemoryIDs []string  `json:"source_memory_ids"`
}

const pageSize = 200

// EachMemory calls fn for every memory of factType ("observation", "world",
// "experience"), page by page.
func (c *Client) EachMemory(ctx context.Context, bank, factType string, fn func(Memory) error) error {
	for offset := 0; ; {
		var page struct {
			Items []Memory `json:"items"`
			Total int      `json:"total"`
		}
		q := url.Values{"type": {factType}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, bankPath(bank, "/memories/list"), q, &page); err != nil {
			return err
		}
		for _, m := range page.Items {
			if err := fn(m); err != nil {
				return err
			}
		}
		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			return nil
		}
	}
}
