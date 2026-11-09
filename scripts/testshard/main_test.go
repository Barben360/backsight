package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

// names is a suite with the awkward cases: a name shorter than a group's
// prefix beside longer ones sharing it, the bare "Test", a non-ASCII
// name, a name in two packages (listed twice), an example and a fuzz
// target, and enough ordinary names to balance.
func names() []listed {
	out := []listed{
		{"a", "Test"}, {"a", "TestA"}, {"a", "TestAb"}, {"a", "TestAbc"}, {"a", "TestAbcd"}, {"a", "TestAbd"},
		{"a", "TestÉtéÉcrit"}, {"a", "TestÉtéLu"}, {"a", "TestDup"}, {"b", "TestDup"},
		{"a", "ExampleParse"}, {"b", "FuzzRead"},
	}
	for i := range 600 {
		out = append(out, listed{fmt.Sprintf("p%d", i%3), fmt.Sprintf("Test%c%c%cCase%d", 'A'+i%26, 'a'+(i/26)%26, 'a'+i%7, i)})
	}
	return out
}

// timesOf gives the tests of all varied times, some left out (they weigh
// the floor), the same every run.
func timesOf(all []listed) durations {
	d := durations{}
	for i, t := range all {
		if i%5 != 0 {
			d[t] = float64(i%17) * 0.7
		}
	}
	return d
}

// matching is the tests the pattern selects, as go test -run applies it
// to a top-level name.
func matching(t *testing.T, pattern string, all []listed) map[listed]int {
	t.Helper()
	if strings.Contains(pattern, "/") {
		t.Fatalf("pattern %q has a slash: go test -run would read it as a subtest level", pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("pattern %q does not compile: %v", pattern, err)
	}
	got := map[listed]int{}
	for _, n := range all {
		if re.MatchString(n.name) {
			got[n]++
		}
	}
	return got
}

// lengthOf is how long the tests a pattern selects last by d, computed
// apart from byTime: the longest package's time or the whole time over
// parallel, whichever is more.
func lengthOf(t *testing.T, pattern string, all []listed, d durations) float64 {
	t.Helper()
	pkgs := map[string]float64{}
	sum, longest := 0.0, 0.0
	for n := range matching(t, pattern, all) {
		if !strings.HasPrefix(n.name, "Test") {
			continue
		}
		w := d.weight(n)
		pkgs[n.pkg] += w
		sum += w
		longest = max(longest, pkgs[n.pkg])
	}
	return max(longest, sum/parallel)
}

func TestEveryTestRunsInExactlyOneSlice(t *testing.T) {
	all := names()
	for _, d := range []durations{nil, timesOf(all)} {
		for total := 1; total <= 6; total++ {
			seen := map[listed]int{}
			for index := range total {
				s, err := slicePattern(all, index, total, d)
				if err != nil {
					t.Fatalf("by time %v, slice %d of %d: %v", d != nil, index, total, err)
				}
				for n := range matching(t, s.pattern, all) {
					seen[n]++
				}
			}
			for _, n := range all {
				if seen[n] != 1 {
					t.Errorf("by time %v, %d slices: %v runs in %d slices, want exactly 1", d != nil, total, n, seen[n])
				}
			}
		}
	}
}

func TestSlicesAreBalanced(t *testing.T) {
	all := names()
	const total = 4
	want := len(all) / total
	for index := range total {
		s, err := slicePattern(all, index, total, nil)
		if err != nil {
			t.Fatal(err)
		}
		// a group (three letters after Test) stays whole: a slice is off by
		// at most a few groups of this suite's size
		if s.count < want*3/4 || s.count > want*5/4 {
			t.Errorf("slice %d holds %d tests, want about %d", index, s.count, want)
		}
	}
}

// By time, the slices last about as long as each other even when the
// slow tests sort together, which a cut by count leaves in one slice: a
// slow package's groups spread over the slices, the fast ones fill in.
func TestTimedSlicesLastAboutAsLong(t *testing.T) {
	var all []listed
	d := durations{}
	for i := range 80 { // TestAa… to TestAd…, 10 s each, all in package slow
		n := listed{"slow", fmt.Sprintf("TestA%c%cSlow", 'a'+i%4, 'a'+i/4)}
		all = append(all, n)
		d[n] = 10
	}
	for i := range 800 { // the rest, fast, in four packages
		all = append(all, listed{fmt.Sprintf("fast%d", i%4), fmt.Sprintf("Test%c%c%cFast%d", 'B'+i%24, 'a'+(i/24)%26, 'a'+i%5, i)})
	}
	const total = 4
	var lengths []float64
	for index := range total {
		s, err := slicePattern(all, index, total, d)
		if err != nil {
			t.Fatal(err)
		}
		l := lengthOf(t, s.pattern, all, d)
		if math.Abs(l-s.seconds) > 1e-6 {
			t.Errorf("slice %d reports %.2f s, its tests last %.2f s", index, s.seconds, l)
		}
		lengths = append(lengths, l)
	}
	// 800 s of package slow over 4 slices, 200 s each at best; the fast
	// tests weigh the floor
	for index, l := range lengths {
		if l < 180 || l > 230 {
			t.Errorf("slice %d lasts %.0f s (all: %.0f), want about 200", index, l, lengths)
		}
	}
	// the same suite cut by count: package slow's groups in one slice
	worst := 0.0
	for index := range total {
		s, err := slicePattern(all, index, total, nil)
		if err != nil {
			t.Fatal(err)
		}
		worst = max(worst, lengthOf(t, s.pattern, all, d))
	}
	if worst < 400 {
		t.Fatalf("by count the longest slice lasts %.0f s: the suite does not tell a cut by time from one by count", worst)
	}
}

// Every slice deals the groups out alike: the patterns of two runs on the
// same listing and durations are the same.
func TestTimedSlicesAreTheSameEveryRun(t *testing.T) {
	all := names()
	d := timesOf(all)
	for index := range 4 {
		a, err := slicePattern(all, index, 4, d)
		if err != nil {
			t.Fatal(err)
		}
		for range 20 {
			if b, _ := slicePattern(all, index, 4, d); b != a {
				t.Fatalf("slice %d: %+v, then %+v", index, a, b)
			}
		}
	}
}

func TestExamplesAndFuzzTargetsRunInTheFirstSlice(t *testing.T) {
	all := names()
	ex := []listed{{"a", "ExampleParse"}, {"b", "FuzzRead"}}
	for _, d := range []durations{nil, timesOf(all)} {
		for index := range 3 {
			s, err := slicePattern(all, index, 3, d)
			if err != nil {
				t.Fatal(err)
			}
			got := matching(t, s.pattern, ex)
			if (index == 0) != (got[ex[0]] == 1 && got[ex[1]] == 1) || (index != 0 && len(got) != 0) {
				t.Errorf("by time %v, slice %d selects %v of the example and fuzz target, want both in slice 0 only", d != nil, index, got)
			}
		}
	}
}

// A pattern stays short whatever the suite's size: Windows bounds a
// command line to 32767 characters, and each name listed would cost its
// length.
func TestPatternsNameGroupsNotTests(t *testing.T) {
	var all []listed
	for i := range 5000 {
		all = append(all, listed{"p", fmt.Sprintf("Test%c%c%cSomethingRatherLongToSpellOut%05d", 'A'+i%26, 'a'+(i/26)%26, 'a'+(i/676)%26, i)})
	}
	for _, d := range []durations{nil, timesOf(all)} {
		s, err := slicePattern(all, 0, 2, d)
		if err != nil {
			t.Fatal(err)
		}
		if s.count == 0 || len(s.pattern) > maxPattern {
			t.Errorf("by time %v, slice 0 of 5000 long names: %d tests, a %d-character pattern; want some tests and a pattern of %d at most", d != nil, s.count, len(s.pattern), maxPattern)
		}
	}
}

func TestRefusals(t *testing.T) {
	three := []listed{{"p", "TestAaa"}, {"p", "TestBbb"}, {"p", "TestCcc"}}
	for _, c := range []struct {
		name         string
		tests        []listed
		index, total int
	}{
		{"no tests listed", nil, 0, 2},
		{"index past the slices", names(), 4, 4},
		{"negative index", names(), -1, 4},
		{"no slices", names(), 0, 0},
		// three groups cannot fill five slices: a slice running nothing
		// would pass while proving nothing
		{"an empty slice", three, 1, 5},
	} {
		for _, d := range []durations{nil, timesOf(c.tests)} {
			if s, err := slicePattern(c.tests, c.index, c.total, d); err == nil {
				t.Errorf("%s, by time %v: pattern %q, want an error", c.name, d != nil, s.pattern)
			}
		}
	}
}

func TestParseListKeepsTheNamesAndTheirPackage(t *testing.T) {
	out := "TestOne\nTestTwo\nExampleThree\nok  \tgithub.com/x/y\t0.012s\n" +
		"?   \tgithub.com/x/z\t[no test files]\nBenchmarkFour\nTestFive\nok  \tgithub.com/x/w\t(cached)\n" +
		"some log line from a TestMain\n"
	got := fmt.Sprint(parseList(out))
	if want := "[{github.com/x/y TestOne} {github.com/x/y TestTwo} {github.com/x/y ExampleThree} {github.com/x/w TestFive}]"; got != want {
		t.Errorf("parseList = %s, want %s", got, want)
	}
}

// The recorded file keeps the top-level tests that passed and took more
// than the floor, and reads back to their times.
func TestRecordedDurationsReadBack(t *testing.T) {
	in := strings.Join([]string{
		`{"Action":"run","Package":"m/a","Test":"TestSlow"}`,
		`{"Action":"pass","Package":"m/a","Test":"TestSlow/sub","Elapsed":3}`,
		`{"Action":"pass","Package":"m/a","Test":"TestSlow","Elapsed":3.456}`,
		`{"Action":"pass","Package":"m/b","Test":"TestOther","Elapsed":1.5}`,
		`{"Action":"skip","Package":"m/b","Test":"TestSkipped","Elapsed":9}`,
		`{"Action":"pass","Package":"m/b","Test":"TestQuick","Elapsed":0.01}`,
		`{"Action":"pass","Package":"m/b","Test":"ExampleX","Elapsed":2}`,
		`{"Action":"pass","Package":"m/b","Elapsed":20}`,
		`# a build line, not JSON`,
		`{"Action":"pass","Package":"m/a","Test":"TestAlso","Elapsed":0.2}`,
	}, "\n")
	var out strings.Builder
	if err := recordDurations(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`(?m)^#.*\n`).ReplaceAllString(out.String(), "")
	if want := "0.20 m/a TestAlso\n3.46 m/a TestSlow\n1.50 m/b TestOther\n"; body != want {
		t.Errorf("recorded:\n%s\nwant:\n%s", body, want)
	}
	d, err := readDurations(strings.NewReader(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(d); got != "map[{m/a TestAlso}:0.2 {m/a TestSlow}:3.46 {m/b TestOther}:1.5]" {
		t.Errorf("read back %s", got)
	}
	if w := d.weight(listed{"m/b", "TestQuick"}); w != floor {
		t.Errorf("an unrecorded test weighs %g, want the floor %g", w, floor)
	}
	for what, bad := range map[string]string{
		"no test over the floor": `{"Action":"pass","Package":"m","Test":"TestQuick","Elapsed":0.01}`,
		"a failed test":          in + "\n" + `{"Action":"fail","Package":"m/b","Test":"TestFailed","Elapsed":1.5}`,
		"a failed package":       in + "\n" + `{"Action":"fail","Package":"m/c","Elapsed":0}`,
		"a failed build":         `{"ImportPath":"m/d","Action":"build-fail"}` + "\n" + in,
	} {
		var w strings.Builder
		err := recordDurations(strings.NewReader(bad), &w)
		if err == nil {
			t.Errorf("recorded a file from input holding %s:\n%s", what, w.String())
			continue
		}
		// the refusal names what failed
		if want, ok := map[string]string{"a failed test": "m/b TestFailed", "a failed package": "m/c", "a failed build": "m/d"}[what]; ok && !strings.Contains(err.Error(), "("+want+")") {
			t.Errorf("input holding %s refused with %q, want it to name (%s)", what, err, want)
		}
	}
}

func TestDurationsRefusals(t *testing.T) {
	for _, in := range []string{
		"",
		"# only a comment\n",
		"1.0 m/a\n",
		"1.0 m/a TestX extra\n",
		"fast m/a TestX\n",
		"-1 m/a TestX\n",
	} {
		if d, err := readDurations(strings.NewReader(in)); err == nil {
			t.Errorf("readDurations(%q) = %v, want an error", in, d)
		}
	}
}

// The committed durations (make test-durations) read, cover the suite's
// slow packages, and name this module's packages.
func TestTheCommittedDurationsRead(t *testing.T) {
	b, err := os.ReadFile("durations.txt")
	if err != nil {
		t.Fatal(err)
	}
	d, err := readDurations(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	pkgs := map[string]bool{}
	for k := range d {
		if !strings.HasPrefix(k.pkg, "github.com/Barben360/backsight/") {
			t.Errorf("durations.txt names %v, outside the module", k)
		}
		pkgs[strings.TrimPrefix(k.pkg, "github.com/Barben360/backsight/")] = true
	}
	for _, p := range []string{"internal/cli", "internal/dashboard", "internal/metrics", "internal/index", "scripts"} {
		if !pkgs[p] {
			t.Errorf("durations.txt has no test of %s", p)
		}
	}
}

// report prints what go test prints without -json: a package's own lines
// as they come, a failed test's output (its subtests' included), a test
// that never ended in a failed package, a build's errors and any line
// that is not an event; a passing or skipped test's output is dropped.
// The events go to raw unchanged.
func TestReportPrintsWhatGoTestWould(t *testing.T) {
	in := strings.Join([]string{
		`{"Action":"start","Package":"m/a"}`,
		`{"Action":"run","Package":"m/a","Test":"TestOK"}`,
		`{"Action":"output","Package":"m/a","Test":"TestOK","Output":"=== RUN   TestOK\n"}`,
		`{"Action":"output","Package":"m/a","Test":"TestOK","Output":"--- PASS: TestOK (0.00s)\n"}`,
		`{"Action":"pass","Package":"m/a","Test":"TestOK","Elapsed":0}`,
		`{"Action":"output","Package":"m/a","Test":"TestBad","Output":"=== RUN   TestBad\n"}`,
		`{"Action":"output","Package":"m/a","Test":"TestBad/sub","Output":"    bad_test.go:9: want 2, got 3\n"}`,
		`{"Action":"fail","Package":"m/a","Test":"TestBad/sub","Elapsed":0}`,
		`{"Action":"output","Package":"m/a","Test":"TestSkip","Output":"--- SKIP: TestSkip\n"}`,
		`{"Action":"skip","Package":"m/a","Test":"TestSkip","Elapsed":0}`,
		`{"Action":"output","Package":"m/a","Test":"TestBad","Output":"--- FAIL: TestBad (0.01s)\n"}`,
		`{"Action":"fail","Package":"m/a","Test":"TestBad","Elapsed":0.01}`,
		`{"Action":"output","Package":"m/a","Output":"FAIL\tm/a\t0.02s\n"}`,
		`{"Action":"fail","Package":"m/a","Elapsed":0.02}`,
		`{"Action":"output","Package":"m/b","Test":"TestHang","Output":"panic: test timed out after 20m0s\n"}`,
		`{"Action":"output","Package":"m/b","Output":"FAIL\tm/b\t1200.00s\n"}`,
		`{"Action":"fail","Package":"m/b","Elapsed":1200}`,
		`{"ImportPath":"m/c","Action":"build-output","Output":"c.go:3:1: syntax error\n"}`,
		`{"Action":"output","Package":"m/d","Output":"PASS\n"}`,
		`{"Action":"output","Package":"m/d","Output":"ok  \tm/d\t0.50s\n"}`,
		`go: downloading example.com/x v1.0.0`,
	}, "\n")
	var raw, out strings.Builder
	if err := report(strings.NewReader(in), &raw, &out); err != nil {
		t.Fatal(err)
	}
	// a test's held lines print in their order of arrival
	want := "=== RUN   TestBad\n    bad_test.go:9: want 2, got 3\n--- FAIL: TestBad (0.01s)\n" +
		"FAIL\tm/a\t0.02s\n" +
		"FAIL\tm/b\t1200.00s\n" + "panic: test timed out after 20m0s\n" +
		"c.go:3:1: syntax error\n" +
		"ok  \tm/d\t0.50s\n" +
		"go: downloading example.com/x v1.0.0\n"
	if out.String() != want {
		t.Errorf("printed:\n%s\nwant:\n%s", out.String(), want)
	}
	if raw.String() != in+"\n" {
		t.Errorf("raw differs from the input:\n%s", raw.String())
	}
}
