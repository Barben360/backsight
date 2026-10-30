package hindsight

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRecallSendsTheScopeAndReadsScores(t *testing.T) {
	var got string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.EscapedPath() + " " + string(body)
		_, _ = w.Write([]byte(`{"results":[{"id":"m1","text":"PostgreSQL 16 stays.","type":"observation","tags":["project:billing"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":0.91,"final":1.1}},{"id":"m2","text":"","type":"observation"}]}`))
	})
	hits, err := c.Recall(context.Background(), "coding-agent::memoire", RecallRequest{Query: "postgres version", Types: []string{"observation"},
		Budget: "low", MaxTokens: 1500, Tags: []string{"project:billing", "project:.claude"}, TagsMatch: "any_strict"})
	want := `POST /v1/default/banks/coding-agent%3A%3Amemoire/memories/recall {"query":"postgres version","types":["observation"],"budget":"low","max_tokens":1500,"tags":["project:billing","project:.claude"],"tags_match":"any_strict","include":{"entities":null}}`
	if err != nil || got != want {
		t.Fatalf("err %v\ngot  %s\nwant %s", err, got, want)
	}
	if len(hits) != 2 || hits[0].ID != "m1" || hits[0].Scores.Reranker != 0.91 || hits[0].Tags[0] != "project:billing" || hits[0].MentionedAt == "" {
		t.Errorf("%+v", hits)
	}
	if _, err := c.Recall(context.Background(), "b", RecallRequest{Query: "x"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "tags") {
		t.Errorf("a whole-bank recall must not send tags: %s", got)
	}
}

func TestGetMemoryReadsSources(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/default/banks/b/memories/m1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"m1","text":"t","type":"observation","tags":["project:p"],"mentioned_at":"2026-10-20T09:00:00Z","document_id":null,"source_memories":[{"id":"f1","text":"src","type":"world","document_id":"conversation:s1","tags":["project:p","kind:decision"]}]}`))
	})
	m, err := c.GetMemory(context.Background(), "b", "m1")
	if err != nil || m.Text != "t" || len(m.Sources) != 1 || m.Sources[0].DocumentID != "conversation:s1" || m.Sources[0].Tags[1] != "kind:decision" {
		t.Fatalf("%+v %v", m, err)
	}
	_, err = c.GetMemory(context.Background(), "b", "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != NotFound {
		t.Errorf("want NotFound, got %v", err)
	}
}

func TestTagsAndEntitiesPage(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/default/banks/b/tags":
			if r.URL.Query().Get("q") != "project:*" { // the server's wildcard syntax (an exact match without *)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("offset") == "0" {
				_, _ = w.Write([]byte(`{"items":[{"tag":"project:a","count":3},{"tag":"project:b","count":1}],"total":3}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"tag":"project:c","count":1}],"total":3}`))
		case "/v1/default/banks/b/entities":
			if r.URL.Query().Get("tags") != "project:a" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"canonical_name":"PostgreSQL","mention_count":4}],"total":1}`))
		}
	})
	tags, err := c.Tags(context.Background(), "b", "project:")
	if err != nil || len(tags) != 3 || tags[2].Tag != "project:c" || tags[0].Count != 3 {
		t.Fatalf("%+v %v", tags, err)
	}
	ents, err := c.Entities(context.Background(), "b", "project:a", 200)
	if err != nil || len(ents) != 1 || ents[0].Name != "PostgreSQL" || ents[0].Mentions != 4 {
		t.Errorf("%+v %v", ents, err)
	}
}

// Recent orders by event time; it falls back to updated_at only when no
// memory has an event time (the server leaves those out of a mentioned_at
// listing).
func TestRecentAsksForTheLatestByEventTime(t *testing.T) {
	var fields []string
	var q url.Values
	withEventTime := true
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		fields = append(fields, q.Get("time_field"))
		if q.Get("time_field") == "mentioned_at" && !withEventTime {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"o1","text":"t","fact_type":"observation","updated_at":"2026-10-24T09:00:00Z","mentioned_at":"2026-10-04T13:30:50.397000+00:00"}]}`))
	})
	got, err := c.Recent(context.Background(), "b", "project:billing", "observation", 5)
	if err != nil || len(got) != 1 || !got[0].When().Equal(time.Date(2026, 10, 4, 13, 30, 50, 397000000, time.UTC)) {
		t.Fatalf("%+v %v", got, err)
	}
	if q.Get("tags") != "project:billing" || q.Get("tags_match") != "any_strict" || strings.Join(fields, ",") != "mentioned_at" || q.Get("limit") != "5" || q.Get("type") != "observation" {
		t.Errorf("query %v, fields %v", q, fields)
	}
	fields, withEventTime = nil, false
	got, _ = c.Recent(context.Background(), "b", "project:billing", "", 5)
	if _, ok := q["type"]; ok || strings.Join(fields, ",") != "mentioned_at,updated_at" || len(got) != 1 {
		t.Errorf("fallback: fields %v, query %v", fields, q)
	}
	// No tag: the bank's latest across projects, no tag filter sent (an
	// empty one matches nothing: ruling R14).
	fields, withEventTime = nil, true
	if got, _ = c.Recent(context.Background(), "b", "", "", 20); len(got) != 1 {
		t.Fatalf("no tag: %+v", got)
	}
	if _, ok := q["tags"]; ok {
		t.Errorf("no tag, yet tags sent: %v", q)
	}
	if _, ok := q["tags_match"]; ok || q.Get("limit") != "20" {
		t.Errorf("no tag: query %v", q)
	}
	if (Memory{UpdatedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}).When().Day() != 24 {
		t.Error("no event time: When is updated_at")
	}
}

func TestCurationCalls(t *testing.T) {
	var got []string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.EscapedPath()+" "+string(body))
		if strings.HasSuffix(r.URL.Path, "/reflect") {
			_, _ = w.Write([]byte(`{"text":"answer","usage":{"input_tokens":10,"output_tokens":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	_ = c.Invalidate(ctx, "b", "m1", "forgotten", false)
	_ = c.Invalidate(ctx, "b", "m1", "restored", true)
	_ = c.ClearObservations(ctx, "b", "f1")
	_ = c.DeleteDocument(ctx, "b", "conversation:s1")
	a, err := c.Reflect(ctx, "b", "why?", []string{"project:p"}, 500)
	want := []string{
		`PATCH /v1/default/banks/b/memories/m1 {"reason":"forgotten","state":"invalidated"}`,
		`PATCH /v1/default/banks/b/memories/m1 {"reason":"restored","state":"valid"}`,
		`DELETE /v1/default/banks/b/memories/f1/observations `,
		`DELETE /v1/default/banks/b/documents/conversation%3As1 `,
		`POST /v1/default/banks/b/reflect {"budget":"low","max_tokens":500,"query":"why?","tags":["project:p"],"tags_match":"any_strict"}`,
	}
	if err != nil || a.Text != "answer" || a.Usage.InputTokens != 10 || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("err %v\ngot:\n%s\nwant:\n%s", err, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRecallAsksForSourceFactsAndReadsTheirTags(t *testing.T) {
	var got string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = string(body)
		_, _ = w.Write([]byte(`{"results":[
			{"id":"o1","text":"obs","type":"observation","source_fact_ids":["f1","f2","f9"]},
			{"id":"o2","text":"no sources","type":"observation","source_fact_ids":null},
			{"id":"w1","text":"raw","type":"world","tags":["kind:state"]}],
			"source_facts":{"f1":{"id":"f1","tags":["project:p","kind:decision"]},"f2":{"id":"f2","tags":["kind:decision","kind:pitfall","source:subagent"]}},
			"source_facts_truncated":true}`))
	})
	hits, err := c.Recall(context.Background(), "b", RecallRequest{Query: "x", SourceFacts: true})
	if err != nil || !strings.Contains(got, `"include":{"entities":null,"source_facts":{"max_tokens":8192}}`) {
		t.Fatalf("err %v body %s", err, got)
	}
	if want := "project:p kind:decision kind:pitfall source:subagent"; strings.Join(hits[0].SourceTags, " ") != want {
		t.Errorf("source tags %q, want %q (each once; f9 was cut by the server's budget)", hits[0].SourceTags, want)
	}
	if len(hits[1].SourceTags) != 0 || len(hits[2].SourceTags) != 0 {
		t.Errorf("a hit without source facts got tags: %+v", hits[1:])
	}
	if _, err := c.Recall(context.Background(), "b", RecallRequest{Query: "x"}); err != nil || strings.Contains(got, "source_facts") {
		t.Errorf("a plain recall must not ask for source facts (err %v): %s", err, got)
	}
}

// The observation history as Hindsight 1.1.0 answers it: a list, oldest
// change first, each with its changed_at (with or without a zone).
func TestHistoryReadsChanges(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.URL.EscapedPath() == "/v1/default/banks/b/memories/o1/history":
			_, _ = w.Write([]byte(`[{"previous_text":null,"previous_tags":[],"previous_occurred_start":null,"previous_occurred_end":null,"previous_mentioned_at":null,"changed_at":"2026-10-20T09:00:00.123456+00:00","new_source_memory_ids":["f1"],"source_facts":[{"id":"f1","text":"a fact","type":"world","context":null,"is_new":true}]},` +
				`{"previous_text":"old","previous_tags":["project:p"],"changed_at":"2026-10-28T11:30:00","new_source_memory_ids":[],"source_facts":[]}]`))
		case r.URL.EscapedPath() == "/v1/default/banks/b/memories/f1/history":
			_, _ = w.Write([]byte(`[]`)) // a raw fact has no history
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Memory unit 'nope' not found"}`))
		}
	})
	h, err := c.History(context.Background(), "b", "o1")
	if err != nil || len(h) != 2 {
		t.Fatalf("%+v %v", h, err)
	}
	if !h[0].ChangedAt.Equal(time.Date(2026, 10, 20, 9, 0, 0, 123456000, time.UTC)) || !h[1].ChangedAt.Equal(time.Date(2026, 10, 28, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("changed_at %v, %v", h[0].ChangedAt, h[1].ChangedAt)
	}
	if h, err := c.History(context.Background(), "b", "f1"); err != nil || len(h) != 0 {
		t.Errorf("raw fact: %+v %v", h, err)
	}
	_, err = c.History(context.Background(), "b", "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != NotFound {
		t.Errorf("want NotFound, got %v", err)
	}
}
