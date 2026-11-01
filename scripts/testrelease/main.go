// Command testrelease makes a Windows release signed by a throwaway
// minisign key, as goreleaser and the release key make the real one:
// scripts/native-chain-windows.ps1 builds a testkey binary trusting the
// key, then installs this release with install.ps1 (BACKSIGHT_RELEASE_DIR,
// BACKSIGHT_INSTALL_PUBKEY). Never a release tool: the key is generated
// for one run and trusted only by testkey builds.
//
//	testrelease keygen <secret key file>    writes the key (0600), prints its public key line
//	testrelease release <secret key file> <backsight.exe> <version> <dir>
//	                                        writes backsight_<version>_windows_amd64.zip, checksums.txt
//	                                        (the bare binary's and the zip's lines) and its .minisig
package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"aead.dev/minisign"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "testrelease:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "keygen":
		pub, err := keygen(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, pub)
		return err
	case len(args) == 5 && args[0] == "release":
		return release(args[1], args[2], args[3], args[4])
	}
	return errors.New("usage: testrelease keygen <key file> | release <key file> <backsight.exe> <version> <dir>")
}

// keygen writes a new unencrypted secret key to file, refusing to replace
// one, and returns its public key line (what LinkedTestKey and
// BACKSIGHT_INSTALL_PUBKEY take).
func keygen(file string) (string, error) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	text, err := priv.MarshalText()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(text); err != nil {
		_ = f.Close()
		return "", err
	}
	return pub.String(), f.Close()
}

// versionRE is what install.ps1 accepts as a version (its directory name).
var versionRE = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]*$`)

// release writes exe's Windows release, version version, into dir: the
// zip goreleaser makes, checksums.txt listing the bare binary (the
// binary's self-check) and the zip (install.ps1's check), and its
// prehashed signature, whose trusted comment names checksums.txt.
func release(keyFile, exe, version, dir string) error {
	if !versionRE.MatchString(version) {
		return fmt.Errorf("version %q: letters, digits and ._+- only", version)
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	var priv minisign.PrivateKey
	if err := priv.UnmarshalText(raw); err != nil {
		return err
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("backsight.exe")
	if err != nil {
		return err
	}
	if _, err := w.Write(bin); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	prefix := "backsight_" + version + "_windows_amd64"
	line := func(b []byte, name string) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	sums := []byte(line(bin, prefix+".exe") + line(buf.Bytes(), prefix+".zip"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r := minisign.NewReader(bytes.NewReader(sums))
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	sig := r.SignWithComments(priv, "timestamp:"+strconv.FormatInt(time.Now().Unix(), 10)+"\tfile:checksums.txt\thashed",
		"signature from a throwaway test key (scripts/testrelease)")
	for name, body := range map[string][]byte{prefix + ".zip": buf.Bytes(), "checksums.txt": sums, "checksums.txt.minisig": sig} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}
