package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveRecordsByClaudePID(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLive(dir, 4242, Live{SessionID: "s1", Cwd: "/w/billing", Project: "billing", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	l, ok := ReadLive(dir, 4242)
	if !ok || l.SessionID != "s1" || l.Project != "billing" {
		t.Fatalf("%+v %v", l, ok)
	}
	if _, ok := ReadLive(dir, 4343); ok {
		t.Error("another pid has no record")
	}
	_ = WriteLive(dir, 1, Live{SessionID: "s2"})      // init: never a Claude Code process
	_ = WriteLive(dir, 5000, Live{SessionID: "../x"}) // not a session id
	if _, ok := ReadLive(dir, 1); ok {
		t.Error("pid 1 recorded")
	}
	if _, ok := ReadLive(dir, 5000); ok {
		t.Error("invalid session id recorded")
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "by-pid", "4242.json"), old, old)
	_ = WriteLive(dir, 4343, Live{SessionID: "s3"})
	PruneLive(dir, 7*24*time.Hour, time.Now())
	if _, ok := ReadLive(dir, 4242); ok {
		t.Error("an old record survived pruning")
	}
	if _, ok := ReadLive(dir, 4343); !ok {
		t.Error("a fresh record was pruned")
	}
}

// The records under session ids (the MCP server's fallback, 0080) round
// trip, refuse an id that is not one (no path out of the directory), and
// are pruned like the pid ones.
func TestLiveRecordsBySessionID(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC().Truncate(time.Second)
	if err := WriteLiveSession(dir, Live{SessionID: "s1", Cwd: "/w", Project: "web", At: at}); err != nil {
		t.Fatal(err)
	}
	if l, ok := ReadLiveSession(dir, "s1"); !ok || l.Project != "web" || l.Cwd != "/w" || !l.At.Equal(at) {
		t.Errorf("%+v %v", l, ok)
	}
	if _, ok := ReadLiveSession(dir, "s2"); ok {
		t.Error("another id read a record")
	}
	for _, bad := range []string{"", "../s1", "a/b"} {
		_ = WriteLiveSession(dir, Live{SessionID: bad, Project: "x"})
		if _, ok := ReadLiveSession(dir, bad); ok {
			t.Errorf("id %q accepted", bad)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("an invalid id wrote outside by-session: %v", ents)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "by-session", "s1.json"), old, old)
	_ = WriteLiveSession(dir, Live{SessionID: "s3"})
	PruneLive(dir, 7*24*time.Hour, time.Now())
	if _, ok := ReadLiveSession(dir, "s1"); ok {
		t.Error("an old session record survived pruning")
	}
	if _, ok := ReadLiveSession(dir, "s3"); !ok {
		t.Error("a fresh session record was pruned")
	}
}
