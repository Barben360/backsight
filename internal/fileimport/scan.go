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
	"io"
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
	// Skip, when set, names the reason a path below the root is skipped
	// ("" reads it), after the hidden and excluded checks: a folder is
	// then not walked. A preset (Codex's memories) knows its layout.
	Skip func(rel string, dir bool) string
	// Inspect, when set, sees each file to send (its raw bytes, once they
	// passed every check) and may set its date and project, or name the
	// reason it is skipped.
	Inspect func(rel string, raw []byte, f *File) string
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
	// Project: the project a preset maps this file to; "" is the import's.
	Project string `json:"project,omitempty"`
}

// Scan is what a folder holds for an import.
type Scan struct {
	Root        string              // absolute, cleaned, the root's own links resolved
	Files       []File              // sorted by Rel
	Skipped     map[string][]string // reason -> relative paths, sorted
	SkippedDirs map[string][]string // excluded | hidden -> relative dirs, sorted
	// Preset: the reader that made the scan ("" a plain folder); Stores:
	// the stores it read, for a preset that has several.
	Preset string
	Stores []string
	// KeptOut: the working folders kept out of memory a preset's sources
	// name, which keep its consolidated files out (mixes_excluded).
	KeptOut []string
}

// ScanDir reads root recursively. Hidden components and excludes apply to
// the path below root only; links are not followed.
func ScanDir(root string, opt ScanOptions) (Scan, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	abs, err := ResolveRoot(root)
	if err != nil {
		return Scan{}, err
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
			if opt.Skip != nil {
				if reason := opt.Skip(rel, true); reason != "" {
					s.SkippedDirs[reason] = append(s.SkippedDirs[reason], rel)
					return fs.SkipDir
				}
			}
			return nil
		}
		reason := ""
		if opt.Skip != nil && !hidden && !excluded {
			reason = opt.Skip(rel, false)
		}
		switch {
		case hidden:
			skip(SkipHidden, rel)
			return nil
		case excluded:
			skip(SkipExcluded, rel)
			return nil
		case reason != "":
			skip(reason, rel)
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
		if opt.Inspect != nil {
			if reason := opt.Inspect(rel, raw, &f); reason != "" {
				skip(reason, rel)
				return nil
			}
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

// ResolveRoot is root made absolute, its own links resolved; it must be
// a folder.
func ResolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return "", fmt.Errorf("%s: %w", root, err)
	}
	if st, err := os.Stat(abs); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("%s is not a folder", root)
	}
	return abs, nil
}

// ReadForSend reads a queued file again just before it is sent, with the
// scan's checks: a regular file (not a link, pipe or folder), at most 1 MB,
// text, not empty after its frontmatter. reason is the skip reason when it
// no longer qualifies; nothing else is read then.
func ReadForSend(abs string) (raw []byte, redacted, reason string) {
	info, err := os.Lstat(abs)
	switch {
	case err != nil:
		return nil, "", SkipUnreadable
	case info.Mode()&fs.ModeSymlink != 0:
		return nil, "", SkipSymlink
	case !info.Mode().IsRegular():
		return nil, "", SkipUnreadable
	case info.Size() > maxFileSize:
		return nil, "", SkipTooLarge
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", SkipUnreadable
	}
	defer func() { _ = f.Close() }()
	raw, err = io.ReadAll(io.LimitReader(f, maxFileSize+1))
	switch {
	case err != nil:
		return nil, "", SkipUnreadable
	case len(raw) > maxFileSize:
		return nil, "", SkipTooLarge
	case bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw):
		return nil, "", SkipNotText
	case len(bytes.TrimSpace(stripFrontmatter(raw))) == 0:
		return nil, "", SkipEmpty
	}
	redacted, _ = redact.Text(string(raw))
	return raw, redacted, ""
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
