package transcript

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, name string, from int64) ([]Record, Stats) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	recs, st, err := Read(bytes.NewReader(raw[from:]), from)
	if err != nil {
		t.Fatal(err)
	}
	return recs, st
}

type flat struct{ uuid, role, content string }

func flatten(recs []Record) []flat {
	var out []flat
	for _, r := range recs {
		for _, tn := range r.Turns {
			out = append(out, flat{r.UUID, tn.Role, tn.Content})
		}
	}
	return out
}

func TestNormalise2_1_310(t *testing.T) {
	recs, st := read(t, "session-2.1.310.jsonl", 0)
	want := []flat{
		{"u1", "user", "Why does the billing export fail on large months?"},
		{"a1", "assistant", "Let me look at the exporter."},
		{"a1", "action", "Read /home/u/work/billing/export.go"},
		{"a2", "action", "Bash go test ./..."},
		{"a2", "action", "Grep /home/u/work/billing"}, // path comes before pattern (C04 key order)
		{"a2", "action", "TodoWrite"},
		{"u4", "user", "OK, raise the page size to 5000 and keep the cursor pagination."},
		{"a3", "assistant", "Done: page size 5000, cursor pagination kept."},
	}
	got := flatten(recs)
	if len(got) != len(want) {
		t.Fatalf("got %d turns:\n%v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("turn %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	all := ""
	for _, f := range got {
		all += f.content
	}
	for _, leak := range []string{"secret reasoning", "thousands of lines", "subagent chatter", "Caveat: meta", "continued from a previous", "Injected context", "/clear"} {
		if strings.Contains(all, leak) {
			t.Errorf("kept %q", leak)
		}
	}
	if st.Unknown["attachment"] != 1 || st.Unknown["ai-title"] != 1 || st.Unknown["permission-mode"] != 1 || st.BadLines != 1 {
		t.Errorf("stats %+v", st)
	}
	r := recs[0]
	if r.SessionID != "s-310" || r.CWD != "/home/u/work/billing" || !r.Timestamp.Equal(time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("record fields %+v", r)
	}
}

// A line still being written is not consumed: the next read resumes there.
func TestPartialLastLineAndResume(t *testing.T) {
	raw, _ := os.ReadFile("testdata/session-2.1.310.jsonl")
	recs, st := read(t, "session-2.1.310.jsonl", 0)
	last := recs[len(recs)-1]
	if st.Consumed >= int64(len(raw)) || !strings.HasPrefix(string(raw[st.Consumed:]), `{"type":"user","uuid":"u5"`) {
		t.Fatalf("consumed %d of %d: rest %q", st.Consumed, len(raw), raw[st.Consumed:])
	}
	if string(raw[last.Start:last.End]) == "" || !strings.Contains(string(raw[last.Start:last.End]), `"uuid":"a3"`) {
		t.Errorf("byte range of a3 wrong: %q", raw[last.Start:last.End])
	}
	// Resuming after a3 yields nothing new until the partial line completes.
	more, st2 := read(t, "session-2.1.310.jsonl", last.End)
	if len(more) != 0 || st2.Consumed != st.Consumed {
		t.Errorf("resume: %v %+v", more, st2)
	}
	// Resuming from u4 yields exactly u4 and a3.
	var u4 Record
	for _, r := range recs {
		if r.UUID == "u4" {
			u4 = r
		}
	}
	tail, _ := read(t, "session-2.1.310.jsonl", u4.Start)
	if len(tail) != 2 || tail[0].UUID != "u4" || tail[1].UUID != "a3" {
		t.Errorf("tail %v", tail)
	}
}

func TestNormalise2_1_128(t *testing.T) {
	recs, st := read(t, "session-2.1.128.jsonl", 0)
	got := flatten(recs)
	if len(got) != 3 || got[0].content != "Remember: always use pnpm in my repos." || got[1].content != "Noted, pnpm everywhere." {
		t.Fatalf("%v", got)
	}
	action := got[2].content
	if !strings.HasPrefix(action, "WebFetch https://example.com/") || len(action) != len("WebFetch ")+100 {
		t.Errorf("target must be capped at 100 chars: %q (%d)", action, len(action))
	}
	if st.Unknown["summary"] != 1 {
		t.Errorf("%+v", st)
	}
}

func TestEmptyInput(t *testing.T) {
	recs, st, err := Read(strings.NewReader(""), 0)
	if err != nil || len(recs) != 0 || st.Consumed != 0 {
		t.Errorf("%v %+v %v", recs, st, err)
	}
}

// A secret at the 100-character cut must not leak its prefix.
func TestActionRedactedBeforeTheCut(t *testing.T) {
	key := "gh" + "p_" + strings.Repeat("a1B2c3D4e5F6", 3)
	cmd := strings.Repeat("x", 90) + " " + key
	line := `{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-10-24T09:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"` + cmd + `"}}]}}` + "\n"
	recs, _, err := Read(strings.NewReader(line), 0)
	if err != nil || len(recs) != 1 {
		t.Fatal(err)
	}
	got := recs[0].Turns[0].Content
	if strings.Contains(got, "a1B2c3") || strings.Contains(got, "gh"+"p_") {
		t.Errorf("token prefix leaked: %q", got)
	}
}

func TestTimestampsMissingOrMalformedKeepTheRecord(t *testing.T) {
	in := `{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-10-24T09:00:00+02:00","message":{"role":"user","content":"first"}}
{"type":"user","uuid":"u2","sessionId":"s","timestamp":"yesterday","message":{"role":"user","content":"malformed"}}
{"type":"user","uuid":"u3","sessionId":"s","message":{"role":"user","content":"missing"}}
`
	recs, st, err := Read(strings.NewReader(in), 0)
	if err != nil || len(recs) != 3 || st.BadLines != 0 {
		t.Fatalf("%d records, %+v, %v", len(recs), st, err)
	}
	want := time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC) // +02:00 normalised to UTC
	for _, r := range recs {
		if !r.Timestamp.Equal(want) || r.Timestamp.Location() != time.UTC {
			t.Errorf("%s: %v", r.UUID, r.Timestamp)
		}
	}
}

// An oversized line is skipped and counted; the records after it are read.
func TestOversizedLineIsSkipped(t *testing.T) {
	old := maxLine
	maxLine = 200
	t.Cleanup(func() { maxLine = old })
	big := `{"type":"user","uuid":"big","sessionId":"s","timestamp":"2026-10-24T09:00:00Z","message":{"role":"user","content":"` + strings.Repeat("x", 500) + `"}}`
	in := big + "\n" + `{"type":"user","uuid":"u2","sessionId":"s","timestamp":"2026-10-24T09:01:00Z","message":{"role":"user","content":"after"}}` + "\n"
	recs, st, err := Read(strings.NewReader(in), 0)
	if err != nil || len(recs) != 1 || recs[0].UUID != "u2" || st.BadLines != 1 || st.Consumed != int64(len(in)) {
		t.Errorf("%v %+v %v", recs, st, err)
	}
}

func TestMultiBlockAssistantRecord(t *testing.T) {
	in := `{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-10-24T09:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"one"},{"type":"tool_use","name":"Read","input":{"file_path":"/a"}},{"type":"text","text":"two"}]}}` + "\n"
	recs, _, _ := Read(strings.NewReader(in), 0)
	got := flatten(recs)
	if len(got) != 2 || got[0].content != "one\ntwo" || got[1].content != "Read /a" {
		t.Errorf("%v", got)
	}
}

func TestToolUsesAreKept(t *testing.T) {
	in := `{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-10-24T09:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t7","name":"mcp__hindsight__hindsight_ingest_document","input":{"title":"Décision : x","content":"y"}}]}}` + "\n"
	recs, _, _ := Read(strings.NewReader(in), 0)
	if len(recs) != 1 || len(recs[0].Tools) != 1 {
		t.Fatalf("%+v", recs)
	}
	tu := recs[0].Tools[0]
	if tu.ID != "t7" || tu.Name != "mcp__hindsight__hindsight_ingest_document" || string(tu.Input["title"]) != `"Décision : x"` {
		t.Errorf("%+v", tu)
	}
}
