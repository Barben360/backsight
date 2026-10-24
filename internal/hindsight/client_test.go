package hindsight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const token = "tok-fake"

func server(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, token, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVersionSendsBearerAndParses(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"api_version":"1.1.0","features":{"observations":true,"bank_config_api":true}}`)
	})
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.APIVersion != "1.1.0" || !v.Features["observations"] {
		t.Errorf("%+v", v)
	}
}

func TestHealth(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"healthy","database":"connected"}`)
	})
	h, err := c.Health(context.Background())
	if err != nil || h.Status != "healthy" || h.Database != "connected" {
		t.Errorf("%+v %v", h, err)
	}
}

// Bank ids contain "::", which must be escaped in the path.
func TestBankStatsEscapesBankID(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/default/banks/coding-agent%3A%3Amemoire/stats" {
			http.Error(w, r.URL.EscapedPath(), http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"bank_id":"coding-agent::memoire","total_nodes":617,"total_documents":24,"nodes_by_fact_type":{"observation":218,"world":244,"experience":155},"pending_operations":2,"failed_operations":1,"last_memory_write_at":"2026-10-24T08:22:37.689498+00:00"}`)
	})
	s, err := c.BankStats(context.Background(), "coding-agent::memoire")
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalNodes != 617 || s.NodesByFactType["observation"] != 218 || s.PendingOperations != 2 || s.FailedOperations != 1 || s.LastMemoryWriteAt.IsZero() {
		t.Errorf("%+v", s)
	}
}

func TestBanks(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"banks":[{"bank_id":"a","name":"A"},{"bank_id":"coding-agent::memoire","name":"m"}]}`)
	})
	banks, err := c.Banks(context.Background())
	if err != nil || len(banks) != 2 || banks[1].ID != "coding-agent::memoire" {
		t.Errorf("%+v %v", banks, err)
	}
}

func TestEachMemoryPaginates(t *testing.T) {
	const total = 450
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("type") != "observation" {
			http.Error(w, "type", http.StatusBadRequest)
			return
		}
		off, _ := strconv.Atoi(q.Get("offset"))
		lim, _ := strconv.Atoi(q.Get("limit"))
		var items []string
		for i := off; i < min(off+lim, total); i++ {
			items = append(items, fmt.Sprintf(`{"id":"m%d","text":"t","fact_type":"observation","tags":["project:p%d"]}`, i, i%3))
		}
		fmt.Fprintf(w, `{"items":[%s],"total":%d,"limit":%d,"offset":%d}`, strings.Join(items, ","), total, lim, off)
	})
	seen := map[string]bool{}
	err := c.EachMemory(context.Background(), "b", "observation", func(m Memory) error {
		if seen[m.ID] {
			t.Errorf("duplicate %s", m.ID)
		}
		seen[m.ID] = true
		return nil
	})
	if err != nil || len(seen) != total {
		t.Errorf("%d seen, err %v", len(seen), err)
	}
}

func TestErrorKinds(t *testing.T) {
	cases := []struct {
		status     int
		header     string
		wantKind   Kind
		retryAfter time.Duration
	}{
		{401, "", Unauthorized, 0},
		{404, "", NotFound, 0},
		{503, "7", Unavailable, 7 * time.Second},
		{500, "", ServerError, 0},
		{422, "", BadRequest, 0},
		{429, "3", RateLimited, 3 * time.Second},
		{403, "", Unauthorized, 0},
	}
	for _, c := range cases {
		cl := server(t, func(w http.ResponseWriter, _ *http.Request) {
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			http.Error(w, `{"detail":"nope"}`, c.status)
		})
		_, err := cl.Version(context.Background())
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != c.wantKind || apiErr.Status != c.status || apiErr.RetryAfter != c.retryAfter {
			t.Errorf("status %d: got %#v", c.status, err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("token leaked in %v", err)
		}
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := New(url, token, time.Second)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("want ErrUnreachable, got %v", err)
	}
}

func TestDeadline(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c.http.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrUnreachable) || time.Since(start) > 2*time.Second {
		t.Errorf("want ErrTimeout (not unreachable), got %v after %s", err, time.Since(start))
	}
}

func TestNewRefusesBadURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "127.0.0.1:8888", "http://", "http://u:fakepw@127.0.0.1:8888"} {
		_, err := New(u, token, time.Second)
		if err == nil {
			t.Errorf("%q accepted", u)
		} else if strings.Contains(err.Error(), "fakepw") {
			t.Errorf("password echoed: %v", err)
		}
	}
	if _, err := New("http://127.0.0.1:8888", token, 0); err == nil {
		t.Error("a zero timeout means no deadline and must be refused")
	}
}

func TestRetryable(t *testing.T) {
	for kind, want := range map[Kind]bool{RateLimited: true, Unavailable: true, ServerError: true,
		Unauthorized: false, NotFound: false, BadRequest: false, Redirected: false} {
		if got := (&APIError{Kind: kind}).Retryable(); got != want {
			t.Errorf("%s: Retryable = %v", kind, got)
		}
	}
}

// A redirect must not carry the bearer token anywhere, even to the same host.
func TestRedirectsAreNotFollowed(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(other.Close)
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/version", http.StatusFound)
	})
	_, err := c.Version(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != Redirected || hit {
		t.Errorf("want a Redirected error and no request to the target, got %v (target hit: %v)", err, hit)
	}
}

func TestOversizedResponseRefused(t *testing.T) {
	old := maxBody
	maxBody = 64
	t.Cleanup(func() { maxBody = old })
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"api_version":"` + strings.Repeat("x", 200) + `"}`))
	})
	if _, err := c.Version(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("want a size refusal, got %v", err)
	}
}

func TestInvalidJSON(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"api_version":`)) })
	if _, err := c.Version(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("got %v", err)
	}
}

func TestNoTokenNoAuthorizationHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL, "", time.Second)
	if _, err := c.Health(context.Background()); err != nil {
		t.Errorf("an empty token must send no Authorization header: %v", err)
	}
}

func TestBasePathPrefixKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/hs/v1/default/banks/a%3Ab/stats" {
			t.Errorf("path %s", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL+"/hs/", token, time.Second)
	if _, err := c.BankStats(context.Background(), "a:b"); err != nil {
		t.Fatal(err)
	}
}

func TestPostSendsJSONBody(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Items []string }
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Items) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	var out struct{ Success bool }
	if err := c.do(context.Background(), http.MethodPost, "/x", nil, map[string][]string{"items": {"a", "b"}}, &out); err != nil || !out.Success {
		t.Errorf("%v %+v", err, out)
	}
}

func TestEachMemoryStopsOnCallbackError(t *testing.T) {
	calls := 0
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"items":[{"id":"a"},{"id":"b"}],"total":1000}`))
	})
	stop := errors.New("stop")
	seen := 0
	err := c.EachMemory(context.Background(), "b", "observation", func(Memory) error { seen++; return stop })
	if !errors.Is(err, stop) || seen != 1 || calls != 1 {
		t.Errorf("err %v, seen %d, calls %d", err, seen, calls)
	}
}

func TestEachMemoryEmptyPageEndsEvenIfTotalLies(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"items":[],"total":1000}`)) })
	if err := c.EachMemory(context.Background(), "b", "observation", func(Memory) error { return nil }); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestEachMemoryPageCap(t *testing.T) {
	old := maxPages
	maxPages = 3
	t.Cleanup(func() { maxPages = old })
	calls := 0
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"items":[{"id":"a"}],"total":999999}`))
	})
	err := c.EachMemory(context.Background(), "b", "observation", func(Memory) error { return nil })
	if err == nil || calls != 3 {
		t.Errorf("want an error after 3 pages, got %v after %d", err, calls)
	}
}
