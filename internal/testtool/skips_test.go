package testtool

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every Windows skip pins Linux-only behaviour and says which: M7 block 2
// is done, so no skip waits for a task any more ("Task <n>: ").
func TestEverySkipWindowsIsLinuxOnly(t *testing.T) {
	call := regexp.MustCompile(`SkipWindows\(t, "((?:[^"\\]|\\.)*)"`)
	ok := regexp.MustCompile(skipReason)
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
				t.Errorf("%s: SkipWindows reason %q does not start with \"linux-only: \"", p, m[1])
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

// skipReason is what a SkipWindows reason starts with.
const skipReason = `^linux-only: \S`

func TestSkipReasonRuleRejectsAVagueOne(t *testing.T) {
	ok := regexp.MustCompile(skipReason)
	for reason, want := range map[string]bool{"linux-only: POSIX script": true, "Task 3: mode bits": false, "Task 3b: x": false,
		"no owning task": false, "linux-only:": false, "linux-only: ": false, "windows: Task 3: x": false} {
		if ok.MatchString(reason) != want {
			t.Errorf("%q: %v, want %v", reason, !want, want)
		}
	}
}
