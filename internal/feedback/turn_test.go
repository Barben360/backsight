package feedback

import (
	"testing"
	"time"
)

func TestTurnPointerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Turn{ID: "p-123", N: 4, At: time.Date(2026, 10, 28, 9, 30, 0, 12345, time.UTC), Machine: true,
		File: dir + "/journal-2026-10.jsonl", Offset: 4096}
	if err := SetTurn(dir, "sess-1", want); err != nil {
		t.Fatal(err)
	}
	got, ok := CurrentTurn(dir, "sess-1")
	if !ok || got.ID != want.ID || got.N != want.N || !got.At.Equal(want.At) || got.Machine != want.Machine ||
		got.File != want.File || got.Offset != want.Offset {
		t.Fatalf("got %+v, %v; want %+v", got, ok, want)
	}
	next := Turn{ID: "p-124", N: 5, At: want.At.Add(time.Minute)}
	if err := SetTurn(dir, "sess-1", next); err != nil {
		t.Fatal(err)
	}
	if got, ok := CurrentTurn(dir, "sess-1"); !ok || got.ID != "p-124" || got.N != 5 || got.Machine {
		t.Errorf("after a second turn: %+v, %v", got, ok)
	}
	if _, ok := CurrentTurn(dir, "sess-2"); ok {
		t.Error("an unknown session has a turn")
	}
	for _, bad := range []string{"", "../x", "a/b", "."} {
		if err := SetTurn(dir, bad, want); err == nil {
			t.Errorf("session %q accepted", bad)
		}
		if _, ok := CurrentTurn(dir, bad); ok {
			t.Errorf("session %q has a turn", bad)
		}
	}
}
