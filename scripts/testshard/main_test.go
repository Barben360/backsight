package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// names is a suite with the awkward cases: a name shorter than a group's
// prefix beside longer ones sharing it, the bare "Test", a non-ASCII
// name, a name in two packages (listed twice), an example and a fuzz
// target, and enough ordinary names to balance.
func names() []string {
	out := []string{
		"Test", "TestA", "TestAb", "TestAbc", "TestAbcd", "TestAbd",
		"TestÉtéÉcrit", "TestÉtéLu", "TestDup", "TestDup",
		"ExampleParse", "FuzzRead",
	}
	for i := range 600 {
		out = append(out, fmt.Sprintf("Test%c%c%cCase%d", 'A'+i%26, 'a'+(i/26)%26, 'a'+i%7, i))
	}
	return out
}

// matching is the names the pattern selects, as go test -run applies it
// to a top-level name.
func matching(t *testing.T, pattern string, all []string) map[string]int {
	t.Helper()
	if strings.Contains(pattern, "/") {
		t.Fatalf("pattern %q has a slash: go test -run would read it as a subtest level", pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("pattern %q does not compile: %v", pattern, err)
	}
	got := map[string]int{}
	for _, n := range all {
		if re.MatchString(n) {
			got[n]++
		}
	}
	return got
}

func TestEveryTestRunsInExactlyOneSlice(t *testing.T) {
	all := names()
	for total := 1; total <= 6; total++ {
		seen := map[string]int{}
		for index := range total {
			p, _, err := slicePattern(all, index, total)
			if err != nil {
				t.Fatalf("slice %d of %d: %v", index, total, err)
			}
			for n := range matching(t, p, all) {
				seen[n]++
			}
		}
		for _, n := range all {
			if seen[n] != 1 {
				t.Errorf("%d slices: %s runs in %d slices, want exactly 1", total, n, seen[n])
			}
		}
	}
}

func TestSlicesAreBalanced(t *testing.T) {
	all := names()
	const total = 4
	want := len(all) / total
	for index := range total {
		_, count, err := slicePattern(all, index, total)
		if err != nil {
			t.Fatal(err)
		}
		// a group (three letters after Test) stays whole: a slice is off by
		// at most a few groups of this suite's size
		if count < want*3/4 || count > want*5/4 {
			t.Errorf("slice %d holds %d tests, want about %d", index, count, want)
		}
	}
}

func TestExamplesAndFuzzTargetsRunInTheFirstSlice(t *testing.T) {
	all := names()
	for index := range 3 {
		p, _, err := slicePattern(all, index, 3)
		if err != nil {
			t.Fatal(err)
		}
		got := matching(t, p, []string{"ExampleParse", "FuzzRead"})
		if (index == 0) != (got["ExampleParse"] == 1 && got["FuzzRead"] == 1) || (index != 0 && len(got) != 0) {
			t.Errorf("slice %d selects %v of the example and fuzz target, want both in slice 0 only", index, got)
		}
	}
}

// A pattern stays short whatever the suite's size: Windows bounds a
// command line to 32767 characters, and each name listed would cost its
// length.
func TestPatternsNameGroupsNotTests(t *testing.T) {
	var all []string
	for i := range 5000 {
		all = append(all, fmt.Sprintf("Test%c%c%cSomethingRatherLongToSpellOut%05d", 'A'+i%26, 'a'+(i/26)%26, 'a'+(i/676)%26, i))
	}
	p, count, err := slicePattern(all, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || len(p) > 2000 {
		t.Errorf("slice 0 of 5000 long names: %d tests, a %d-character pattern; want some tests and a short pattern", count, len(p))
	}
}

func TestRefusals(t *testing.T) {
	for _, c := range []struct {
		name         string
		tests        []string
		index, total int
	}{
		{"no tests listed", nil, 0, 2},
		{"index past the slices", names(), 4, 4},
		{"negative index", names(), -1, 4},
		{"no slices", names(), 0, 0},
		// three groups cannot fill five slices: a slice running nothing
		// would pass while proving nothing
		{"an empty slice", []string{"TestAaa", "TestBbb", "TestCcc"}, 1, 5},
	} {
		if p, _, err := slicePattern(c.tests, c.index, c.total); err == nil {
			t.Errorf("%s: pattern %q, want an error", c.name, p)
		}
	}
}

func TestParseListKeepsOnlyTheNames(t *testing.T) {
	out := "TestOne\nTestTwo\nExampleThree\nok  \tgithub.com/x/y\t0.012s\n" +
		"?   \tgithub.com/x/z\t[no test files]\nBenchmarkFour\nTestFive\nok  \tgithub.com/x/w\t(cached)\n" +
		"some log line from a TestMain\n"
	got := strings.Join(parseList(out), ",")
	if want := "TestOne,TestTwo,ExampleThree,TestFive"; got != want {
		t.Errorf("parseList = %s, want %s", got, want)
	}
}
