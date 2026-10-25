// Package selfcheck lets a release binary check itself (SPEC §11.1,
// decision 0067): its SHA-256 must be the entry of its bare binary in the
// signed checksums.txt kept next to it in <data>/bin/<v>/. The check runs
// once per binary and is recorded in state.json; hooks never run it.
package selfcheck

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
)

const (
	Verified = "verified"
	Unsigned = "unsigned" // no checksums.txt next to the binary: a dev build
	Failed   = "failed"
)

// Files kept next to an installed binary.
const (
	ChecksumsFile = "checksums.txt"
	SignatureFile = "checksums.txt.minisig"
)

// BinaryName is the bare binary's entry in checksums.txt, as goreleaser's
// binary archive names it.
func BinaryName(version, goos, goarch string) string {
	name := "backsight_" + version + "_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Check hashes exe (symlinks resolved) and verifies it against the
// checksums.txt and its signature in exe's directory.
func Check(exe, version, goos, goarch string, now time.Time) state.SelfCheck {
	rec := state.SelfCheck{Version: version, Exe: exe, CheckedAt: now.UTC()}
	fail := func(err error) state.SelfCheck {
		rec.Status, rec.Detail = Failed, err.Error()
		return rec
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fail(err)
	}
	rec.Exe = real
	fi, err := os.Stat(real)
	if err != nil {
		return fail(err)
	}
	rec.Size, rec.ModTime = fi.Size(), fi.ModTime().UTC()
	dir := filepath.Dir(real)
	sums, err := os.ReadFile(filepath.Join(dir, ChecksumsFile))
	if errors.Is(err, fs.ErrNotExist) {
		rec.Status, rec.Detail = Unsigned, "no "+ChecksumsFile+" next to "+real
		return rec
	}
	if err != nil {
		return fail(err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, SignatureFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	if rec.KeyID, err = signing.VerifyChecksums(sums, sig); err != nil {
		return fail(err)
	}
	f, err := os.Open(real)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = f.Close() }()
	if err := signing.ArchiveSum(sums, BinaryName(version, goos, goarch), f); err != nil {
		return fail(err)
	}
	rec.Status = Verified
	return rec
}

// matches: rec is the check of exe as it is now, for this version.
func matches(rec *state.SelfCheck, exe, version string) bool {
	if rec == nil || rec.Version != version {
		return false
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil || real != rec.Exe {
		return false
	}
	fi, err := os.Stat(real)
	return err == nil && fi.Size() == rec.Size && fi.ModTime().UTC().Equal(rec.ModTime)
}

func check(exe string) state.SelfCheck {
	return Check(exe, buildinfo.Version, runtime.GOOS, runtime.GOARCH, time.Now())
}

// Ensure returns the recorded check of exe, running and recording it first
// when the record is for another binary (jobs tick: the first run of a
// version). A record that cannot be saved is returned with the error; the
// next call checks again.
func Ensure(d paths.Dirs, exe string) (state.SelfCheck, error) {
	if st, err := state.Load(d.StateFile()); err == nil && matches(st.SelfCheck, exe, buildinfo.Version) {
		return *st.SelfCheck, nil
	}
	rec := check(exe)
	err := state.Update(d, "self-check", func(s *state.State) error {
		s.SelfCheck = &rec
		return nil
	})
	return rec, err
}

// Current returns the recorded check of exe, or a fresh one that it does
// not record (doctor is read-only).
func Current(d paths.Dirs, exe string) state.SelfCheck {
	if st, err := state.Load(d.StateFile()); err == nil && matches(st.SelfCheck, exe, buildinfo.Version) {
		return *st.SelfCheck
	}
	return check(exe)
}
