// Package hindsight is a minimal client for the Hindsight HTTP API (research
// C03). It covers what backsight needs and nothing more.
package hindsight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnreachable wraps connection failures (server down, refused, DNS).
var ErrUnreachable = errors.New("hindsight server unreachable")

// ErrTimeout wraps calls that hit their deadline: the server may still be
// working, so writers keep the item and retry.
var ErrTimeout = errors.New("hindsight call timed out")

// Limits against a buggy or hostile server; variables so tests can shrink them.
var (
	maxBody  int64 = 32 << 20
	maxPages       = 10000 // 2 million memories at 200 per page
)

// connectTimeout bounds the dial: name resolution and the TCP connect
// together (net.Dialer.Timeout), split between the addresses a name
// resolves to. A server that silently drops packets fails a call here, not
// after its whole budget. A LAN or tailnet connect takes milliseconds; 3 s
// leaves room for a far tailnet peer. A host name slower than 3 s to
// resolve fails like a dead server; an IP URL, which the token flow
// writes, resolves nothing.
const connectTimeout = 3 * time.Second

// dialContext opens the connection; a variable so tests need no network.
var dialContext = func(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	return d.DialContext(ctx, network, addr)
}

// Kind classifies HTTP errors so callers can pick a fix.
type Kind string

const (
	Unauthorized Kind = "unauthorized"
	NotFound     Kind = "not_found"
	BadRequest   Kind = "bad_request"
	Unavailable  Kind = "unavailable" // 503: migrating or shedding load
	ServerError  Kind = "server_error"
	RateLimited  Kind = "rate_limited" // 429
	Redirected   Kind = "redirected"   // 3xx: never followed, the token stays put
)

// APIError is a non-2xx response. It never carries the token.
type APIError struct {
	Method, Path string
	Status       int
	Kind         Kind
	RetryAfter   time.Duration
	Body         string // first 300 bytes
}

// Retryable reports whether the same request may succeed later.
func (e *APIError) Retryable() bool {
	return e.Kind == RateLimited || e.Kind == Unavailable || e.Kind == ServerError
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
		return nil, errors.New("invalid Hindsight URL: want http(s)://host[:port][/prefix]")
	}
	if u.User != nil {
		return nil, errors.New("invalid Hindsight URL: credentials belong in secrets.toml, not the URL")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("invalid Hindsight timeout %s", timeout)
	}
	bound := min(connectTimeout, timeout)
	dialer := &net.Dialer{Timeout: bound, KeepAlive: 30 * time.Second}
	tr := http.DefaultTransport.(*http.Transport).Clone() // keeps the environment's proxy and TLS defaults
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialContext(ctx, dialer, network, addr)
	}
	hc := &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &Client{base: u, token: token, http: hc}, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// do sends one request; body, when not nil, is sent as JSON.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *c.base // keeps a base path such as a reverse-proxy prefix
	u.RawPath = strings.TrimSuffix(c.base.EscapedPath(), "/") + path
	u.Path, _ = url.PathUnescape(u.RawPath)
	u.RawQuery = query.Encode()
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if (errors.As(err, &ue) && ue.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s %s", ErrTimeout, method, path)
		}
		return fmt.Errorf("%w: %s %s: %w", ErrUnreachable, method, path, errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		e := &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(msg)}
		switch {
		case resp.StatusCode/100 == 3:
			e.Kind = Redirected
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			e.Kind = Unauthorized
		case resp.StatusCode == http.StatusNotFound:
			e.Kind = NotFound
		case resp.StatusCode == http.StatusTooManyRequests:
			e.Kind = RateLimited
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("%s %s: reading the response: %w", method, path, err)
	}
	if int64(len(raw)) > maxBody {
		return fmt.Errorf("%s %s: response larger than %d bytes", method, path, maxBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: invalid JSON: %w", method, path, err)
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

	TotalLinks         int            `json:"total_links"`
	TotalObservations  int            `json:"total_observations"`
	OperationsByStatus map[string]int `json:"operations_by_status"`
	LinksByLinkType    map[string]int `json:"links_by_link_type"`

	// Present names the fields the response carried, not null: a field it
	// lacks decodes to 0, which is not the server saying 0.
	Present map[string]bool `json:"-"`
}

// UnmarshalJSON reads the two times with serverTime: the server sends them
// with or without a zone, and null or garbage is the zero time.
func (s *BankStats) UnmarshalJSON(b []byte) error {
	type plain BankStats
	aux := struct {
		*plain
		LastConsolidatedAt string `json:"last_consolidated_at"`
		LastMemoryWriteAt  string `json:"last_memory_write_at"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	s.LastConsolidatedAt = serverTime(aux.LastConsolidatedAt)
	s.LastMemoryWriteAt = serverTime(aux.LastMemoryWriteAt)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	s.Present = map[string]bool{}
	for k, v := range fields {
		if string(bytes.TrimSpace(v)) != "null" {
			s.Present[k] = true
		}
	}
	return nil
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
	MentionedAt     time.Time `json:"mentioned_at"`
	SourceMemoryIDs []string  `json:"source_memory_ids"`
	// ConsolidatedAt (a fact): when consolidation last took it in, "" not
	// yet; ConsolidationFailedAt: it failed for good (ISO 8601).
	ConsolidatedAt        string `json:"consolidated_at"`
	ConsolidationFailedAt string `json:"consolidation_failed_at"`
}

// When is the memory's event time, else when the server last wrote it.
func (m Memory) When() time.Time {
	if !m.MentionedAt.IsZero() {
		return m.MentionedAt
	}
	return m.UpdatedAt
}

const pageSize = 200

// EachMemory calls fn for every memory of factType ("observation", "world",
// "experience"), page by page; state "invalidated" lists the archive of
// soft-forgotten facts instead of the live rows.
func (c *Client) EachMemory(ctx context.Context, bank, factType, state string, fn func(Memory) error) error {
	return c.eachMemory(ctx, bank, factType, state, "", fn)
}

// EachTagged is EachMemory over the memories tagged tag only.
func (c *Client) EachTagged(ctx context.Context, bank, factType, state, tag string, fn func(Memory) error) error {
	return c.eachMemory(ctx, bank, factType, state, tag, fn)
}

func (c *Client) eachMemory(ctx context.Context, bank, factType, state, tag string, fn func(Memory) error) error {
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return fmt.Errorf("listing %s memories: more than %d pages, stopping", factType, maxPages)
		}
		var page struct {
			Items []Memory `json:"items"`
			Total int      `json:"total"`
		}
		q := url.Values{"type": {factType}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if state != "" {
			q.Set("state", state) // "invalidated": the archive; default: live rows
		}
		if tag != "" {
			q.Set("tags", tag)
			q.Set("tags_match", "any_strict")
		}
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

// MemoryQuery narrows a listing of live memories.
type MemoryQuery struct {
	Type         string    // world | experience | observation; "" = every type
	DocumentID   string    // the facts extracted from one document
	UpdatedSince time.Time // zero: no time window
}

// ListMemories calls fn for every live memory q selects, page by page.
func (c *Client) ListMemories(ctx context.Context, bank string, q MemoryQuery, fn func(Memory) error) error {
	v := url.Values{}
	if q.Type != "" {
		v.Set("type", q.Type)
	}
	if q.DocumentID != "" {
		v.Set("document_id", q.DocumentID)
	}
	if !q.UpdatedSince.IsZero() {
		v.Set("time_field", "updated_at")
		v.Set("start_date", q.UpdatedSince.UTC().Format(time.RFC3339))
	}
	what := q.Type
	if what == "" {
		what = "document " + q.DocumentID
	}
	for offset, pages := 0, 0; ; pages++ {
		if pages == maxPages {
			return fmt.Errorf("listing %s memories: more than %d pages, stopping", what, maxPages)
		}
		var page struct {
			Items []Memory `json:"items"`
			Total int      `json:"total"`
		}
		v.Set("limit", strconv.Itoa(pageSize))
		v.Set("offset", strconv.Itoa(offset))
		if err := c.get(ctx, bankPath(bank, "/memories/list"), v, &page); err != nil {
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

// CreateBank creates (or updates the profile of) bank.
func (c *Client) CreateBank(ctx context.Context, bank, name, mission string) error {
	return c.do(ctx, http.MethodPut, bankPath(bank, ""), nil, map[string]string{"name": name, "mission": mission}, nil)
}

// BankConfigOverrides returns only the bank-specific config overrides.
func (c *Client) BankConfigOverrides(ctx context.Context, bank string) (map[string]json.RawMessage, error) {
	var out struct {
		Overrides map[string]json.RawMessage `json:"overrides"`
	}
	return out.Overrides, c.get(ctx, bankPath(bank, "/config"), nil, &out)
}

// PatchBankConfig sets the given config keys on bank; other keys are kept.
func (c *Client) PatchBankConfig(ctx context.Context, bank string, updates map[string]any) error {
	return c.do(ctx, http.MethodPatch, bankPath(bank, "/config"), nil, map[string]any{"updates": updates}, nil)
}

// ExtractedFact is one candidate fact from a dry-run extraction.
type ExtractedFact struct {
	Text     string   `json:"text"`
	FactType string   `json:"fact_type"`
	Entities []string `json:"entities"`
}

// DryRunResult is what the retain step would extract.
type DryRunResult struct {
	Facts []ExtractedFact `json:"facts"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// DryRunExtract asks the server's LLM to extract facts from content
// without storing anything.
func (c *Client) DryRunExtract(ctx context.Context, bank, content, strategy string, at time.Time) (DryRunResult, error) {
	var out DryRunResult
	body := map[string]any{"content": content, "strategy": strategy, "timestamp": at.UTC().Format(time.RFC3339), "context": "backsight provider check"}
	return out, c.do(ctx, http.MethodPost, bankPath(bank, "/memories/dry-run-extract"), nil, body, &out)
}

// RetainItem is one item of a retain request.
type RetainItem struct {
	Content           string            `json:"content"`
	Context           string            `json:"context,omitempty"`
	DocumentID        string            `json:"document_id"`
	Strategy          string            `json:"strategy,omitempty"`
	Tags              []string          `json:"tags"`
	ObservationScopes [][]string        `json:"observation_scopes,omitempty"`
	Timestamp         *time.Time        `json:"timestamp,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	UpdateMode        string            `json:"update_mode,omitempty"`
}

// Retain queues items for extraction (async). A repeated operationID is
// collapsed by the server (E21).
func (c *Client) Retain(ctx context.Context, bank string, items []RetainItem, operationID string) error {
	body := map[string]any{"items": items, "async": true}
	if operationID != "" {
		body["operation_id"] = operationID
	}
	return c.do(ctx, http.MethodPost, bankPath(bank, "/memories"), nil, body, nil)
}

// RetainNow retains items synchronously: the documents exist, with their
// metadata, when it returns (an async retain creates them only once its
// operation runs).
func (c *Client) RetainNow(ctx context.Context, bank string, items []RetainItem) error {
	return c.do(ctx, http.MethodPost, bankPath(bank, "/memories"), nil, map[string]any{"items": items, "async": false}, nil)
}

// Document is what backsight reads back of a stored document.
type Document struct {
	ID          string            `json:"id"`
	Tags        []string          `json:"tags"`
	Metadata    map[string]string `json:"document_metadata"`
	MemoryUnits int               `json:"memory_unit_count"`
	// OriginalText: the text retained (a purge hashes a note's, M5 review
	// rules).
	OriginalText string `json:"original_text"`
}

// GetDocument returns a document; an *APIError of kind NotFound when absent.
func (c *Client) GetDocument(ctx context.Context, bank, id string) (Document, error) {
	var d Document
	return d, c.get(ctx, bankPath(bank, "/documents/"+strings.ReplaceAll(url.PathEscape(id), ":", "%3A")), nil, &d)
}

// LLMTokens are the bank's summed LLM token counts over a period.
type LLMTokens struct {
	Input, Output, Cached, Thoughts int64
}

// LLMTokensByDay returns the bank's LLM token sums per UTC day
// ("2006-01-02") over period (e.g. "30d").
func (c *Client) LLMTokensByDay(ctx context.Context, bank, period string) (map[string]LLMTokens, error) {
	var out struct {
		Buckets []struct {
			Time   string `json:"time"`
			Tokens struct {
				Input    int64 `json:"input"`
				Output   int64 `json:"output"`
				Cached   int64 `json:"cached"`
				Thoughts int64 `json:"thoughts"`
			} `json:"tokens"`
		} `json:"buckets"`
	}
	if err := c.get(ctx, bankPath(bank, "/llm-requests/stats"), url.Values{"period": {period}}, &out); err != nil {
		return nil, err
	}
	days := map[string]LLMTokens{}
	for _, b := range out.Buckets {
		day := b.Time
		if len(day) >= 10 {
			day = day[:10]
		}
		t := days[day]
		t.Input += b.Tokens.Input
		t.Output += b.Tokens.Output
		t.Cached += b.Tokens.Cached
		t.Thoughts += b.Tokens.Thoughts
		days[day] = t
	}
	return days, nil
}

// LLMTokens sums the bank's llm-requests/stats buckets over period (e.g.
// "30d"): the measured spend of extraction and consolidation.
func (c *Client) LLMTokens(ctx context.Context, bank, period string) (LLMTokens, error) {
	var out struct {
		Buckets []struct {
			Tokens struct {
				Input    int64 `json:"input"`
				Output   int64 `json:"output"`
				Cached   int64 `json:"cached"`
				Thoughts int64 `json:"thoughts"`
			} `json:"tokens"`
		} `json:"buckets"`
	}
	var t LLMTokens
	if err := c.get(ctx, bankPath(bank, "/llm-requests/stats"), url.Values{"period": {period}}, &out); err != nil {
		return t, err
	}
	for _, b := range out.Buckets {
		t.Input += b.Tokens.Input
		t.Output += b.Tokens.Output
		t.Cached += b.Tokens.Cached
		t.Thoughts += b.Tokens.Thoughts
	}
	return t, nil
}

// QueuedOperations counts the bank's pending and processing operations
// (async retains and consolidations). /stats does not count async retains
// on Hindsight 1.1.0, the operations list does.
// Operation is one async operation of a bank, as its listing shows it.
type Operation struct {
	ID         string    `json:"id"`
	Type       string    `json:"operation_type"`
	Status     string    `json:"status"`
	DocumentID string    `json:"document_id"`
	UpdatedAt  time.Time `json:"updated_at"`
	CreatedAt  time.Time `json:"created_at"`
	RetryCount int       `json:"retry_count"`
	TaskType   string    `json:"task_type"`
	// error_message is free text and is deliberately not decoded.
}

// UnmarshalJSON reads the times with serverTime (zone-less is UTC, null or
// garbage is the zero time) and a null retry_count as 0.
func (o *Operation) UnmarshalJSON(b []byte) error {
	type plain Operation
	aux := struct {
		*plain
		UpdatedAt  string `json:"updated_at"`
		CreatedAt  string `json:"created_at"`
		RetryCount *int   `json:"retry_count"`
	}{plain: (*plain)(o)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	o.UpdatedAt, o.CreatedAt = serverTime(aux.UpdatedAt), serverTime(aux.CreatedAt)
	o.RetryCount = 0
	if aux.RetryCount != nil {
		o.RetryCount = *aux.RetryCount
	}
	return nil
}

// Operations lists up to limit operations of bank with status (pending,
// processing…), and how many there are.
func (c *Client) Operations(ctx context.Context, bank, status string, limit int) ([]Operation, int, error) {
	var out struct {
		Total      int         `json:"total"`
		Operations []Operation `json:"operations"`
	}
	err := c.get(ctx, bankPath(bank, "/operations"), url.Values{"status": {status}, "limit": {strconv.Itoa(limit)}}, &out)
	return out.Operations, out.Total, err
}

func (c *Client) QueuedOperations(ctx context.Context, bank string) (int, error) {
	n := 0
	for _, status := range []string{"pending", "processing"} {
		var out struct {
			Total int `json:"total"`
		}
		if err := c.get(ctx, bankPath(bank, "/operations"), url.Values{"status": {status}, "limit": {"1"}}, &out); err != nil {
			return 0, err
		}
		n += out.Total
	}
	return n, nil
}

// QueuedOperationsOf lists the operations of bank with status, 100 per
// page, up to pages pages; complete is false when the queue is longer.
func (c *Client) QueuedOperationsOf(ctx context.Context, bank, status string, pages int) (ops []Operation, complete bool, err error) {
	for page := 0; page < pages; page++ {
		var out struct {
			Total      int         `json:"total"`
			Operations []Operation `json:"operations"`
		}
		q := url.Values{"status": {status}, "limit": {"100"}, "offset": {strconv.Itoa(len(ops))}}
		if err := c.get(ctx, bankPath(bank, "/operations"), q, &out); err != nil {
			return ops, false, err
		}
		ops = append(ops, out.Operations...)
		if len(ops) >= out.Total {
			return ops, true, nil
		}
		if len(out.Operations) == 0 { // the queue moved under the paging: not read whole
			return ops, false, nil
		}
	}
	return ops, false, nil
}

// CreateBankAlias makes alias a second id of bank: a client still calling
// the alias reaches the bank (Hindsight resolves it before anything reads
// it). 409 when alias already names a bank or another alias.
func (c *Client) CreateBankAlias(ctx context.Context, bank, alias string) error {
	return c.do(ctx, http.MethodPost, bankPath(bank, "/aliases"), nil, map[string]any{"alias": alias}, nil)
}
