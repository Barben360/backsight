package retain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/transcript"
)

func TestUUID5KnownVector(t *testing.T) {
	// RFC 4122 DNS namespace, "python.org" (Python's uuid documentation).
	if got := uuid5(nsDNS, "python.org"); got != "886313e1-3b8a-5372-9b90-0c9aee199e5d" {
		t.Errorf("%s", got)
	}
}

func recs() []transcript.Record {
	at := func(m int) time.Time { return time.Date(2026, 10, 24, 9, m, 0, 0, time.UTC) }
	return []transcript.Record{
		{UUID: "u1", SessionID: "s1", CWD: "/w/billing", Timestamp: at(0), Turns: []transcript.Turn{{Role: "user", Content: "Use key " + "sk-" + "ant-" + strings.Repeat("Ab3dE5gH9k", 3) + " for the test", Timestamp: at(0)}}},
		{UUID: "a1", SessionID: "s1", CWD: "/w/billing", Timestamp: at(1), Turns: []transcript.Turn{{Role: "assistant", Content: "Done.", Timestamp: at(1)}, {Role: "action", Content: "Bash go test ./...", Timestamp: at(1)}}},
	}
}

func lines(content string) []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(content, "\n") {
		var m map[string]any
		_ = json.Unmarshal([]byte(l), &m)
		out = append(out, m)
	}
	return out
}

func TestFirstSegment(t *testing.T) {
	seg := BuildSegment("claude-memory", "s1", "conversation:s1", recs(), 1, "billing", "machine-1", "0.1.0")
	it := seg.Item
	ls := lines(it.Content)
	if len(ls) != 4 || ls[0]["role"] != "system" || ls[0]["content"] != "REF-ID: conversation:s1" {
		t.Fatalf("content:\n%s", it.Content)
	}
	if ls[1]["role"] != "user" || !strings.Contains(ls[1]["content"].(string), "[redacted:anthropic_key]") || strings.Contains(it.Content, "Ab3dE5gH9k") {
		t.Errorf("redaction: %s", it.Content)
	}
	if ls[3]["role"] != "action" || ls[3]["timestamp"] != "2026-10-24T09:01:00Z" {
		t.Errorf("action line %v", ls[3])
	}
	if it.DocumentID != "conversation:s1" || it.Strategy != "conversation" || it.Context != "coding agent session" || it.UpdateMode != "" ||
		strings.Join(it.Tags, ",") != "project:billing,source:chat,harness:claude-code" || it.ObservationScopes[0][0] != "project:billing" ||
		!it.Timestamp.Equal(time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("item %+v", it)
	}
	m := it.Metadata
	if m["session_id"] != "s1" || m["last_uuid"] != "a1" || m["records"] != "2" || m["segment"] != "1" || m["machine_id"] != "machine-1" ||
		m["binary_version"] != "0.1.0" || m["source"] != "chat" || m["harness"] != "claude-code" {
		t.Errorf("metadata %v", m)
	}
	if seg.FirstUUID != "u1" || seg.LastUUID != "a1" || len(seg.OperationID) != 36 || seg.OperationID[14] != '5' {
		t.Errorf("%+v", seg)
	}
}

func TestLaterSegmentAppendsWithoutHeader(t *testing.T) {
	seg := BuildSegment("claude-memory", "s1", "conversation:s1", recs()[1:], 3, "billing", "m", "v")
	if seg.Item.UpdateMode != "append" || strings.Contains(seg.Item.Content, "REF-ID") || seg.Item.Metadata["segment"] != "3" {
		t.Errorf("%+v", seg.Item)
	}
}

func TestOperationIDIsDeterministicAndContentBound(t *testing.T) {
	a := BuildSegment("b", "s1", "conversation:s1", recs(), 1, "p", "m", "v").OperationID
	if b := BuildSegment("b", "s1", "conversation:s1", recs(), 1, "p", "m", "v").OperationID; a != b {
		t.Error("same segment, different id")
	}
	if c := BuildSegment("other-bank", "s1", "conversation:s1", recs(), 1, "p", "m", "v").OperationID; a == c {
		t.Error("the bank must be part of the id")
	}
	r := recs()
	r[1].Turns[0].Content = "Done differently."
	if d := BuildSegment("b", "s1", "conversation:s1", r, 1, "p", "m", "v").OperationID; a == d {
		t.Error("the content must be part of the id")
	}
}

func TestCursorRoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadCursor(dir, "s1"); ok || err != nil {
		t.Fatalf("missing cursor: %v %v", ok, err)
	}
	c := Cursor{Transcript: "/t/s1.jsonl", Offset: 1234, Fingerprint: "abc", LastUUID: "a1", Records: 2, Segment: 1,
		Document: "conversation:s1", Home: "billing", Segments: map[string]int{"billing": 1, "infra": 2}, Project: "infra",
		Bank: "b", LastTurnAt: time.Date(2026, 10, 24, 9, 1, 0, 0, time.UTC)}
	if err := SaveCursor(dir, "s1", c); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadCursor(dir, "s1")
	if err != nil || !ok || !reflect.DeepEqual(got, c) {
		t.Errorf("%+v %v %v", got, ok, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "s1.json")); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %o", fi.Mode().Perm())
		}
	}
	if err := SaveCursor(dir, "../evil", c); err == nil {
		t.Error("a session id with a path separator must be refused")
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(p, []byte(strings.Repeat("a", 100)), 0o600)
	f1, err := Fingerprint(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Appending after the consumed part keeps it, even in a small file.
	_ = os.WriteFile(p, []byte(strings.Repeat("a", 100)+"more"), 0o600)
	if f2, _ := Fingerprint(p, 100); f2 != f1 {
		t.Error("an append changed the fingerprint")
	}
	// Only the first 4 KiB count for a long consumed part.
	_ = os.WriteFile(p, []byte(strings.Repeat("a", 5000)), 0o600)
	g1, _ := Fingerprint(p, 5000)
	_ = os.WriteFile(p, []byte(strings.Repeat("a", 4096)+strings.Repeat("b", 904)), 0o600)
	if g2, _ := Fingerprint(p, 5000); g2 != g1 {
		t.Error("bytes beyond 4 KiB changed the fingerprint")
	}
	_ = os.WriteFile(p, []byte("b"+strings.Repeat("a", 99)), 0o600)
	if f3, _ := Fingerprint(p, 100); f3 == f1 {
		t.Error("a rewritten prefix kept the fingerprint")
	}
}

func TestSentIndex(t *testing.T) {
	dir := t.TempDir()
	idx, err := LoadSentIndex(dir)
	if err != nil || idx.Has("u1") {
		t.Fatalf("%v", err)
	}
	if err := idx.Add("u1", "a1"); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadSentIndex(dir)
	if !again.Has("u1") || !again.Has("a1") || again.Has("u2") {
		t.Error("index not persisted")
	}
	_ = again.Add("u2")
	if fi, _ := os.Stat(filepath.Join(dir, "index.bin")); fi.Size() != 24 {
		t.Errorf("3 entries of 8 bytes, got %d bytes", fi.Size())
	}
}

func TestMachineIDIsStable(t *testing.T) {
	dir := t.TempDir()
	a, err := MachineID(dir)
	if err != nil || len(a) != 36 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := MachineID(dir); b != a {
		t.Error("machine id changed")
	}
}

// A torn trailing entry (crash mid-write) must not shift later keys.
func TestSentIndexTornEntry(t *testing.T) {
	dir := t.TempDir()
	idx, _ := LoadSentIndex(dir)
	_ = idx.Add("u1")
	f, _ := os.OpenFile(filepath.Join(dir, "index.bin"), os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.Write([]byte{1, 2, 3})
	_ = f.Close()
	idx, err := LoadSentIndex(dir)
	if err != nil || !idx.Has("u1") {
		t.Fatalf("%v", err)
	}
	if err := idx.Add("u2"); err != nil {
		t.Fatalf("adding after a torn entry: %v", err)
	}
	again, _ := LoadSentIndex(dir)
	if !again.Has("u1") || !again.Has("u2") {
		t.Error("an entry written after a torn one was misaligned")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "index.bin")); fi.Size() != 16 {
		t.Errorf("size %d", fi.Size())
	}
}

// Remove rewrites the file from disk: an entry another flusher appended
// after this index was loaded survives, and the removed ones are gone.
func TestSentIndexRemoveKeepsEntriesAppendedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	mine, _ := LoadSentIndex(dir)
	_ = mine.Add("u1", "u2")
	other, _ := LoadSentIndex(dir)
	_ = other.Add("u3") // after mine was loaded
	if err := mine.Remove("u1"); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadSentIndex(dir)
	if again.Has("u1") || !again.Has("u2") || !again.Has("u3") || mine.Has("u1") || !mine.Has("u3") {
		t.Errorf("after Remove: disk u1=%v u2=%v u3=%v, in memory u1=%v u3=%v", again.Has("u1"), again.Has("u2"), again.Has("u3"), mine.Has("u1"), mine.Has("u3"))
	}
}

// After bank rename, the records sent under the bank's former name are
// still this session's own: the index keeps their old keys (issue 62).
// Without it a re-send would take them for a fork's copies and drop them.
func TestOwnKeyFollowsTheRenamedBank(t *testing.T) {
	idx, _ := LoadSentIndex(t.TempDir())
	_ = idx.Add(sessionKey("claude-memory", "s1", "u1"), sessionKey("backsight", "s1", "u2"), sessionKey("team", "s1", "u3"))
	for _, c := range []struct{ bank, uuid, want string }{
		{"backsight", "u1", sessionKey("claude-memory", "s1", "u1")}, // sent before the rename
		{"backsight", "u2", sessionKey("backsight", "s1", "u2")},
		{"claude-memory", "u1", sessionKey("claude-memory", "s1", "u1")},
		{"claude-memory", "u2", ""}, // never the other way round
		{"other", "u3", ""},         // another bank's keys stay its own
		{"backsight", "u9", ""},
	} {
		if got := ownKey(idx, c.bank, "s1", c.uuid); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.bank, c.uuid, got, c.want)
		}
	}
}

// ReadMachineID reads the id MachineID created and creates nothing: an
// absent or malformed file is no id, and stays as it was.
func TestReadMachineIDCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "machine_id")
	if id := ReadMachineID(dir); id != "" {
		t.Errorf("absent: %q", id)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("ReadMachineID created %s: %v", p, err)
	}
	_ = os.WriteFile(p, []byte("short\n"), 0o600)
	if id := ReadMachineID(dir); id != "" {
		t.Errorf("malformed: %q", id)
	}
	if raw, _ := os.ReadFile(p); string(raw) != "short\n" {
		t.Errorf("malformed file rewritten: %q", raw)
	}
	_ = os.Remove(p)
	made, err := MachineID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id := ReadMachineID(dir); id != made {
		t.Errorf("got %q, want %q", id, made)
	}
}
