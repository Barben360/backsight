package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

func dirs(t *testing.T) paths.Dirs {
	t.Helper()
	h := t.TempDir()
	return paths.Dirs{Data: filepath.Join(h, "data"), Config: filepath.Join(h, "config"), State: filepath.Join(h, "state"), Bin: filepath.Join(h, "bin")}
}

func applied(t *testing.T, d paths.Dirs) []string {
	t.Helper()
	s, err := state.Load(d.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	return s.AppliedMigrations
}

// The shipped registry must stay numbered and in order: Apply runs it in
// that order and an id is never reused.
func TestTheRegistryIsValid(t *testing.T) {
	if err := Validate(All()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesBadRegistries(t *testing.T) {
	noop := func(context.Context, Env) error { return nil }
	for name, list := range map[string][]Migration{
		"unnumbered":   {{ID: "rename-config", Run: noop}},
		"out of order": {{ID: "0002-b", Run: noop}, {ID: "0001-a", Run: noop}},
		"duplicate":    {{ID: "0001-a", Run: noop}, {ID: "0001-a", Run: noop}},
		"same number":  {{ID: "0001-a", Run: noop}, {ID: "0001-b", Run: noop}},
		"no func":      {{ID: "0001-a"}},
	} {
		if err := Validate(list); err == nil {
			t.Errorf("%s: accepted %v", name, list)
		}
	}
}

func TestApplyRunsPendingOnceAndRecordsEach(t *testing.T) {
	d := dirs(t)
	runs := map[string]int{}
	list := []Migration{
		{ID: "0001-first", Run: func(context.Context, Env) error { runs["0001-first"]++; return nil }},
		{ID: "0002-second", Run: func(_ context.Context, e Env) error {
			runs["0002-second"]++
			if e.Dirs != d {
				t.Errorf("env %+v", e)
			}
			return nil
		}},
	}
	// 0001 was applied by an earlier update.
	if err := state.Update(d, "test", func(s *state.State) error { s.AppliedMigrations = []string{"0001-first"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if p := Pending(list, applied(t, d)); len(p) != 1 || p[0].ID != "0002-second" {
		t.Fatalf("pending %v", p)
	}
	ran, err := Apply(context.Background(), Env{Dirs: d}, list)
	if err != nil || !slices.Equal(ran, []string{"0002-second"}) {
		t.Fatalf("ran %v, %v", ran, err)
	}
	if got := applied(t, d); !slices.Equal(got, []string{"0001-first", "0002-second"}) {
		t.Errorf("recorded %v", got)
	}
	// A rerun is a no-op: nothing runs twice, nothing is recorded twice.
	ran, err = Apply(context.Background(), Env{Dirs: d}, list)
	if err != nil || len(ran) != 0 || runs["0001-first"] != 0 || runs["0002-second"] != 1 {
		t.Errorf("rerun ran %v (%v), counts %v", ran, err, runs)
	}
	if got := applied(t, d); len(got) != 2 {
		t.Errorf("recorded twice: %v", got)
	}
}

func TestApplyStopsAtAFailureAndRecordsOnlyWhatPassed(t *testing.T) {
	d := dirs(t)
	boom := errors.New("boom")
	third := false
	list := []Migration{
		{ID: "0001-ok", Run: func(context.Context, Env) error { return nil }},
		{ID: "0002-fails", Run: func(context.Context, Env) error { return boom }},
		{ID: "0003-after", Run: func(context.Context, Env) error { third = true; return nil }},
	}
	ran, err := Apply(context.Background(), Env{Dirs: d}, list)
	if !errors.Is(err, boom) || !slices.Equal(ran, []string{"0001-ok"}) || third {
		t.Fatalf("ran %v, err %v, third ran %v", ran, err, third)
	}
	if got := applied(t, d); !slices.Equal(got, []string{"0001-ok"}) {
		t.Errorf("recorded %v", got)
	}
	if _, err := os.Stat(d.StateFile()); err != nil {
		t.Error(err)
	}
}

// An unknown id in state.json (written by a newer binary) is kept and
// does not make a known migration pending again.
func TestPendingIgnoresUnknownApplied(t *testing.T) {
	list := []Migration{{ID: "0001-a", Run: func(context.Context, Env) error { return nil }}}
	if p := Pending(list, []string{"0001-a", "0007-from-a-newer-binary"}); len(p) != 0 {
		t.Errorf("pending %v", p)
	}
}
