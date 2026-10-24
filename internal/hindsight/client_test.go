package hindsight

import (
	"context"
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
	if err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("err %v after %s", err, time.Since(start))
	}
}

func TestNewRefusesBadURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "127.0.0.1:8888"} {
		if _, err := New(u, token, time.Second); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
