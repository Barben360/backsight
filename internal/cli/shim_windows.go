//go:build windows

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/selfcheck"
)

// pointShimWindows makes shim a copy of target. The shim may be running
// (every Claude Code session and the supervisor hold current.exe open):
// Windows lets a running image be renamed, not replaced or deleted, so the
// shim is renamed to the first free shim.old-<n> (n from 1), the copy
// takes its name, and the .old-<n> nobody runs any more are removed. Two
// swaps in one run (update apply) therefore never collide on a held .old.
// The signed checksums beside target follow the copy, so the shim checks
// itself like any installed binary (SPEC §11.1).
func pointShimWindows(target, shim string) error {
	// Read everything first: a failure here changes nothing.
	if _, err := os.Stat(target); err != nil {
		return err
	}
	sums, err := readChecksums(filepath.Dir(target))
	if err != nil {
		return err
	}
	var prev checksumSet
	if filepath.Clean(filepath.Dir(target)) != filepath.Clean(filepath.Dir(shim)) {
		if prev, err = readChecksums(filepath.Dir(shim)); err != nil {
			return err
		}
	}
	if sameContent(shim, target) {
		// Already target's bytes (a repeat client install): renaming it
		// aside would move a supervisor or a session running it onto a
		// .old-<n> for nothing. Only the checksums may need to follow.
		if prev != nil && !prev.equal(sums) {
			if err := writeChecksums(filepath.Dir(shim), sums); err != nil {
				return errors.Join(err, writeChecksums(filepath.Dir(shim), prev))
			}
		}
		cleanOld(shim)
		return nil
	}
	old := ""
	if _, err := os.Lstat(shim); err == nil {
		for n := 1; ; n++ {
			cand := shim + ".old-" + strconv.Itoa(n)
			if _, err := os.Lstat(cand); err == nil {
				continue // held by a session, or not cleaned yet
			}
			if err := os.Rename(shim, cand); err != nil {
				return err
			}
			old = cand
			break
		}
	}
	if err := copyFile(target, shim, 0o755); err != nil {
		if old != "" { // never leave the shim missing
			if rerr := os.Rename(old, shim); rerr != nil {
				err = errors.Join(err, fmt.Errorf("putting the previous shim back: %w", rerr))
			}
		}
		return err
	}
	// The checksums follow the exe only once it is in place; both or none.
	if prev != nil {
		if err := writeChecksums(filepath.Dir(shim), sums); err != nil {
			err = errors.Join(err, writeChecksums(filepath.Dir(shim), prev))
			if old != "" {
				err = errors.Join(err, os.Remove(shim), os.Rename(old, shim))
			}
			return err
		}
	}
	cleanOld(shim)
	return nil
}

// cleanOld removes every shim.old-<n> that can be removed now. A running
// image cannot be deleted: its removal fails at once (access denied) and
// is ignored, with a single attempt and no wait, because waiting the retry
// window for each held copy would stall every swap while sessions run;
// the next swap or install tries again.
func cleanOld(shim string) {
	olds, _ := filepath.Glob(escapeGlob(shim) + ".old-*")
	sort.Strings(olds)
	for _, p := range olds {
		if n := strings.TrimPrefix(p, shim+".old-"); n == "" || strings.Trim(n, "0123456789") != "" {
			continue
		}
		_ = os.Remove(p)
	}
}

// escapeGlob quotes the glob metacharacters of a path (a profile may hold
// [ or ]; Windows paths have no backslash escape, so these are bracketed).
func escapeGlob(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '[', ']', '*', '?':
			b.WriteString("[" + string(r) + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// checksumSet is the content of checksums.txt and its signature in a
// directory; a nil entry is a file that is not there.
type checksumSet map[string][]byte

func (c checksumSet) equal(o checksumSet) bool {
	for _, name := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		if (c[name] == nil) != (o[name] == nil) || !bytes.Equal(c[name], o[name]) {
			return false
		}
	}
	return true
}

func readChecksums(dir string) (checksumSet, error) {
	set := checksumSet{}
	for _, name := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			set[name] = nil
		case err != nil:
			return nil, err
		default:
			set[name] = raw
		}
	}
	return set, nil
}

// writeChecksums makes dir hold exactly set: files written atomically,
// absent ones removed (a dev build must not keep another version's).
func writeChecksums(dir string, set checksumSet) error {
	var errs []error
	for _, name := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		p := filepath.Join(dir, name)
		if set[name] == nil {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := fsutil.WriteAtomic(p, set[name], 0o644); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sameContent reports that a and b both exist with the same bytes.
func sameContent(a, b string) bool {
	ha, err := fsutil.HashFile(a)
	if err != nil || ha == fsutil.Absent {
		return false
	}
	hb, err := fsutil.HashFile(b)
	return err == nil && ha == hb
}
