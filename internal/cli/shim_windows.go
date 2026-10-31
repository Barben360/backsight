//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
	if err := moveChecksums(filepath.Dir(target), filepath.Dir(shim)); err != nil {
		return err
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
			_ = os.Rename(old, shim)
		}
		return err
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

// moveChecksums makes dst hold the same checksums.txt and signature as
// src: copied when src has them, removed when it has none (a dev build
// must not keep another version's, which the new binary would fail).
func moveChecksums(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	for _, name := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		if _, err := os.Stat(from); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if err := os.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("removing %s: %w", to, err)
				}
				continue
			}
			return err
		}
		if err := copyFile(from, to, 0o644); err != nil {
			return err
		}
	}
	return nil
}
