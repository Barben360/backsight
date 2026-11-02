// Package fileimport reads a folder of text files for `backsight import`:
// the files to send, the ones skipped and why, their dates and hashes, the
// plan's estimates, and the manifest that remembers what was sent.
package fileimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/redact"
)

// maxFileSize: larger files are skipped (decision: 1 MB).
const maxFileSize = 1 << 20

// DefaultExt are the extensions read without --ext.
var DefaultExt = []string{".md", ".markdown", ".txt"}

// Skip reasons.
const (
	SkipExtension   = "extension"
	SkipTooLarge    = "too_large"
	SkipNotText     = "not_text"
	SkipEmpty       = "empty"
	SkipExcluded    = "excluded"
	SkipHidden      = "hidden"
	SkipSymlink     = "symlink"
	SkipUnreadable  = "unreadable"
	SkipOtherSource = "other_source"
)

// ScanOptions tunes a scan.
type ScanOptions struct {
	Exclude []string // globs relative to the root
	Ext     []string // extra extensions, with the dot
	Now     time.Time
}

// File is one file to send.
type File struct {
	Rel           string    `json:"rel"` // relative path, '/' separators
	Abs           string    `json:"-"`
	Size          int64     `json:"size"`
	SHA256        string    `json:"sha256"` // of the raw bytes
	Chars         int64     `json:"chars"`  // runes of the redacted content
	Date          time.Time `json:"date"`   // UTC
	DateFromMtime bool      `json:"date_from_mtime,omitempty"`
	Redacted      []string  `json:"redacted,omitempty"` // kinds, sorted
}

// Scan is what a folder holds for an import.
type Scan struct {
	Root        string              // absolute, cleaned, the root's own links resolved
	Files       []File              // sorted by Rel
	Skipped     map[string][]string // reason -> relative paths, sorted
	SkippedDirs map[string][]string // excluded | hidden -> relative dirs, sorted
}

// ScanDir reads root recursively. Hidden components and excludes apply to
// the path below root only; links are not followed.
func ScanDir(root string, opt ScanOptions) (Scan, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Scan{}, err
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return Scan{}, fmt.Errorf("%s: %w", root, err)
	}
	if st, err := os.Stat(abs); err != nil {
		return Scan{}, err
	} else if !st.IsDir() {
		return Scan{}, fmt.Errorf("%s is not a folder", root)
	}
	exts := map[string]bool{}
	for _, e := range append(slices.Clone(DefaultExt), opt.Ext...) {
		exts[NormalizeExt(e)] = true
	}
	s := Scan{Root: abs, Skipped: map[string][]string{}, SkippedDirs: map[string][]string{}}
	skip := func(reason, rel string) { s.Skipped[reason] = append(s.Skipped[reason], rel) }
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, werr error) error {
		if p == abs {
			return werr
		}
		r, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(r)
		if werr != nil {
			skip(SkipUnreadable, rel)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		hidden := strings.HasPrefix(d.Name(), ".")
		excluded := slices.ContainsFunc(opt.Exclude, func(g string) bool { return MatchGlob(g, rel) })
		if d.IsDir() {
			switch {
			case hidden:
				s.SkippedDirs[SkipHidden] = append(s.SkippedDirs[SkipHidden], rel)
				return fs.SkipDir
			case excluded:
				s.SkippedDirs[SkipExcluded] = append(s.SkippedDirs[SkipExcluded], rel)
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case hidden:
			skip(SkipHidden, rel)
			return nil
		case excluded:
			skip(SkipExcluded, rel)
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			skip(SkipSymlink, rel)
			return nil
		case !d.Type().IsRegular():
			skip(SkipUnreadable, rel)
			return nil
		case !exts[strings.ToLower(filepath.Ext(rel))]:
			skip(SkipExtension, rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			skip(SkipUnreadable, rel)
			return nil
		}
		if info.Size() > maxFileSize {
			skip(SkipTooLarge, rel)
			return nil
		}
		raw, red, kinds, err := ReadContent(p)
		switch {
		case errors.Is(err, errNotText):
			skip(SkipNotText, rel)
			return nil
		case err != nil:
			skip(SkipUnreadable, rel)
			return nil
		case len(bytes.TrimSpace(stripFrontmatter(raw))) == 0:
			skip(SkipEmpty, rel)
			return nil
		}
		f := File{Rel: rel, Abs: p, Size: int64(len(raw)), SHA256: hashOf(raw),
			Chars: int64(utf8.RuneCountInString(red)), Redacted: kinds}
		if t, ok := FrontmatterDate(raw, opt.Now); ok {
			f.Date = t
		} else {
			f.Date, f.DateFromMtime = info.ModTime().UTC(), true
		}
		s.Files = append(s.Files, f)
		return nil
	})
	if err != nil {
		return Scan{}, err
	}
	slices.SortFunc(s.Files, func(a, b File) int { return strings.Compare(a.Rel, b.Rel) })
	for _, m := range []map[string][]string{s.Skipped, s.SkippedDirs} {
		for _, v := range m {
			slices.Sort(v)
		}
	}
	return s, nil
}

var errNotText = errors.New("not text")

// ReadContent reads a file to send: its raw bytes, the redacted text and
// the redaction kinds. A file with a NUL byte or invalid UTF-8 is not text.
func ReadContent(abs string) (raw []byte, redacted string, kinds []string, err error) {
	raw, err = os.ReadFile(abs)
	if err != nil {
		return nil, "", nil, err
	}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		return raw, "", nil, errNotText
	}
	redacted, kinds = redact.Text(string(raw))
	return raw, redacted, kinds, nil
}

// NormalizeExt is ext lower-cased with a leading dot.
func NormalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
