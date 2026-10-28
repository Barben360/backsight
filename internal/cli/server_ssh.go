package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/release"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/scripts"
)

// The remote scripts reach sh through the user's login shell, inside one
// quoted word (remoteScript): one line (csh refuses a newline inside
// quotes), no quote, no backslash (fish and csh read them otherwise) and
// no '!' (csh's history).

// probeScript prints the host's facts, one key=value line each, and
// changes nothing. The shim is where install.sh puts it on Linux.
var probeScript = strings.Join([]string{
	`set -u`,
	`echo "os=$(uname -s)"`,
	`echo "machine=$(uname -m)"`,
	`echo "conn=${SSH_CONNECTION:-}"`,
	`if [ -n "${BACKSIGHT_HOME:-}" ]; then shim=$BACKSIGHT_HOME/data/bin/current; else shim=${XDG_DATA_HOME:-$HOME/.local/share}/backsight/bin/current; fi`,
	`echo "shim=$shim"`,
	`if [ -e "$shim" ] || [ -L "$shim" ]; then echo has_shim=yes; else echo has_shim=no; fi`,
	`v=`,
	`if command -v minisign >/dev/null 2>&1; then v=minisign; elif command -v openssl >/dev/null 2>&1; then case $(openssl version 2>/dev/null) in "OpenSSL "[3-9]*) v=openssl ;; esac; fi`,
	`echo "verifier=$v"`,
	`m=`,
	`for c in tar mktemp; do command -v "$c" >/dev/null 2>&1 || m="$m $c"; done`,
	`echo "missing=$m"`,
	`echo "linger=$(loginctl show-user "$(id -u)" -p Linger --value 2>/dev/null)"`,
}, "; ")

// installScript unpacks the tar on stdin (install.sh and release/) into a
// temp directory removed on exit, also on a hangup (a killed ssh) or a
// signal, and runs that install.sh on it. install.sh checks the release
// against the key it carries, or against $1 instead when given (a test
// key); the host's own environment never chooses it (unset first).
var installScript = strings.Join([]string{
	`unset BACKSIGHT_INSTALL_PUBKEY BACKSIGHT_RELEASE_URL`,
	`set -eu`,
	// The traps come first: once the directory exists, a signal removes it.
	`d=`,
	`cleanup() { if [ -n "$d" ]; then rm -rf -- "$d"; fi; }`,
	`trap cleanup EXIT`,
	`trap "exit 129" HUP`, // dash and zsh run no EXIT trap on a signal's death
	`trap "exit 130" INT`,
	`trap "exit 143" TERM`,
	`d=$(mktemp -d)`,
	`tar -xf - -C "$d"`,
	`if [ -n "${1:-}" ]; then BACKSIGHT_INSTALL_PUBKEY=$1; export BACKSIGHT_INSTALL_PUBKEY; fi`,
	`BACKSIGHT_RELEASE_DIR=$d/release sh "$d/install.sh"`,
}, "; ")

// handbackScript prints the token file $1, which server token wrote on
// the host just before, as one line, and removes it on exit, also on a
// hangup (a killed ssh) or a signal.
var handbackScript = strings.Join([]string{
	`set -eu`,
	`f=${1:-}`,
	`cleanup() { if [ -n "$f" ]; then rm -f -- "$f"; fi; }`,
	`trap cleanup EXIT`,
	`trap "exit 129" HUP`,
	`trap "exit 130" INT`,
	`trap "exit 143" TERM`,
	`t=`,
	`IFS= read -r t < "$f" || :`,
	`[ -n "$t" ]`,
	`echo "BACKSIGHT-TOKEN $t"`,
}, "; ")

// hostFacts: what probeScript says about the host, read-only.
type hostFacts struct {
	OS, Machine string   // uname -s, uname -m
	GOARCH      string   // amd64 | arm64; "" unsupported
	Addr        string   // the host's end of the ssh connection (SSH_CONNECTION's third field)
	Shim        string   // <data>/bin/current, computed as install.sh does on Linux
	HasShim     bool     // the shim exists (a link, even dangling)
	Verifier    string   // minisign | openssl | "" (none able to check Ed25519)
	Missing     []string // of tar, mktemp
	Linger      string   // loginctl's Linger for this user: yes | no | ""
}

// probeHost reads the host's facts. Lines the login shell's profile
// prints around the answer are ignored.
func probeHost(ctx context.Context, target string) (hostFacts, error) {
	remote, err := remoteScript("probe", probeScript)
	if err != nil {
		return hostFacts{}, err
	}
	var out bytes.Buffer
	tail, err := sshRun(ctx, target, remote, nil, &out, nil)
	if re := (*remoteExit)(nil); errors.As(err, &re) {
		return hostFacts{}, &output.Error{Code: output.Blocked, Message: fmt.Sprintf("cannot read what %s runs (%s): nothing was changed there", target, lastLine(tail))}
	}
	if err != nil {
		return hostFacts{}, err
	}
	var f hostFacts
	seen := map[string]bool{}
	for _, l := range strings.Split(out.String(), "\n") {
		k, v, ok := strings.Cut(strings.TrimSuffix(l, "\r"), "=")
		if !ok {
			continue
		}
		seen[k] = true
		switch k {
		case "os":
			f.OS = v
		case "machine":
			f.Machine = v
		case "conn":
			if w := strings.Fields(v); len(w) >= 3 {
				f.Addr = w[2]
			}
		case "shim":
			f.Shim = v
		case "has_shim":
			f.HasShim = v == "yes"
		case "verifier":
			if v == "minisign" || v == "openssl" {
				f.Verifier = v
			}
		case "missing":
			if w := strings.Fields(v); len(w) > 0 {
				f.Missing = w
			}
		case "linger":
			if v == "yes" || v == "no" {
				f.Linger = v
			}
		default:
			seen[k] = false
		}
	}
	if !seen["os"] || !seen["machine"] || !seen["shim"] {
		return hostFacts{}, &output.Error{Code: output.Blocked,
			Message: "the shell on " + target + " did not answer the probe as sh does: check that it runs sh -c for a command (ssh " + shellWord(target) + " uname -s)"}
	}
	switch f.Machine {
	case "x86_64", "amd64":
		f.GOARCH = "amd64"
	case "aarch64", "arm64":
		f.GOARCH = "arm64"
	}
	return f, nil
}

// findRelease finds the release of this backsight's version and its
// archive for a Linux goarch host, reading only the source's listing.
func findRelease(ctx context.Context, src release.Source, version, goarch string) (rel release.Release, archive string, err error) {
	if v, err := release.Parse(version); err != nil || v.Dev() {
		return rel, "", &output.Error{Code: output.Blocked,
			Message: fmt.Sprintf("this backsight is a development build (%s): no release matches it; run a released backsight, or backsight server install --print-commands", version)}
	}
	rels, err := src.Releases(ctx)
	if err != nil {
		return rel, "", sourceError(err)
	}
	rel, ok := release.Find(rels, version)
	if !ok {
		return rel, "", &output.Error{Code: output.Blocked,
			Message: fmt.Sprintf("the release source has no version %s: server install --ssh copies the release matching this backsight", version)}
	}
	a, ok := rel.Archive("linux", goarch)
	if !ok {
		return rel, "", nothingChanged("release %s has no archive for linux/%s", rel.Version, goarch)
	}
	return rel, a.Name, nil
}

// stageRelease downloads the release of this backsight's version for a
// Linux goarch host into work and verifies it here: the host needs no
// route to the release source, and checks it again itself.
func stageRelease(ctx context.Context, src release.Source, version, goarch, work string) (archive, keyID string, err error) {
	rel, archive, err := findRelease(ctx, src, version, goarch)
	if err != nil {
		return "", "", err
	}
	if _, keyID, err = verifyRelease(ctx, src, rel, archive, work); err != nil {
		return "", "", err
	}
	return archive, keyID, nil
}

// installOnHost sends this version's install.sh and the release staged in
// work to the host on ssh's stdin, and runs install.sh there: it checks
// the signature with the key it carries and the archive's hash, and
// switches the shim, then the command on PATH, last (a failure between
// the two leaves the shim switched). tail is the last lines of its stderr.
func installOnHost(ctx context.Context, target, work, archive string) (tail string, err error) {
	var args []string
	if signing.TestKey != "" { // a testkey build: install.sh trusts it too, and says so
		args = append(args, signing.TestKey)
	}
	remote, err := remoteScript("install", installScript, args...)
	if err != nil {
		return "", err
	}
	upload := filepath.Join(work, "upload.tar")
	if err := writeUpload(upload, work, archive); err != nil {
		return "", blocked(err)
	}
	f, err := os.Open(upload)
	if err != nil {
		return "", blocked(err)
	}
	defer func() { _ = f.Close() }()
	tail, err = sshRun(ctx, target, remote, f, nil, nil)
	if re := (*remoteExit)(nil); errors.As(err, &re) {
		return tail, &output.Error{Code: output.Blocked,
			Message: fmt.Sprintf("the release did not install on %s (%s): the host may still run its previous backsight; rerunning this is safe", target, lastLine(tail))}
	}
	if err != nil {
		return tail, &sshTransport{err}
	}
	return tail, nil
}

// writeUpload writes the tar installScript unpacks: install.sh and
// release/ holding checksums.txt, its signature and the archive.
func writeUpload(dst, work, archive string) (err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(out)
	if err := tw.WriteHeader(&tar.Header{Name: "install.sh", Mode: 0o644, Size: int64(len(scripts.Install)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(scripts.Install); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "release/", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	for _, name := range []string{"checksums.txt", "checksums.txt.minisig", archive} {
		if err := tarFile(tw, filepath.Join(work, name), "release/"+name); err != nil {
			return err
		}
	}
	return tw.Close()
}

func tarFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// lastLine is the last non-blank line of s, trimmed.
func lastLine(s string) string {
	last := ""
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			last = l
		}
	}
	return last
}

// sshTransport is ssh's own failure on a step on the host (exit 255, or
// ssh not started): the step may not have run, or have run in part.
type sshTransport struct{ err error }

func (s *sshTransport) Error() string { return s.err.Error() }
func (s *sshTransport) Unwrap() error { return s.err }
