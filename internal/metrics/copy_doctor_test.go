package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every check the doctor can emit has a plain name without an internal word,
// and the sentences use it.
func TestDoctorNamesCoverEveryCheck(t *testing.T) {
	files, err := filepath.Glob("../doctor/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("doctor sources: %v %v", files, err)
	}
	id := regexp.MustCompile(`ID: *"([a-z][a-z-]*)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		for _, m := range id.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) < 40 {
		t.Fatalf("found only %d check ids: the scan is broken", len(seen))
	}
	internal := regexp.MustCompile(`(?i)\b(bank|ledger|journal|hooks?|epoch|quarantine|retain|boost|consolidation|observation|telemetry)\b`)
	for id := range seen {
		name, ok := doctorNames[id]
		if !ok {
			t.Errorf("check %q has no plain name", id)
			continue
		}
		if internal.MatchString(name) {
			t.Errorf("check %q is named %q", id, name)
		}
	}
	for stale := range doctorNames {
		if !seen[stale] {
			t.Errorf("name for %q, which the doctor does not emit", stale)
		}
	}
	if got := DoctorName("some-new-check"); got != "some new check" {
		t.Errorf("unknown id = %q", got)
	}
	if got := doctorErrorSentence("bank"); got != "Health check «memory» failed." {
		t.Errorf("sentence = %q", got)
	}
	if got := doctorWarnSentence("server-reach"); got != "Health check «memory server reach» reports a warning." {
		t.Errorf("sentence = %q", got)
	}
}
