// Command testshard prints the go test -run pattern of one slice of a
// test suite: CI runs the suite in several jobs at once, each running its
// slice (ci.yml test-race and test-windows, make test-shard).
//
//	testshard -index <i> -total <n> [-durations <file>] -- [go test flags] <packages>
//	testshard -record < go-test.json > durations.txt
//	go test -json ... | testshard -report [<file>]
//
// It lists the tests with go test -list, given the flags and packages the
// run will use (-race, -tags: the build the run then reuses), and groups
// the top-level tests by the three characters after "Test". Without
// -durations it cuts the sorted groups into n runs of about as many tests
// each. With -durations (make test-durations records the file) it deals
// the groups out by time, the heaviest first, each to the slice it
// lengthens least: a slice lasts about as long as its longest package
// (go test runs the packages at once, the tests of one in sequence) or
// its whole time over parallel, whichever is more. Every test
// falls in exactly one slice, whatever package holds it; examples and fuzz
// targets go to slice 0. The pattern names groups, not tests, so it stays
// a few hundred characters long (Windows bounds a command line to 32767).
// A slice that would run nothing is an error, as is a listing that fails.
//
// -report reads go test -json (make test-shard runs its slice so, for the
// times CI records) and prints what go test would print without it, the
// JSON kept in the file given: make test-durations reads CI's.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

func main() {
	index := flag.Int("index", -1, "the slice to print, from 0")
	total := flag.Int("total", 0, "how many slices")
	file := flag.String("durations", "", "balance the slices by the test durations of this file (make test-durations), not by count")
	record := flag.Bool("record", false, "read go test -json on stdin and print its durations file")
	rep := flag.Bool("report", false, "read go test -json on stdin, print it as go test would without -json, and keep it in the file given as argument, if any")
	flag.Parse()
	if *rep {
		if err := reportTo(flag.Arg(0)); err != nil {
			fmt.Fprintln(os.Stderr, "testshard:", err)
			os.Exit(1)
		}
		return
	}
	if *record {
		if err := recordDurations(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "testshard:", err)
			os.Exit(1)
		}
		return
	}
	var d durations
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err == nil {
			d, err = readDurations(bytes.NewReader(b))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "testshard:", err)
			os.Exit(1)
		}
	}
	args := append([]string{"test", "-list", "."}, flag.Args()...)
	var out bytes.Buffer
	cmd := exec.Command("go", args...)
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprint(os.Stderr, out.String())
		fmt.Fprintf(os.Stderr, "testshard: go %s: %v\n", strings.Join(args, " "), err)
		os.Exit(1)
	}
	tests := parseList(out.String())
	s, err := slicePattern(tests, *index, *total, d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testshard:", err)
		os.Exit(1)
	}
	if d != nil {
		fmt.Fprintf(os.Stderr, "testshard: slice %d of %d runs %d of %d tests, about %.0f s by the durations\n", *index, *total, s.count, len(tests), s.seconds)
	} else {
		fmt.Fprintf(os.Stderr, "testshard: slice %d of %d runs %d of %d tests\n", *index, *total, s.count, len(tests))
	}
	fmt.Println(s.pattern)
}

// listed is a test, example or fuzz target of go test -list, and the
// package holding it.
type listed struct{ pkg, name string }

// parseList keeps the test, example and fuzz names of go test -list's
// output, one per package that holds them, each with the package of the
// ok line that follows its names, and drops its ? lines and anything a
// TestMain printed.
func parseList(out string) []listed {
	var tests []listed
	from := 0 // the first name not yet given its package
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "ok" {
			for i := from; i < len(tests); i++ {
				tests[i].pkg = f[1]
			}
			from = len(tests)
			continue
		}
		if line == "" || strings.IndexFunc(line, unicode.IsSpace) >= 0 {
			continue
		}
		if strings.HasPrefix(line, "Test") || strings.HasPrefix(line, "Example") || strings.HasPrefix(line, "Fuzz") {
			tests = append(tests, listed{name: line})
		}
	}
	return tests
}

// durations is each recorded test's time, in seconds.
type durations map[listed]float64

// floor is the time of a test the durations do not list: one under it
// when recorded (recordDurations leaves those out), or one added since.
const floor = 0.05

// parallel is how many tests' worth of time a slice runs at once, as
// byTime counts it: below the hosted Linux runner's 4 vCPUs (go test's
// -p there), which the race detector and the builds share. Measured on
// the 2026-11-09 run of main cut by count, a slice's time followed its
// whole time as much as its longest package's; over 3, the slices of the
// durations keep both their longest package (130 s here) and their whole
// time (391 s at most of 1464) near the least they can be, where 4 let
// one slice's whole time reach 489 s and 2 its longest package 156 s.
const parallel = 3

// weight is a test's time by d, floor for one d does not list.
func (d durations) weight(t listed) float64 {
	if w, ok := d[t]; ok && w > floor {
		return w
	}
	return floor
}

// readDurations reads a durations file: "<seconds> <package> <test>"
// lines, # comments and blank lines left out.
func readDurations(r io.Reader) (durations, error) {
	d := durations{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("durations line %d: %q, want <seconds> <package> <test>", n, line)
		}
		s, err := strconv.ParseFloat(f[0], 64)
		if err != nil || s < 0 {
			return nil, fmt.Errorf("durations line %d: %q is not a time in seconds", n, f[0])
		}
		d[listed{pkg: f[1], name: f[2]}] = s
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(d) == 0 {
		return nil, errors.New("the durations file lists no tests")
	}
	return d, nil
}

// recordDurations reads go test -json events and writes the durations
// file of its top-level tests that passed, those over floor, sorted by
// package and name. A failure anywhere (a test, a package, its build)
// refuses the input: a run that failed may have stopped short, and make
// test-durations pipes go test into it, so its status is lost.
func recordDurations(r io.Reader, w io.Writer) error {
	d := durations{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var e struct {
			Action, Package, ImportPath, Test string
			Elapsed                           float64
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Action == "fail" || e.Action == "build-fail" {
			at := strings.TrimSpace(e.Package + " " + e.Test)
			if at == "" {
				at = e.ImportPath // a build-fail event's package
			}
			return fmt.Errorf("go test failed (%s): nothing recorded", at)
		}
		if e.Action != "pass" {
			continue
		}
		if !strings.HasPrefix(e.Test, "Test") || strings.Contains(e.Test, "/") || e.Package == "" {
			continue
		}
		t := listed{pkg: e.Package, name: e.Test}
		d[t] = max(d[t], e.Elapsed)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	var keys []listed
	for t, s := range d {
		if s > floor {
			keys = append(keys, t)
		}
	}
	if len(keys) == 0 {
		return errors.New("the go test -json input holds no test over the floor")
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pkg != keys[j].pkg {
			return keys[i].pkg < keys[j].pkg
		}
		return keys[i].name < keys[j].name
	})
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# The top-level tests' times over %g s, in seconds (make test-durations,\n# scripts/testshard): the slices of make test-shard are balanced by them.\n", floor)
	for _, t := range keys {
		fmt.Fprintf(bw, "%.2f %s %s\n", d[t], t.pkg, t.name)
	}
	return bw.Flush()
}

// reportTo runs report on stdin and stdout, the JSON kept in file when
// one is given.
func reportTo(file string) error {
	if file == "" {
		return report(os.Stdin, io.Discard, os.Stdout)
	}
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	err = report(os.Stdin, f, os.Stdout)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// report copies go test -json events from r to raw and prints to out what
// go test prints without -json: each package's own lines (ok, FAIL, a
// build's errors) as they come, and the output of each top-level test
// that fails, or that never ends in a package that fails (a panic, a
// timeout). A line that is not an event is printed as it is.
func report(r io.Reader, raw, out io.Writer) error {
	type key struct{ pkg, test string }
	held := map[key][]string{}
	var order []key // held tests in their first output's order
	flush := func(k key) error {
		for _, l := range held[k] {
			if _, err := io.WriteString(out, l); err != nil {
				return err
			}
		}
		delete(held, k)
		return nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Bytes()
		if _, err := raw.Write(append(line, '\n')); err != nil {
			return err
		}
		var e struct {
			Action, Package, ImportPath, Test, Output string
		}
		if json.Unmarshal(line, &e) != nil || e.Action == "" {
			if _, err := fmt.Fprintf(out, "%s\n", line); err != nil {
				return err
			}
			continue
		}
		top, _, _ := strings.Cut(e.Test, "/")
		k := key{e.Package, top}
		switch {
		case e.Action == "output" && e.Test != "":
			if _, ok := held[k]; !ok {
				order = append(order, k)
			}
			held[k] = append(held[k], e.Output)
		case e.Action == "output" && e.Output == "PASS\n":
			// the test binary's own PASS, which go test prints only with -v
		case e.Action == "output" || e.Action == "build-output":
			if _, err := io.WriteString(out, e.Output); err != nil {
				return err
			}
		case e.Test != "" && e.Test == top && e.Action == "fail":
			if err := flush(k); err != nil {
				return err
			}
		case e.Test != "" && e.Test == top && (e.Action == "pass" || e.Action == "skip"):
			delete(held, k)
		case e.Test == "" && e.Action == "fail":
			// the package failed: its tests that never ended
			for _, h := range order {
				if _, ok := held[h]; ok && h.pkg == e.Package {
					if err := flush(h); err != nil {
						return err
					}
				}
			}
		}
	}
	return sc.Err()
}

// maxPattern bounds a slice's pattern: far below Windows' 32767
// characters for a whole command line.
const maxPattern = 4000

// groupOf is a test's group: the width characters after "Test", or the
// whole rest and "$" when it is shorter, so TestAb is not in TestAbc's
// group. The bare "Test" is the group "$".
func groupOf(name string, width int) string {
	rest := []rune(strings.TrimPrefix(name, "Test"))
	if len(rest) >= width {
		return string(rest[:width])
	}
	return string(rest) + "$"
}

// slice is one slice's -run pattern, how many tests it runs (one entry
// per package holding the name), and with durations about how long it
// lasts.
type slice struct {
	pattern string
	count   int
	seconds float64
}

// slicePattern is slice index of total, by count or, with d, by time.
// The groups are three characters wide, or narrower when a slice's
// pattern would pass maxPattern: every slice makes the same choice from
// the same list.
func slicePattern(tests []listed, index, total int, d durations) (slice, error) {
	if total < 1 || index < 0 || index >= total {
		return slice{}, fmt.Errorf("slice %d of %d: want 0 <= index < total", index, total)
	}
	for width := 3; ; width-- {
		all, err := slices(tests, total, width, d)
		if err != nil {
			return slice{}, err
		}
		longest := 0
		for _, s := range all {
			longest = max(longest, len(s.pattern))
		}
		if longest <= maxPattern || width == 1 {
			return all[index], nil
		}
	}
}

// slices cuts tests into total slices of groups width characters wide.
func slices(tests []listed, total, width int, d durations) ([]slice, error) {
	weight := map[string]int{}
	others := 0 // examples and fuzz targets, slice 0's
	for _, t := range tests {
		if strings.HasPrefix(t.name, "Test") {
			weight[groupOf(t.name, width)]++
		} else {
			others++
		}
	}
	if len(weight) == 0 {
		return nil, errors.New("go test -list listed no tests")
	}
	groups := make([]string, 0, len(weight))
	for g := range weight {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	var at map[string]int
	var seconds []float64
	if d == nil {
		at = byCount(groups, weight, total)
	} else {
		at, seconds = byTime(tests, total, width, d)
	}
	alts := make([][]string, total)
	out := make([]slice, total)
	for _, g := range groups {
		s := at[g]
		out[s].count += weight[g]
		if strings.HasSuffix(g, "$") {
			alts[s] = append(alts[s], regexp.QuoteMeta(strings.TrimSuffix(g, "$"))+"$")
		} else {
			alts[s] = append(alts[s], regexp.QuoteMeta(g))
		}
	}
	for s := range total {
		if len(alts[s]) == 0 {
			return nil, fmt.Errorf("slice %d of %d holds no tests (%d groups of tests): fewer slices", s, total, len(groups))
		}
		out[s].pattern = "^Test(?:" + strings.Join(alts[s], "|") + ")"
		if seconds != nil {
			out[s].seconds = seconds[s]
		}
	}
	out[0].pattern = "^(?:Example|Fuzz)|" + out[0].pattern
	out[0].count += others
	return out, nil
}

// byCount puts each group in the slice its middle falls in: the slices
// are runs of the sorted groups, of about as many tests each.
func byCount(groups []string, weight map[string]int, total int) map[string]int {
	sum := 0
	for _, w := range weight {
		sum += w
	}
	at := map[string]int{}
	before := 0
	for _, g := range groups {
		w := weight[g]
		at[g] = min((2*before+w)*total/(2*sum), total-1)
		before += w
	}
	return at
}

// byTime deals the groups out by d's times, the heaviest first, each to
// the slice it lengthens least (ties: the slice with less time, then the
// first): a slice lasts its longest package's time or its whole time
// over parallel, whichever is more. It returns each group's slice and each
// slice's length.
func byTime(tests []listed, total, width int, d durations) (map[string]int, []float64) {
	type group struct {
		name string
		sum  float64
		pkgs map[string]float64
	}
	byName := map[string]*group{}
	for _, t := range tests {
		if !strings.HasPrefix(t.name, "Test") {
			continue
		}
		n := groupOf(t.name, width)
		g := byName[n]
		if g == nil {
			g = &group{name: n, pkgs: map[string]float64{}}
			byName[n] = g
		}
		w := d.weight(t)
		g.sum += w
		g.pkgs[t.pkg] += w
	}
	order := make([]*group, 0, len(byName))
	for _, g := range byName {
		order = append(order, g)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].sum != order[j].sum {
			return order[i].sum > order[j].sum
		}
		return order[i].name < order[j].name
	})
	sums := make([]float64, total)
	longest := make([]float64, total) // each slice's longest package
	pkgs := make([]map[string]float64, total)
	for s := range pkgs {
		pkgs[s] = map[string]float64{}
	}
	length := func(s int, g *group) float64 {
		l := max(longest[s], (sums[s]+g.sum)/parallel)
		for p, w := range g.pkgs {
			l = max(l, pkgs[s][p]+w)
		}
		return l
	}
	at := map[string]int{}
	for _, g := range order {
		best := 0
		for s := 1; s < total; s++ {
			if ls, lb := length(s, g), length(best, g); ls < lb || (ls == lb && sums[s] < sums[best]) {
				best = s
			}
		}
		at[g.name] = best
		sums[best] += g.sum
		for p, w := range g.pkgs {
			pkgs[best][p] += w
			longest[best] = max(longest[best], pkgs[best][p])
		}
	}
	out := make([]float64, total)
	for s := range total {
		out[s] = max(longest[s], sums[s]/parallel)
	}
	return at, out
}
