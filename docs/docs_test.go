package docs

import (
	"io/fs"
	"strings"
	"testing"
)

// The guide shows these files in a terminal: no HTML image in them (the
// README carries the pictures).
func TestEmbeddedDocsHoldNoImage(t *testing.T) {
	names, err := fs.Glob(FS, "*.md")
	if err != nil || len(names) == 0 {
		t.Fatalf("embedded docs: %v, %d files", err, len(names))
	}
	for _, name := range names {
		raw, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "<img") {
			t.Errorf("%s holds an <img> tag", name)
		}
	}
}
