package testtool

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every Windows skip names its owner, so that the last task can find what
// is left: "Task <n>: ", "Task 3b: " or "linux-only: ".
func TestEverySkipWindowsNamesItsOwner(t *testing.T) {
	call := regexp.MustCompile(`SkipWindows\(t, "((?:[^"\\]|\\.)*)"`)
	ok := regexp.MustCompile(`^(Task (3b|[0-9]+)|linux-only): \S`)
	calls := 0
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".superpowers") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range call.FindAllStringSubmatch(string(raw), -1) {
			calls++
			if !ok.MatchString(m[1]) {
				t.Errorf("%s: SkipWindows reason %q does not start with \"Task <n>: \", \"Task 3b: \" or \"linux-only: \"", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 100 {
		t.Errorf("only %d SkipWindows calls found: the scan reads nothing?", calls)
	}
}

func TestSkipReasonRuleRejectsAVagueOne(t *testing.T) {
	ok := regexp.MustCompile(`^(Task (3b|[0-9]+)|linux-only): \S`)
	for reason, want := range map[string]bool{"Task 3: mode bits": true, "Task 3b: x": true, "linux-only: POSIX script": true,
		"no owning task": false, "Task 7-10: x": false, "Task 3:": false, "windows: Task 3: x": false} {
		if ok.MatchString(reason) != want {
			t.Errorf("%q: %v, want %v", reason, !want, want)
		}
	}
}
