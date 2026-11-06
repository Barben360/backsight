package indexgen

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/machines"
)

var machinesOut = flag.String("out", "", "TestWriteMachinesCache: the directory to write machines.json to")

// TestWriteMachinesCache writes the synthetic cache of four other PCs over
// the Budget's months to -out/machines.json (the metrics directory of a
// state a bench runs on); skipped without -out.
func TestWriteMachinesCache(t *testing.T) {
	if *machinesOut == "" {
		t.Skip("no -out directory")
	}
	s := Budget
	s.OtherPCs = 4
	if err := os.MkdirAll(*machinesOut, 0o700); err != nil {
		t.Fatal(err)
	}
	WriteMachines(t, *machinesOut, s)
	if _, err := machines.LoadCache(*machinesOut); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetHasNoOtherPCs(t *testing.T) {
	if Budget.OtherPCs != 0 {
		t.Errorf("Budget.OtherPCs = %d", Budget.OtherPCs)
	}
	dir := t.TempDir()
	Generate(t, dir, Spec{Months: 1, TelemetryPerYear: 12, JournalPerYear: 12, Memories: 2, Seed: 1})
	if _, err := os.Stat(filepath.Join(dir, "metrics", "machines.json")); !os.IsNotExist(err) {
		t.Errorf("a state without other PCs holds machines.json (%v)", err)
	}
}

// The cache of n PCs: n documents that all parse at Now, over the spec's
// months, the last stale, plus one ignored entry; the same for the same seed.
func TestMachinesCacheOfOtherPCs(t *testing.T) {
	s := Spec{Months: 3, TelemetryPerYear: 12, JournalPerYear: 12, Memories: 2, Seed: 5, OtherPCs: 4}
	dir := t.TempDir()
	Generate(t, dir, s)
	c, err := machines.LoadCache(filepath.Join(dir, "metrics"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Docs) != 4 || len(c.Ignored) != 1 {
		t.Fatalf("%d documents, %d ignored; want 4 and 1", len(c.Docs), len(c.Ignored))
	}
	if !c.FetchedAt.Before(Now) || Now.Sub(c.FetchedAt) > 6*time.Hour {
		t.Errorf("fetched_at %v", c.FetchedAt)
	}
	first := monthSpan(s, End())[0][0].Format("2006-01-02")
	stale, seen := 0, map[string]bool{}
	for _, cd := range c.Docs {
		d, err := machines.Parse(cd.Name, cd.Raw, Now)
		if err != nil {
			t.Fatalf("%s: %v", cd.Name, err)
		}
		if seen[d.Machine] {
			t.Errorf("machine %s twice", d.Machine)
		}
		seen[d.Machine] = true
		if Now.Sub(d.At) > machines.StaleAfter {
			stale++
		}
		if len(d.Days) < 80 || d.Days[0].Day != first {
			t.Errorf("%s: %d days from %s, want the spec's months from %s", d.Machine, len(d.Days), d.Days[0].Day, first)
		}
		versions := 0
		for _, day := range d.Days {
			versions += len(day.Versions)
		}
		if versions == 0 {
			t.Errorf("%s: no version rows", d.Machine)
		}
	}
	if stale != 1 {
		t.Errorf("%d stale documents, want 1", stale)
	}
	if c.Ignored[0].Machine == "" || seen[c.Ignored[0].Machine] {
		t.Errorf("ignored entry %+v", c.Ignored[0])
	}

	again := t.TempDir()
	Generate(t, again, s)
	c2, err := machines.LoadCache(filepath.Join(again, "metrics"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Docs) != 4 || c2.Docs[0] != c.Docs[0] || c2.Docs[3] != c.Docs[3] {
		t.Error("the same seed gives another cache")
	}
}
