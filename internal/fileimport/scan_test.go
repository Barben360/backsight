package fileimport

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// tree writes files (relative path -> content) under a new temp root.
func tree(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func rels(fs []File) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Rel)
	}
	return out
}

func TestScanSkipReasons(t *testing.T) {
	root := tree(t, "", map[string]string{
		"a.md":         "alpha",
		"b.TXT":        "beta",
		"c.pdf":        "pdf",
		"big.md":       strings.Repeat("x", maxFileSize+1),
		"nul.md":       "ab\x00",
		"bad.md":       "a\xffb",
		"empty.md":     "---\ntype: x\n---\n  \n",
		".hidden/x.md": "hidden",
		".dot.md":      "hidden file",
		"logs/x.md":    "log",
		"logs/y/z.md":  "log",
	})
	symlinked := true
	if err := os.Symlink(filepath.Join(root, "a.md"), filepath.Join(root, "link.md")); err != nil {
		symlinked = false
	}
	s, err := ScanDir(root, ScanOptions{Exclude: []string{"logs/**"}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(s.Files); !reflect.DeepEqual(got, []string{"a.md", "b.TXT"}) {
		t.Errorf("files = %v", got)
	}
	want := map[string][]string{
		"extension": {"c.pdf"},
		"too_large": {"big.md"},
		"not_text":  {"bad.md", "nul.md"},
		"empty":     {"empty.md"},
		"hidden":    {".dot.md"},
	}
	if symlinked {
		want["symlink"] = []string{"link.md"}
	}
	if !reflect.DeepEqual(s.Skipped, want) {
		t.Errorf("skipped = %v, want %v", s.Skipped, want)
	}
	wantDirs := map[string][]string{"hidden": {".hidden"}, "excluded": {"logs"}}
	if !reflect.DeepEqual(s.SkippedDirs, wantDirs) {
		t.Errorf("skipped dirs = %v, want %v", s.SkippedDirs, wantDirs)
	}
}

func TestScanExtAddsJSONL(t *testing.T) {
	root := tree(t, "", map[string]string{"e.jsonl": `{"a":1}`, "n.md": "note"})
	s, err := ScanDir(root, ScanOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(s.Files); !reflect.DeepEqual(got, []string{"n.md"}) || !reflect.DeepEqual(s.Skipped["extension"], []string{"e.jsonl"}) {
		t.Fatalf("default: files %v, skipped %v", got, s.Skipped)
	}
	s, err = ScanDir(root, ScanOptions{Ext: []string{".jsonl"}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(s.Files); !reflect.DeepEqual(got, []string{"e.jsonl", "n.md"}) {
		t.Errorf("with --ext: files %v", got)
	}
}

func TestScanRootUnderHiddenDirReadsAll(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".claude", "LIFEOS", "MEMORY")
	tree(t, root, map[string]string{"KNOWLEDGE/People/p.md": "a person"})
	s, err := ScanDir(root, ScanOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(s.Files); !reflect.DeepEqual(got, []string{"KNOWLEDGE/People/p.md"}) {
		t.Errorf("files = %v, skipped %v %v", got, s.Skipped, s.SkippedDirs)
	}
}

func TestScanEdgeFiles(t *testing.T) {
	names := []string{"a b.md", "é.md", "x#2.md", "y@z.md", "exact.md"}
	files := map[string]string{}
	for _, n := range names[:4] {
		files[n] = "content of " + n
	}
	files["exact.md"] = strings.Repeat("y", maxFileSize)
	root := tree(t, "", files)
	s, err := ScanDir(root, ScanOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	got := rels(s.Files)
	for _, n := range names {
		if !slices.Contains(got, n) {
			t.Errorf("%q not read; files %v, skipped %v", n, got, s.Skipped)
		}
	}
	if maxFileSize != 1048576 {
		t.Errorf("maxFileSize = %d, want 1048576", maxFileSize)
	}
}

func TestScanRedactsAndHashesRaw(t *testing.T) {
	secret := "sk-ant-" + strings.Repeat("Ab3", 20)
	raw := "my key is " + secret + " ok"
	root := tree(t, "", map[string]string{"k.md": raw})
	s, err := ScanDir(root, ScanOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 1 {
		t.Fatalf("files %v", s.Files)
	}
	f := s.Files[0]
	sum := sha256.Sum256([]byte(raw))
	if f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %s, want the raw bytes' hash", f.SHA256)
	}
	if !reflect.DeepEqual(f.Redacted, []string{"anthropic_key"}) {
		t.Errorf("redacted = %v", f.Redacted)
	}
	_, red, _, err := ReadContent(f.Abs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(red, secret) || f.Chars != int64(utf8.RuneCountInString(red)) {
		t.Errorf("redacted content %q, chars %d", red, f.Chars)
	}
}

func TestScanDateFallsBackToMtime(t *testing.T) {
	root := tree(t, "", map[string]string{"n.md": "no frontmatter", "d.md": "---\ndate: 2025-03-04\n---\nbody"})
	mt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "n.md"), mt, mt); err != nil {
		t.Fatal(err)
	}
	s, err := ScanDir(root, ScanOptions{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	byRel := map[string]File{}
	for _, f := range s.Files {
		byRel[f.Rel] = f
	}
	if n := byRel["n.md"]; !n.Date.Equal(mt) || !n.DateFromMtime {
		t.Errorf("n.md date %v from mtime %v", n.Date, n.DateFromMtime)
	}
	if d := byRel["d.md"]; d.DateFromMtime || d.Date.Year() != 2025 {
		t.Errorf("d.md date %v from mtime %v", d.Date, d.DateFromMtime)
	}
}

func TestScanRefusesAMissingOrFileRoot(t *testing.T) {
	root := tree(t, "", map[string]string{"f.md": "x"})
	if _, err := ScanDir(filepath.Join(root, "nope"), ScanOptions{}); err == nil {
		t.Error("missing root: no error")
	}
	if _, err := ScanDir(filepath.Join(root, "f.md"), ScanOptions{}); err == nil {
		t.Error("file root: no error")
	}
}
