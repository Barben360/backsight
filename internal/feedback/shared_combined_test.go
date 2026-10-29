package feedback

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"testing"
	"time"
)

// The combined view the tick saves, read and decayed hours later, equals
// combining the documents then, within 1e-9; hidings carry over as they
// are.
func TestCombinedThenDecayedEqualsCombiningLater(t *testing.T) {
	docs := []SharedDoc{
		viaMeta(t, SharedDoc{V: 1, Machine: "pc-b", At: daysBefore(2), Entries: map[string]SharedEntry{
			idM1: {Credits: 2.5, CreditsAt: daysBefore(100), Misses: 0.75, MissesAt: daysBefore(3)},
			idM2: {HiddenSince: daysBefore(4)},
		}}),
		viaMeta(t, SharedDoc{V: 1, Machine: "pc-c", At: daysBefore(5), Entries: map[string]SharedEntry{
			idM1: {Credits: 1.5, CreditsAt: daysBefore(200), Misses: 1, MissesAt: daysBefore(50)},
			idR1: {Misses: 2.25, MissesAt: daysBefore(90), HiddenSince: daysBefore(1)},
		}}),
	}
	dir := t.TempDir()
	combinedAt := sharedNow.Add(-5 * time.Hour)
	if err := SaveSharedCombined(dir, combinedAt, CombineRemote(docs, "pc-a", combinedAt, sharedParams)); err != nil {
		t.Fatal(err)
	}
	c, err := LoadSharedCombined(dir, sharedNow)
	if err != nil || !c.At().Equal(combinedAt) || c.Empty() {
		t.Fatalf("load: %v at %s", err, c.At())
	}
	got := Ledger{Memories: map[string]Entry{}}
	for _, id := range []string{idM1, idM2, idR1} {
		s, ok := c.Get(id)
		if !ok {
			t.Fatalf("%s not read", id)
		}
		got.Memories[id] = Entry{}.WithRemote(s, sharedParams, sharedNow)
	}
	want := Ledger{Memories: map[string]Entry{}}.WithRemote(CombineRemote(docs, "pc-a", sharedNow, sharedParams), sharedParams, sharedNow)
	if len(got.Memories) != 3 || len(want.Memories) != 3 {
		t.Fatalf("got %+v, want %+v", got.Memories, want.Memories)
	}
	for id, w := range want.Memories {
		g, w := got.Memories[id].DecayedTo(sharedNow, sharedParams), w.DecayedTo(sharedNow, sharedParams)
		if math.Abs(g.Credits-w.Credits) > 1e-9 || math.Abs(g.Misses-w.Misses) > 1e-9 || (g.Hidden == nil) != (w.Hidden == nil) ||
			(g.Hidden != nil && !g.Hidden.Since.Equal(w.Hidden.Since)) {
			t.Errorf("%s: got %+v, want %+v", id, g, w)
		}
		if math.Abs(got.Boost(id, sharedParams, sharedNow)-want.Boost(id, sharedParams, sharedNow)) > 1e-9 {
			t.Errorf("%s: boost differs", id)
		}
	}
}

// A combined view is refused when missing, garbage, of another version,
// more than 30 days old or more than a day ahead.
func TestLoadSharedCombinedRefuses(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadSharedCombined(dir, sharedNow); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	r := Remote{idM1: {Credits: 1, CreditsAt: daysBefore(1)}}
	for name, c := range map[string]struct {
		at     time.Time
		accept bool
	}{
		"30 days":         {sharedNow.Add(-30 * 24 * time.Hour), true},
		"30 days and 1 s": {sharedNow.Add(-30*24*time.Hour - time.Second), false},
		"a day ahead":     {sharedNow.Add(24 * time.Hour), true},
		"a day and 1 s":   {sharedNow.Add(24*time.Hour + time.Second), false},
	} {
		if err := SaveSharedCombined(dir, c.at, r); err != nil {
			t.Fatal(err)
		}
		got, err := LoadSharedCombined(dir, sharedNow)
		if e, ok := got.Get(idM1); c.accept != (err == nil) || (c.accept && (!ok || e.Credits != 1)) {
			t.Errorf("%s: %+v, %v", name, e, err)
		}
	}
	// A line that does not read is not read, the others are.
	if err := os.WriteFile(SharedCombinedFile(dir), []byte("v1 1793000000\n"+idM1+"\t1\t1e30\t0\t0\t0\n"+idM2+"\t1\t1793000000\t0\t0\t0\n"+idR1+"\t-1\t0\t0\t0\t0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadSharedCombined(dir, sharedNow); err != nil {
		t.Errorf("one bad line refused the view: %v", err)
	} else if _, ok := c.Get(idM1); ok {
		t.Error("a line with a time out of range was read")
	} else if _, ok := c.Get(idR1); ok {
		t.Error("a line with a negative count was read")
	} else if e, ok := c.Get(idM2); !ok || e.Credits != 1 || len(c.Remote()) != 1 {
		t.Errorf("the good line: %+v %v", e, ok)
	}
	for name, content := range map[string]string{
		"garbage":       "{",
		"version 2":     "v2 1793000000\n",
		"no time":       "v1 \n",
		"a JSON object": `{"v":1,"at":1793000000,"entries":{}}`,
	} {
		if err := os.WriteFile(SharedCombinedFile(dir), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSharedCombined(dir, sharedNow); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Get finds every memory of a view, and none it does not hold, whatever
// its place in the sorted lines.
func TestCombinedGetFindsEveryMemory(t *testing.T) {
	dir := t.TempDir()
	r := Remote{}
	for i := range 1000 {
		r[fmt.Sprintf("%08x-0000-4000-8000-000000000000", i*7)] = SharedEntry{Credits: float64(i), CreditsAt: daysBefore(1)}
	}
	if err := SaveSharedCombined(dir, sharedNow, r); err != nil {
		t.Fatal(err)
	}
	c, err := LoadSharedCombined(dir, sharedNow)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 7000 {
		e, ok := c.Get(fmt.Sprintf("%08x-0000-4000-8000-000000000000", i))
		if ok != (i%7 == 0) || (ok && e.Credits != float64(i/7)) {
			t.Fatalf("%d: %+v %v", i, e, ok)
		}
	}
	for _, id := range []string{"", "0", "zzzzzzzz-0000-4000-8000-000000000000"} {
		if _, ok := c.Get(id); ok {
			t.Errorf("%q found", id)
		}
	}
	if empty, err := (func() (Combined, error) {
		_ = SaveSharedCombined(dir, sharedNow, Remote{})
		return LoadSharedCombined(dir, sharedNow)
	})(); err != nil || !empty.Empty() {
		t.Errorf("empty view: %v %v", empty.Empty(), err)
	}
}

// What recall pays per hook for the other machines' counts: one combined
// view of 10 machines at the 5000-entry cap (50,000 memories) read, and
// the 20 memories a recall ranks looked up in it.
func BenchmarkSharedCombinedPerHook(b *testing.B) {
	dir := b.TempDir()
	r, local := Remote{}, Ledger{Memories: map[string]Entry{}}
	for i := range 50000 {
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
		r[id] = SharedEntry{Credits: 1.25, CreditsAt: daysBefore(3), Misses: 0.5, MissesAt: daysBefore(9)}
		if i < 5000 {
			local.Memories[id] = Entry{Shown: 3, Credits: 1, CreditsAt: daysBefore(2)}
		}
	}
	if err := SaveSharedCombined(dir, sharedNow, r); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		c, err := LoadSharedCombined(dir, sharedNow)
		if err != nil {
			b.Fatal(err)
		}
		for i := range 20 {
			id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i*2000)
			if s, ok := c.Get(id); ok {
				_ = local.Memories[id].WithRemote(s, sharedParams, sharedNow).Boost(sharedParams, sharedNow)
			}
		}
	}
}
