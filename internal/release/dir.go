package release

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Dir reads releases from a local directory (decision 0063 Q2: installs
// from a local build or `gh release download`, until the repository is
// public). Path is either one release or a parent of releases:
//
//   - one release: Path holds checksums.txt, as goreleaser's dist/ or a
//     `gh release download <tag> -D <dir>` directory does;
//   - otherwise every immediate subdirectory holding checksums.txt is a
//     release (for example releases/v0.1.0/, releases/v0.2.0/).
//
// In each release the version is metadata.json's "version" (goreleaser),
// else the one in the archive names listed by checksums.txt
// (backsight_<version>_<os>_<arch>.tar.gz), else the directory name; the
// date is metadata.json's "date", else checksums.txt's modification time;
// the notes are CHANGELOG.md when present. Assets are the top-level files,
// without goreleaser's bookkeeping (metadata.json, artifacts.json,
// config.yaml) and the notes.
type Dir struct{ Path string }

const checksumsFile = "checksums.txt"

var (
	notAssets   = []string{"metadata.json", "artifacts.json", "config.yaml", "CHANGELOG.md"}
	archiveName = regexp.MustCompile(`^backsight_(.+)_[0-9a-z]+_[0-9a-z]+\.(?:tar\.gz|zip)$`)
)

func (d Dir) Name() string { return "dir:" + d.Path }

func (d Dir) Releases(context.Context) ([]Release, error) {
	if _, err := os.Stat(filepath.Join(d.Path, checksumsFile)); err == nil {
		r, err := readReleaseDir(d.Path)
		if err != nil {
			return nil, err
		}
		return []Release{r}, nil
	}
	ents, err := os.ReadDir(d.Path)
	if err != nil {
		return nil, fmt.Errorf("release directory unreadable: %w", err)
	}
	var rels []Release
	for _, e := range ents {
		sub := filepath.Join(d.Path, e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(sub, checksumsFile)); err != nil {
			continue
		}
		r, err := readReleaseDir(sub)
		if err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("%s holds no release: no %s in it or in its subdirectories", d.Path, checksumsFile)
	}
	return rels, nil
}

func readReleaseDir(dir string) (Release, error) {
	var meta struct {
		Version string    `json:"version"`
		Date    time.Time `json:"date"`
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "metadata.json")); err == nil {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return Release{}, fmt.Errorf("%s: %w", filepath.Join(dir, "metadata.json"), err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Release{}, err
	}
	v := strings.TrimPrefix(meta.Version, "v")
	if v == "" {
		v = versionFromChecksums(filepath.Join(dir, checksumsFile))
	}
	if v == "" {
		v = strings.TrimPrefix(filepath.Base(dir), "v")
	}
	parsed, err := Parse(v)
	if err != nil {
		return Release{}, fmt.Errorf("%s: no release version (metadata.json, archive names or directory name): %w", dir, err)
	}
	r := Release{Version: v, Tag: "v" + v, Prerelease: parsed.Prerelease(), Published: meta.Date.UTC()}
	if meta.Date.IsZero() {
		if fi, err := os.Stat(filepath.Join(dir, checksumsFile)); err == nil {
			r.Published = fi.ModTime().UTC()
		}
	}
	if notes, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md")); err == nil {
		r.Notes = string(notes)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return Release{}, err
	}
	for _, e := range ents {
		if !e.Type().IsRegular() || slices.Contains(notAssets, e.Name()) {
			continue
		}
		a := Asset{Name: e.Name(), URL: filepath.Join(dir, e.Name())}
		if fi, err := e.Info(); err == nil {
			a.Size = fi.Size()
		}
		r.Assets = append(r.Assets, a)
	}
	return r, nil
}

// versionFromChecksums is the version in the first archive name listed.
func versionFromChecksums(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if m := archiveName.FindStringSubmatch(strings.TrimPrefix(fields[1], "*")); m != nil {
			return m[1]
		}
	}
	return ""
}

// Open opens one of rel's assets; only names Releases listed are opened.
func (d Dir) Open(_ context.Context, rel Release, name string) (io.ReadCloser, error) {
	a, ok := rel.Asset(name)
	if !ok || name != filepath.Base(name) {
		return nil, fmt.Errorf("%s: release %s has no asset %s", d.Name(), rel.Tag, name)
	}
	return os.Open(a.URL)
}
