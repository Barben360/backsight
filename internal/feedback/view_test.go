package feedback

import (
	"errors"
	"testing"
	"time"
)

const (
	idA  = "aaaaaaaa-0000-4000-8000-000000000001"
	idA2 = "aaaaaaaa-1111-4000-8000-000000000002"
	idB  = "bbbbbbbb-0000-4000-8000-000000000003"
)

func TestResolve(t *testing.T) {
	shown := []Show{{ID: idA}, {ID: idA2}, {ID: idB}, {ID: idB}, {ID: "m1"}, {ID: "dddddddd-0000"}, {ID: "dddddddd"}}
	for _, c := range []struct {
		ref, want string
		err       error
	}{
		{idB, idB, nil},               // a full id
		{"bbbbbbbb", idB, nil},        // its 8-character prefix, shown twice: still one id
		{"aaaaaaaa-1", idA2, nil},     // a longer prefix tells two ids apart
		{"m1", "m1", nil},             // a short id that is a whole shown id
		{"dddddddd", "dddddddd", nil}, // a whole id wins over the ids it starts
		{"aaaaaaaa", "", ErrAmbiguous},
		{"cccccccc", "", ErrUnknown},
		{"bbbbbbb", "", ErrUnknown}, // 7 characters
		{"BBBBBBBB", "", ErrUnknown},
		{"m", "", ErrUnknown},
		{"", "", ErrUnknown},
	} {
		got, err := Resolve(c.ref, shown)
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("Resolve(%q) = %q, %v; want %q, %v", c.ref, got, err, c.want, c.err)
		}
	}
	if _, err := Resolve(idA, nil); !errors.Is(err, ErrUnknown) {
		t.Errorf("nothing shown: %v", err)
	}
}

// Shows lists the session's shows only, one per item, keyed by the show's
// own event so two shows of one id in one turn stay apart.
func TestShowsOfOneSession(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 123, time.UTC)
	events := []Event{
		{Kind: "turn", Session: "s1", Turn: "p-1", TS: at},
		{Kind: "shows", Session: "s1", Turn: "p-1", Agent: "a7", Project: "billing", Path: "search", TS: at, Items: []Item{{ID: idA, Rank: 1, Project: "infra"}, {ID: idB, Rank: 2}}},
		{Kind: "shows", Session: "s2", Turn: "q-1", Path: "prompt", TS: at, Items: []Item{{ID: "other", Rank: 1}}},
		{Kind: "credit", Session: "s1", Turn: "p-1", IDs: []string{idA}, TS: at},
	}
	got := Shows(events, "s1")
	if len(got) != 2 {
		t.Fatalf("shows %+v", got)
	}
	want := Show{ID: idB, Turn: "p-1", Agent: "a7", Path: "search", Project: "billing", Rank: 2, At: at,
		Key: "s1|p-1|" + "1793178000000000123" + "|" + idB}
	if got[1] != want {
		t.Errorf("show\n got %+v\nwant %+v", got[1], want)
	}
	// an item's own project wins; a line without one (older) keeps the session's
	if got[0].Project != "infra" {
		t.Errorf("item project: %+v", got[0])
	}
	if len(Shows(events, "s3")) != 0 {
		t.Error("another session's shows")
	}
}
