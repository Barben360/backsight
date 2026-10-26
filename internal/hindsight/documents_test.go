package hindsight

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
)

func TestListDocumentsReadsEveryPage(t *testing.T) {
	var offsets []string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/default/banks/b-docs/documents" || r.URL.Query().Get("limit") != "200" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		off := r.URL.Query().Get("offset")
		offsets = append(offsets, off)
		n, _ := strconv.Atoi(off)
		if n >= 201 {
			_, _ = w.Write([]byte(`{"items":[],"total":201,"limit":200,"offset":` + off + `}`))
			return
		}
		items := `{"id":"docs:a#x","tags":["docs-version:1"],"document_metadata":{"section_hash":"h"}}`
		if n == 0 {
			for i := 1; i < 200; i++ {
				items += `,{"id":"d` + strconv.Itoa(i) + `","tags":[]}`
			}
		}
		_, _ = w.Write([]byte(`{"items":[` + items + `],"total":201,"limit":200,"offset":` + off + `}`))
	})
	docs, err := c.ListDocuments(context.Background(), "b-docs")
	if err != nil || len(docs) != 201 || docs[0].ID != "docs:a#x" || docs[0].Tags[0] != "docs-version:1" || docs[0].Metadata["section_hash"] != "h" {
		t.Fatalf("%v %d %+v", err, len(docs), docs[:1])
	}
	if len(offsets) != 2 || offsets[1] != "200" {
		t.Errorf("pages %v", offsets)
	}
}

func TestSetDocumentTagsReplacesThem(t *testing.T) {
	var got string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.EscapedPath() + " " + string(body)
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	if err := c.SetDocumentTags(context.Background(), "b-docs", "docs:install#uninstall", []string{"docs-version:2"}); err != nil {
		t.Fatal(err)
	}
	if want := `PATCH /v1/default/banks/b-docs/documents/docs%3Ainstall%23uninstall {"tags":["docs-version:2"]}`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRecallReadsTheDocumentID(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"id":"u1","text":"t","type":"world","document_id":"docs:a#b","scores":{"final":1.2,"reranker":0.9}}]}`))
	})
	hits, err := c.Recall(context.Background(), "b", RecallRequest{Query: "q"})
	if err != nil || len(hits) != 1 || hits[0].DocumentID != "docs:a#b" || hits[0].Scores.Final != 1.2 {
		t.Fatalf("%v %+v", err, hits)
	}
}

// The listing's search matches ids and names as substrings: only the exact
// id counts, on whichever page it is.
func TestFindBankPicksTheExactIDAcrossPages(t *testing.T) {
	var queries []string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/default/banks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		queries = append(queries, r.URL.Query().Get("q")+"@"+r.URL.Query().Get("offset"))
		if r.URL.Query().Get("offset") == "0" {
			_, _ = w.Write([]byte(`{"banks":[{"bank_id":"m-docs-old","name":"backsight docs"},{"bank_id":"x","name":"m-docs"}],"total":3}`))
			return
		}
		_, _ = w.Write([]byte(`{"banks":[{"bank_id":"m-docs","name":"mine"}],"total":3}`))
	})
	b, found, err := c.FindBank(context.Background(), "m-docs")
	if err != nil || !found || b.Name != "mine" {
		t.Fatalf("%v %v %+v", err, found, b)
	}
	if len(queries) != 2 || queries[0] != "m-docs@0" || queries[1] != "m-docs@2" {
		t.Errorf("queries %v", queries)
	}
	queries = nil
	if _, found, err := c.FindBank(context.Background(), "m-do"); err != nil || found || len(queries) != 2 {
		t.Errorf("a substring is no match: %v %v %v", err, found, queries)
	}
}
