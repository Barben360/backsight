package signing

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aead.dev/minisign"
)

// The embedded keys must be the ones published at the repository root.
func TestEmbeddedKeysMatchRepository(t *testing.T) {
	for file, got := range map[string]string{"../../minisign.pub": Current, "../../minisign.next.pub": Next} {
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from the embedded key", file)
		}
	}
	if Current == Next {
		t.Error("the next key must differ from the current one")
	}
}

func TestKeyIDs(t *testing.T) {
	for _, k := range []string{Current, Next} {
		if !strings.HasPrefix(k, "untrusted comment: minisign public key ") || len(strings.Split(strings.TrimSpace(k), "\n")) != 2 {
			t.Errorf("not a minisign public key file: %q", k)
		}
	}
}

// Keys reports the id minisign prints for each key (the release pipeline
// compares them with the .pub files' comments), current then next.
func TestKeysAreTheEmbeddedOnes(t *testing.T) {
	setTestKey(t, "")
	comment := func(k string) string {
		first, _, _ := strings.Cut(k, "\n")
		return strings.TrimPrefix(first, "untrusted comment: minisign public key ")
	}
	want := []Key{{comment(Current), "current"}, {comment(Next), "next"}}
	if got := Keys(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}

func TestTestKeyIsTrustedOnlyWhenSet(t *testing.T) {
	pub, priv := keypair(t)
	sums := []byte(sumLine("a", "backsight_1.0.0_linux_amd64.tar.gz"))
	sig := sign(t, priv, sums, "timestamp:1\tfile:checksums.txt\thashed")

	setTestKey(t, "")
	if _, err := VerifyChecksums(sums, sig); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("without a test key: %v, want ErrUntrustedKey", err)
	}
	setTestKey(t, pub.String())
	id, err := VerifyChecksums(sums, sig)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%016X", pub.ID()); id != want {
		t.Errorf("key id %q, want %q", id, want)
	}
	if ks := Keys(); len(ks) != 3 || ks[2] != (Key{id, "test"}) {
		t.Errorf("Keys() = %v, want the test key third", ks)
	}
}

// A test key that does not parse is still reported (the release check
// sees it) but trusts nothing.
func TestInvalidTestKeyIsReportedNotTrusted(t *testing.T) {
	setTestKey(t, "not a key")
	ks := Keys()
	if len(ks) != 3 || ks[2].Role != "test" || ks[2].ID != "invalid" {
		t.Errorf("Keys() = %v, want an invalid test key listed", ks)
	}
}

func TestVerifyChecksumsRefusals(t *testing.T) {
	pub, priv := keypair(t)
	_, other := keypair(t)
	setTestKey(t, pub.String())
	sums := []byte(sumLine("a", "backsight_1.0.0_linux_amd64.tar.gz") + sumLine("b", "backsight_1.0.0_linux_amd64"))
	good := sign(t, priv, sums, "timestamp:1\tfile:checksums.txt\thashed")
	if _, err := VerifyChecksums(sums, good); err != nil {
		t.Fatalf("a good signature is refused: %v", err)
	}
	tamperedComment := bytes.Replace(good, []byte("timestamp:1\t"), []byte("timestamp:2\t"), 1)
	legacy := minisign.SignWithComments(priv, sums, "timestamp:1\tfile:checksums.txt", "x")

	cases := []struct {
		name      string
		sums, sig []byte
		want      error
	}{
		{"unsigned", sums, nil, ErrUnsigned},
		{"blank signature", sums, []byte("\n \n"), ErrUnsigned},
		{"garbage signature", sums, []byte("untrusted comment: x\nnot base64\ntrusted comment: y\nz\n"), ErrBadSignature},
		{"another key", sums, sign(t, other, sums, "timestamp:1\tfile:checksums.txt\thashed"), ErrUntrustedKey},
		{"tampered checksums", append([]byte(sumLine("c", "evil.tar.gz")), sums...), good, ErrBadSignature},
		{"tampered trusted comment", sums, tamperedComment, ErrBadSignature},
		{"legacy signature", sums, legacy, ErrLegacySignature},
		{"made for another file", sums, sign(t, priv, sums, "timestamp:1\tfile:checksums.txt.bak\thashed"), ErrWrongFile},
		{"no file in the comment", sums, sign(t, priv, sums, "timestamp:1"), ErrWrongFile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := VerifyChecksums(c.sums, c.sig); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestArchiveSum(t *testing.T) {
	sums := []byte(sumLine("archive", "backsight_1.0.0_linux_amd64.tar.gz") + sumLine("binary", "backsight_1.0.0_linux_amd64"))
	if err := ArchiveSum(sums, "backsight_1.0.0_linux_amd64.tar.gz", strings.NewReader("archive")); err != nil {
		t.Errorf("the right content is refused: %v", err)
	}
	if err := ArchiveSum(sums, "backsight_1.0.0_linux_amd64", strings.NewReader("binary")); err != nil {
		t.Errorf("the second entry is refused: %v", err)
	}
	cases := []struct {
		name, sums, file, content string
		want                      error
	}{
		{"hash mismatch", string(sums), "backsight_1.0.0_linux_amd64.tar.gz", "archivE", ErrMismatch},
		{"another entry's content", string(sums), "backsight_1.0.0_linux_amd64.tar.gz", "binary", ErrMismatch},
		{"missing entry", string(sums), "backsight_1.0.0_darwin_arm64.tar.gz", "archive", ErrMissing},
		{"prefix of a name is not the name", string(sums), "backsight_1.0.0_linux_amd64.tar", "archive", ErrMissing},
		{"empty name", string(sums), "", "", ErrMissing},
		{"duplicate entry", string(sums) + sumLine("other", "backsight_1.0.0_linux_amd64.tar.gz"), "backsight_1.0.0_linux_amd64.tar.gz", "archive", ErrMalformed},
		{"short hash", "abcd  x\n", "x", "", ErrMalformed},
		{"one space", strings.Replace(sumLine("x", "x"), "  ", " ", 1), "x", "x", ErrMalformed},
		{"uppercase hash", strings.ToUpper(sumLine("x", "x")[:64]) + "  x\n", "x", "x", ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ArchiveSum([]byte(c.sums), c.file, strings.NewReader(c.content)); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestArchiveSumReadError(t *testing.T) {
	sums := []byte(sumLine("archive", "a.tar.gz"))
	boom := errors.New("disk gone")
	err := ArchiveSum(sums, "a.tar.gz", io.MultiReader(strings.NewReader("arch"), errReader{boom}))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the read error", err)
	}
}

// testdata/minisign is real output: `goreleaser release --snapshot` signing
// with minisign 0.12 and a throwaway key (its public half is test.pub; the
// secret half was never kept), plus checksums.txt.legacy.minisig made by
// `minisign -S -l` from the same key.
func TestRealMinisignFixture(t *testing.T) {
	dir := filepath.Join("testdata", "minisign")
	pub := strings.TrimSpace(lines(t, filepath.Join(dir, "test.pub"))[1])
	sums := read(t, filepath.Join(dir, "checksums.txt"))
	setTestKey(t, pub)
	if _, err := VerifyChecksums(sums, read(t, filepath.Join(dir, "checksums.txt.minisig"))); err != nil {
		t.Fatalf("the real minisign signature is refused: %v", err)
	}
	if _, err := VerifyChecksums(sums, read(t, filepath.Join(dir, "checksums.txt.legacy.minisig"))); !errors.Is(err, ErrLegacySignature) {
		t.Errorf("minisign -l output: %v, want ErrLegacySignature", err)
	}
	if _, err := VerifyChecksums(append(sums, '\n'), read(t, filepath.Join(dir, "checksums.txt.minisig"))); !errors.Is(err, ErrBadSignature) {
		t.Errorf("one byte added: %v, want ErrBadSignature", err)
	}
	setTestKey(t, "")
	if _, err := VerifyChecksums(sums, read(t, filepath.Join(dir, "checksums.txt.minisig"))); !errors.Is(err, ErrUntrustedKey) {
		t.Errorf("without the test key: %v, want ErrUntrustedKey", err)
	}
}

// The minisign CLI of this machine, when installed: a fresh keypair and the
// exact goreleaser invocation.
func TestMinisignCLI(t *testing.T) {
	bin, err := exec.LookPath("minisign")
	if err != nil {
		t.Skip("minisign is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("minisign %v: %v\n%s", args, err, out)
		}
	}
	key, pubFile, sums := filepath.Join(dir, "k.key"), filepath.Join(dir, "k.pub"), filepath.Join(dir, "checksums.txt")
	run("-G", "-W", "-p", pubFile, "-s", key)
	if err := os.WriteFile(sums, []byte(sumLine("a", "backsight_1.0.0_linux_amd64.tar.gz")), 0o600); err != nil {
		t.Fatal(err)
	}
	run("-S", "-s", key, "-m", sums, "-x", sums+".minisig")
	setTestKey(t, strings.TrimSpace(lines(t, pubFile)[1]))
	id, err := VerifyChecksums(read(t, sums), read(t, sums+".minisig"))
	if err != nil {
		t.Fatalf("minisign -S output refused: %v", err)
	}
	if want := strings.TrimPrefix(lines(t, pubFile)[0], "untrusted comment: minisign public key "); id != want {
		t.Errorf("key id %q, minisign says %q", id, want)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func keypair(t *testing.T) (minisign.PublicKey, minisign.PrivateKey) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// sign makes a prehashed signature, as minisign -S does by default.
func sign(t *testing.T, priv minisign.PrivateKey, msg []byte, trusted string) []byte {
	t.Helper()
	r := minisign.NewReader(bytes.NewReader(msg))
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	return r.SignWithComments(priv, trusted, "signature from minisign secret key")
}

func setTestKey(t *testing.T, k string) {
	old := TestKey
	TestKey = k
	t.Cleanup(func() { TestKey = old })
}

func sumLine(content, name string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:]) + "  " + name + "\n"
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lines(t *testing.T, path string) []string {
	return strings.Split(strings.TrimRight(string(read(t, path)), "\n"), "\n")
}

// The next key is trusted like the current one: a release signed after
// the rotation verifies, and its id is the next key's.
func TestTheNextKeyVerifies(t *testing.T) {
	pub, priv := keypair(t)
	setTestKey(t, "")
	old := Next
	Next = pub.String()
	t.Cleanup(func() { Next = old })
	sums := []byte(sumLine("a", "backsight_1.0.0_linux_amd64.tar.gz"))
	id, err := VerifyChecksums(sums, sign(t, priv, sums, "timestamp:1\tfile:checksums.txt\thashed"))
	if err != nil || id != fmt.Sprintf("%016X", pub.ID()) {
		t.Fatalf("signed by the next key: %q %v", id, err)
	}
	if ks := Keys(); len(ks) != 2 || ks[1] != (Key{id, "next"}) {
		t.Errorf("Keys() = %v, want the next key second", ks)
	}
}
