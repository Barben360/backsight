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
	PruneLive(dir, 7*24*time.Hour, time.Now())
	if _, ok := ReadLive(dir, 4242); ok {
		t.Error("an old record survived pruning")
	}
}
