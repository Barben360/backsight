package release

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRelease lays out a goreleaser dist/ for version v under dir.
func writeRelease(t *testing.T, dir, v string, metadata bool) {
	t.Helper()
	_ = os.MkdirAll(filepath.Join(dir, "backsight_linux_amd64_v1"), 0o700)
	files := map[string]string{
		"checksums.txt":                                  "aa  backsight_" + v + "_linux_amd64.tar.gz\nbb  backsight_" + v + "_windows_amd64.zip\n",
		"checksums.txt.minisig":                          "sig",
		"backsight_" + v + "_linux_amd64.tar.gz":         "tgz",
		"backsight_" + v + "_linux_amd64.tar.gz.minisig": "sig",
		"backsight_" + v + "_windows_amd64.zip":          "zip",
		"artifacts.json":                                 "[]",
		"config.yaml":                                    "version: 2",
		"backsight_linux_amd64_v1/backsight":             "bin",
	}
	if metadata {
		files["metadata.json"] = `{"project_name":"backsight","tag":"v` + v + `","version":"` + v + `","date":"2026-10-24T13:48:25+02:00"}`
		files["CHANGELOG.md"] = "## Changelog\n* abc1234 ✨ Add update check\n"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func names(r Release) string {
	var s []string
	for _, a := range r.Assets {
		s = append(s, a.Name)
	}
	return strings.Join(s, " ")
}

func TestDirReadsAGoreleaserDist(t *testing.T) {
	dir := t.TempDir()
	writeRelease(t, dir, "0.0.3-snapshot+1300233", true)
	rels, err := Dir{Path: dir}.Releases(context.Background())
	if err != nil || len(rels) != 1 {
		t.Fatalf("%v %+v", err, rels)
	}
	r := rels[0]
	if r.Version != "0.0.3-snapshot+1300233" || r.Tag != "v0.0.3-snapshot+1300233" || !r.Prerelease ||
		!r.Published.Equal(time.Date(2026, 10, 24, 11, 48, 25, 0, time.UTC)) || Summary(r.Notes) != "✨ Add update check" {
		t.Errorf("%+v", r)
	}
	// goreleaser's own bookkeeping and the notes are not assets
	want := "backsight_0.0.3-snapshot+1300233_linux_amd64.tar.gz backsight_0.0.3-snapshot+1300233_linux_amd64.tar.gz.minisig backsight_0.0.3-snapshot+1300233_windows_amd64.zip checksums.txt checksums.txt.minisig"
	if names(r) != want {
		t.Errorf("assets %q", names(r))
	}
}

// A `gh release download` directory has no metadata.json: the version comes
// from the archive names, the date from checksums.txt.
func TestDirWithoutMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "download")
	_ = os.MkdirAll(dir, 0o700)
	writeRelease(t, dir, "0.1.0", false)
	at := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	_ = os.Chtimes(filepath.Join(dir, "checksums.txt"), at, at)
	rels, err := Dir{Path: dir}.Releases(context.Background())
	if err != nil || len(rels) != 1 || rels[0].Version != "0.1.0" || rels[0].Prerelease || !rels[0].Published.Equal(at) || rels[0].Notes != "" {
		t.Fatalf("%v %+v", err, rels)
	}
}

func TestDirOfVersions(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"0.1.0", "0.2.0-m1"} {
		d := filepath.Join(root, "v"+v)
		_ = os.MkdirAll(d, 0o700)
		writeRelease(t, d, v, true)
	}
	_ = os.MkdirAll(filepath.Join(root, "scratch"), 0o700) // no checksums.txt: not a release
	rels, err := Dir{Path: root}.Releases(context.Background())
	if err != nil || versions(Newer(rels, "", true)) != "0.2.0-m1 0.1.0" {
		t.Fatalf("%v %+v", err, rels)
	}
}

func TestDirRefusals(t *testing.T) {
	if _, err := (Dir{Path: filepath.Join(t.TempDir(), "absent")}).Releases(context.Background()); err == nil {
		t.Error("a missing directory listed")
	}
	if _, err := (Dir{Path: t.TempDir()}).Releases(context.Background()); err == nil || !strings.Contains(err.Error(), "checksums.txt") {
		t.Errorf("an empty directory: %v", err)
	}
	bad := t.TempDir()
	_ = os.WriteFile(filepath.Join(bad, "checksums.txt"), []byte("aa  notes.txt\n"), 0o600)
	if _, err := (Dir{Path: bad}).Releases(context.Background()); err == nil {
		t.Error("a release with no version listed")
	}
}

func TestDirOpen(t *testing.T) {
	dir := t.TempDir()
	writeRelease(t, dir, "0.1.0", true)
	src := Dir{Path: dir}
	rels, err := src.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := src.Open(context.Background(), rels[0], "backsight_0.1.0_linux_amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "tgz" {
		t.Errorf("%q", b)
	}
	for _, name := range []string{"metadata.json", "../x", "backsight_linux_amd64_v1/backsight", "absent"} {
		if rc, err := src.Open(context.Background(), rels[0], name); err == nil {
			_ = rc.Close()
			t.Errorf("%s opened", name)
		}
	}
}
