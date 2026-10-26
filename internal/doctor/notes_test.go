package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

type note struct {
	project string
	at      time.Time
}

// notesBank answers the notes counts like the server: documents carrying
// every tag asked, created since start_date.
type notesBank struct {
	notes  []note // source:note documents
	status int
}

func (b *notesBank) fixture(t *testing.T) *fixture {
	base := fakeHandler(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.EscapedPath() != docsPath || !q.Has("tags") {
			base.ServeHTTP(w, r)
			return
		}
		if b.status != 0 {
			w.WriteHeader(b.status)
			return
		}
		since, err := time.Parse(time.RFC3339, q.Get("start_date"))
		if err != nil || q.Get("time_field") != "created_at" || q.Get("tags_match") != "all_strict" {
			t.Errorf("notes query %s", r.URL.RawQuery)
		}
		n := 0
		items := []any{}
		for i, d := range b.notes {
			tags := []string{"source:note", "project:" + d.project}
			ok := !d.at.Before(since)
			for _, tg := range q["tags"] {
				ok = ok && slices.Contains(tags, tg)
			}
			if ok {
				n++
				items = append(items, map[string]any{"id": fmt.Sprintf("note:n%d", i), "tags": tags})
			}
		}
		if q.Get("limit") == "1" {
			items = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": n})
	}))
	t.Cleanup(srv.Close)
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nurl = \""+srv.URL+"\"\nbank = \"coding-agent::memoire\"\n[projects]\ntransverse = [\".claude\", \"home\"]\n", 0o600)
	put(t, f.env.Dirs.SecretsFile(), "api_key = \"fakevalue\"\n", 0o600)
	return f
}

// Decision 0072 M6: doctor counts the notes stored this week, the
// transverse pool's apart, and names the command that lists them.
func TestNotesCheckCountsThisWeeksNotes(t *testing.T) {
	now := time.Now()
	b := &notesBank{}
	f := b.fixture(t)
	expect(t, check(t, f.run(), "notes"), OK, "no note stored in the last 7 days")
	add := func(project string, age time.Duration) {
		b.notes = append(b.notes, note{project, now.Add(-age)})
	}
	add("billing", time.Hour)
	add("billing", 50*time.Hour)
	add(".claude", 3*time.Hour)
	add("billing", 9*24*time.Hour) // older than a week
	c := check(t, f.run(), "notes")
	expect(t, c, Info, "3 notes stored in the last 7 days (1 in the transverse pool)")
	if !strings.Contains(c.Fix, "backsight memory notes") {
		t.Errorf("fix %q", c.Fix)
	}
	b.status = http.StatusInternalServerError
	r := f.run()
	expect(t, check(t, r, "notes"), Info, "cannot count")
}

func TestNotesCheckSkippedWithoutAServer(t *testing.T) {
	f := newFixture(t)
	expect(t, check(t, f.run(), "notes"), Info, "skipped")
}

// Issue 33: a document tagged project:transverse (a replay that kept the
// keyword) is named, with the by-hand fix, since retain reattribute does
// not move notes; a bank without one is fine.
func TestLiteralTransverseTagIsNamed(t *testing.T) {
	b := &notesBank{}
	f := b.fixture(t)
	expect(t, check(t, f.run(), "transverse-keyword"), OK, "no document is tagged project:transverse")
	b.notes = append(b.notes, note{"billing", time.Now()}, note{"transverse", time.Now()})
	c := check(t, f.run(), "transverse-keyword")
	expect(t, c, Warn, "1 document tagged project:transverse")
	for _, want := range []string{"note:n1", "PATCH", "project:.claude", "source:note", "/documents/note%3An1"} {
		if !strings.Contains(c.Fix+c.Detail, want) {
			t.Errorf("%q missing from %q / %q", want, c.Fix, c.Detail)
		}
	}
	if strings.Contains(c.Fix, "note:n0") || strings.Contains(c.Fix, `"project:transverse"`) {
		t.Errorf("fix names a healthy note or keeps the bad tag: %q", c.Fix)
	}
}
