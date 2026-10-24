package hindsight

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
	want := `POST /v1/default/banks/coding-agent%3A%3Amemoire/memories/recall {"query":"postgres version","types":["observation"],"budget":"low","max_tokens":1500,"tags":["project:billing","project:.claude"],"tags_match":"any_strict"}`
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

func TestRecentAsksForTheLatestUpdatedOfOneProject(t *testing.T) {
	var q url.Values
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		_, _ = w.Write([]byte(`{"items":[{"id":"o1","text":"t","fact_type":"observation","updated_at":"2026-10-24T09:00:00Z"}]}`))
	})
	got, err := c.Recent(context.Background(), "b", "project:billing", "observation", 5)
	if err != nil || len(got) != 1 || got[0].ID != "o1" {
		t.Fatalf("%+v %v", got, err)
	}
	if q.Get("tags") != "project:billing" || q.Get("tags_match") != "any_strict" || q.Get("time_field") != "updated_at" || q.Get("limit") != "5" || q.Get("type") != "observation" {
		t.Errorf("query %v", q)
	}
	_, _ = c.Recent(context.Background(), "b", "project:billing", "", 5)
	if _, ok := q["type"]; ok {
		t.Errorf("no type filter wanted: %v", q)
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
