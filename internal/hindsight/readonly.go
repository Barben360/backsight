package hindsight

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrNotGET is returned by the read-only client for any other method.
var ErrNotGET = errors.New("this client only reads: a method other than GET was refused")

// getOnly refuses every request that is not a GET before it dials.
type getOnly struct{ next http.RoundTripper }

func (g getOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrNotGET
	}
	return g.next.RoundTrip(req)
}

// ReadOnly is the dashboard's view of the server: GET only, by transport
// and by method set, and it never decodes free text (error messages, LLM
// inputs or outputs).
type ReadOnly struct {
	c    *Client
	bank string
}

// NewReadOnly returns a read-only client of bank with a per-call timeout.
func NewReadOnly(baseURL, token, bank string, timeout time.Duration) (*ReadOnly, error) {
	c, err := New(baseURL, token, timeout)
	if err != nil {
		return nil, err
	}
	c.http.Transport = getOnly{next: c.http.Transport}
	return &ReadOnly{c: c, bank: bank}, nil
}

func (r *ReadOnly) Version(ctx context.Context) (Version, error) { return r.c.Version(ctx) }

func (r *ReadOnly) BankStats(ctx context.Context) (BankStats, error) {
	return r.c.BankStats(ctx, r.bank)
}

func (r *ReadOnly) Memory(ctx context.Context, id string) (MemoryDetail, error) {
	return r.c.GetMemory(ctx, r.bank, id)
}

// ProjectTags lists every "project:" tag of the bank with its memory count.
func (r *ReadOnly) ProjectTags(ctx context.Context) ([]TagCount, error) {
	return r.c.Tags(ctx, r.bank, "project:")
}

// SeriesBucket is the memories created in one bucket, by fact type.
type SeriesBucket struct {
	Time                           time.Time
	World, Experience, Observation int64
}

// Timeseries is the memories created over a period, padded by the server.
type Timeseries struct {
	Period, Trunc string
	Buckets       []SeriesBucket
}

// MemoriesTimeseries reads the memories created over period (1h, 12h, 1d,
// 7d, 30d, 90d).
func (r *ReadOnly) MemoriesTimeseries(ctx context.Context, period string) (Timeseries, error) {
	var raw struct {
		Period  string `json:"period"`
		Trunc   string `json:"trunc"`
		Buckets []struct {
			Time        string `json:"time"`
			World       int64  `json:"world"`
			Experience  int64  `json:"experience"`
			Observation int64  `json:"observation"`
		} `json:"buckets"`
	}
	q := url.Values{"period": {period}, "time_field": {"created_at"}}
	if err := r.c.get(ctx, bankPath(r.bank, "/stats/memories-timeseries"), q, &raw); err != nil {
		return Timeseries{}, err
	}
	out := Timeseries{Period: raw.Period, Trunc: raw.Trunc, Buckets: make([]SeriesBucket, 0, len(raw.Buckets))}
	for _, b := range raw.Buckets {
		out.Buckets = append(out.Buckets, SeriesBucket{Time: serverTime(b.Time), World: b.World, Experience: b.Experience, Observation: b.Observation})
	}
	return out, nil
}

// LLMBucket is the LLM calls of one bucket: counts by status and tokens.
type LLMBucket struct {
	Time                            time.Time
	Statuses                        map[string]int64
	Total                           int64
	Input, Output, Cached, Thoughts int64
}

// LLMStats reads the LLM request counts and tokens over period (1d, 7d,
// 30d), for one operation or, when operation is "", all of them.
func (r *ReadOnly) LLMStats(ctx context.Context, period, operation string) ([]LLMBucket, error) {
	var raw struct {
		Buckets []struct {
			Time     string           `json:"time"`
			Statuses map[string]int64 `json:"statuses"`
			Total    int64            `json:"total"`
			Tokens   struct {
				Input    int64  `json:"input"`
				Output   int64  `json:"output"`
				Cached   int64  `json:"cached"`
				Thoughts *int64 `json:"thoughts"`
			} `json:"tokens"`
		} `json:"buckets"`
	}
	q := url.Values{"period": {period}}
	if operation != "" {
		q.Set("operation", operation)
	}
	if err := r.c.get(ctx, bankPath(r.bank, "/llm-requests/stats"), q, &raw); err != nil {
		return nil, err
	}
	out := make([]LLMBucket, 0, len(raw.Buckets))
	for _, b := range raw.Buckets {
		lb := LLMBucket{Time: serverTime(b.Time), Statuses: b.Statuses, Total: b.Total, Input: b.Tokens.Input, Output: b.Tokens.Output, Cached: b.Tokens.Cached}
		if b.Tokens.Thoughts != nil {
			lb.Thoughts = *b.Tokens.Thoughts
		}
		out = append(out, lb)
	}
	return out, nil
}

// OperationPage is one page of the operations list, most recent first.
type OperationPage struct {
	Total      int
	Operations []Operation
}

// OperationsPage lists operations filtered by status and type ("" = any).
func (r *ReadOnly) OperationsPage(ctx context.Context, status, typ string, limit, offset int) (OperationPage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if status != "" {
		q.Set("status", status)
	}
	if typ != "" {
		q.Set("type", typ)
	}
	var raw struct {
		Total      int         `json:"total"`
		Operations []Operation `json:"operations"`
	}
	if err := r.c.get(ctx, bankPath(r.bank, "/operations"), q, &raw); err != nil {
		return OperationPage{}, err
	}
	return OperationPage{Total: raw.Total, Operations: raw.Operations}, nil
}
