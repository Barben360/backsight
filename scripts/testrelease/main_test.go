package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aead.dev/minisign"
)

// The release verifies as install.ps1 and the binary's self-check need it
// to: a prehashed signature by the key keygen printed, whose trusted
// comment names checksums.txt, over checksums listing the bare binary and
// the zip, the zip holding the binary as given.
func TestReleaseVerifiesWithTheKeyItPrinted(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "test.key")
	var out bytes.Buffer
	if err := run([]string{"keygen", key}, &out); err != nil {
		t.Fatal(err)
	}
	var pub minisign.PublicKey
	if err := pub.UnmarshalText(bytes.TrimSpace(out.Bytes())); err != nil {
		t.Fatalf("keygen printed %q: %v", out.String(), err)
	}
	if fi, err := os.Stat(key); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0) {
		t.Errorf("the secret key file: %v, %v", fi, err)
	}
	exe := filepath.Join(dir, "backsight.exe")
	body := []byte("MZ not really a binary")
	if err := os.WriteFile(exe, body, 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dir, "rel")
	if err := run([]string{"release", key, exe, "0.950.0", rel}, &out); err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(rel, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sums, sig, zipped := read("checksums.txt"), read("checksums.txt.minisig"), read("backsight_0.950.0_windows_amd64.zip")
	if !verify(t, sums, pub, sig) {
		t.Fatal("checksums.txt.minisig does not verify with the printed key")
	}
	var s minisign.Signature
	if err := s.UnmarshalText(sig); err != nil {
		t.Fatal(err)
	}
	if s.Algorithm != minisign.HashEdDSA || !strings.Contains("\t"+s.TrustedComment+"\t", "\tfile:checksums.txt\t") {
		t.Errorf("signature algorithm %v, trusted comment %q: want prehashed, naming checksums.txt", s.Algorithm, s.TrustedComment)
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	want := sum(body) + "  backsight_0.950.0_windows_amd64.exe\n" + sum(zipped) + "  backsight_0.950.0_windows_amd64.zip\n"
	if string(sums) != want {
		t.Errorf("checksums.txt:\n%s\nwant:\n%s", sums, want)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "backsight.exe" {
		t.Fatalf("zip members %v, want backsight.exe", zr.File)
	}
	f, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	_, _ = got.ReadFrom(f)
	if !bytes.Equal(got.Bytes(), body) {
		t.Error("the zip's backsight.exe is not the binary given")
	}
	// Another key does not verify it: the check above can fail.
	other, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if verify(t, sums, other, sig) {
		t.Error("another key verifies the signature")
	}
}

// verify checks sig over data, prehashed, as minisign -V does.
func verify(t *testing.T, data []byte, pub minisign.PublicKey, sig []byte) bool {
	t.Helper()
	r := minisign.NewReader(bytes.NewReader(data))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	return r.Verify(pub, sig)
}

func TestReleaseRefusals(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if err := run([]string{"keygen", key}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"keygen", key}, &bytes.Buffer{}); err == nil {
		t.Error("keygen replaced an existing key")
	}
	exe := filepath.Join(dir, "x.exe")
	_ = os.WriteFile(exe, []byte("x"), 0o644)
	for _, v := range []string{"", "../x", "1.0/2", ".hidden"} {
		if err := run([]string{"release", key, exe, v, filepath.Join(dir, "rel")}, &bytes.Buffer{}); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
	if err := run([]string{"sign"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("an unknown command: %v", err)
	}
}
