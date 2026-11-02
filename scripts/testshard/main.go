// Command testshard prints the go test -run pattern of one slice of a
// test suite: CI runs the suite in several jobs at once, each running its
// slice (ci.yml test-race and test-windows, make test-shard).
//
//	testshard -index <i> -total <n> -- [go test flags] <packages>
//
// It lists the tests with go test -list, given the flags and packages the
// run will use (-race, -tags: the build the run then reuses), groups the
// top-level tests by the three characters after "Test", and cuts the
// sorted groups into n runs of about as many tests each. Every test falls
// in exactly one slice, whatever package holds it; examples and fuzz
// targets go to slice 0. The pattern names groups, not tests, so it stays
// a few hundred characters long (Windows bounds a command line to 32767).
// A slice that would run nothing is an error, as is a listing that fails.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

func main() {
	index := flag.Int("index", -1, "the slice to print, from 0")
	total := flag.Int("total", 0, "how many slices")
	flag.Parse()
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
	pattern, count, err := slicePattern(tests, *index, *total)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testshard:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "testshard: slice %d of %d runs %d of %d tests\n", *index, *total, count, len(tests))
	fmt.Println(pattern)
}

// parseList keeps the test, example and fuzz names of go test -list's
// output, one per package that holds them, and drops its ok and ? lines
// and anything a TestMain printed.
func parseList(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.IndexFunc(line, unicode.IsSpace) >= 0 {
			continue
		}
		if strings.HasPrefix(line, "Test") || strings.HasPrefix(line, "Example") || strings.HasPrefix(line, "Fuzz") {
			names = append(names, line)
		}
	}
	return names
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

// slicePattern is the -run pattern of slice index of total, and how many
// of tests (one entry per package holding the name) it runs. The groups
// are three characters wide, or narrower when a slice's pattern would
// pass maxPattern: every slice makes the same choice from the same list.
func slicePattern(tests []string, index, total int) (string, int, error) {
	if total < 1 || index < 0 || index >= total {
		return "", 0, fmt.Errorf("slice %d of %d: want 0 <= index < total", index, total)
	}
	for width := 3; ; width-- {
		patterns, counts, err := slices(tests, total, width)
		if err != nil {
			return "", 0, err
		}
		longest := 0
		for _, p := range patterns {
			longest = max(longest, len(p))
		}
		if longest <= maxPattern || width == 1 {
			return patterns[index], counts[index], nil
		}
	}
}

// slices cuts tests into total slices of groups width characters wide:
// each slice's pattern and how many tests it runs.
func slices(tests []string, total, width int) ([]string, []int, error) {
	weight := map[string]int{}
	others := 0 // examples and fuzz targets, slice 0's
	for _, n := range tests {
		if strings.HasPrefix(n, "Test") {
			weight[groupOf(n, width)]++
		} else {
			others++
		}
	}
	if len(weight) == 0 {
		return nil, nil, errors.New("go test -list listed no tests")
	}
	groups := make([]string, 0, len(weight))
	sum := 0
	for g, w := range weight {
		groups = append(groups, g)
		sum += w
	}
	sort.Strings(groups)
	// each group goes to the slice its middle falls in: the slices are
	// runs of sorted groups, of about sum/total tests each
	alts := make([][]string, total)
	counts := make([]int, total)
	before := 0
	for _, g := range groups {
		w := weight[g]
		s := min((2*before+w)*total/(2*sum), total-1)
		before += w
		counts[s] += w
		if strings.HasSuffix(g, "$") {
			alts[s] = append(alts[s], regexp.QuoteMeta(strings.TrimSuffix(g, "$"))+"$")
		} else {
			alts[s] = append(alts[s], regexp.QuoteMeta(g))
		}
	}
	patterns := make([]string, total)
	for s := range total {
		if len(alts[s]) == 0 {
			return nil, nil, fmt.Errorf("slice %d of %d holds no tests (%d groups of tests): fewer slices", s, total, len(groups))
		}
		patterns[s] = "^Test(?:" + strings.Join(alts[s], "|") + ")"
	}
	patterns[0] = "^(?:Example|Fuzz)|" + patterns[0]
	counts[0] += others
	return patterns, counts, nil
}
