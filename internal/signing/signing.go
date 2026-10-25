// Package signing verifies releases (decision 0027): checksums.txt carries
// a minisign signature by the current or the next embedded public key (the
// next one lets a key rotation happen without a trust reset), and every
// release file is checked against its SHA-256 in checksums.txt.
package signing

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"aead.dev/minisign"
)

var (
	//go:embed keys/current.pub
	Current string
	//go:embed keys/next.pub
	Next string
	// TestKey is a third trusted key, the base64 line of a minisign public
	// key. Only test builds set it, with
	// -ldflags "-X github.com/Barben360/backsight/internal/signing.TestKey=RW…";
	// release builds leave it empty and the release pipeline fails when a
	// release binary reports it (decision 0067).
	TestKey string
)

// Key is one trusted public key: its minisign id (hex, as minisign prints
// it) and its role, current, next or test.
type Key struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

var (
	// ErrUnsigned: no signature, or an empty one.
	ErrUnsigned = errors.New("checksums.txt is not signed")
	// ErrUntrustedKey: signed by a key this binary does not embed.
	ErrUntrustedKey = errors.New("checksums.txt is signed by a key this binary does not trust")
	// ErrLegacySignature: a non-prehashed (minisign -l) signature;
	// releases are signed prehashed, as the install script requires.
	ErrLegacySignature = errors.New("checksums.txt has a legacy (not prehashed) minisign signature")
	// ErrBadSignature: malformed, or not valid for the file or for its
	// trusted comment.
	ErrBadSignature = errors.New("checksums.txt does not match its signature")
	// ErrWrongFile: a valid signature made for another file.
	ErrWrongFile = errors.New("the signature was made for another file than checksums.txt")
	// ErrMalformed: checksums.txt is not goreleaser's "<sha256>  <name>" lines.
	ErrMalformed = errors.New("checksums.txt is malformed")
	// ErrMissing: checksums.txt has no entry for the file.
	ErrMissing = errors.New("checksums.txt has no entry for the file")
	// ErrMismatch: the file's SHA-256 differs from its entry.
	ErrMismatch = errors.New("checksum mismatch")
)

type trusted struct {
	Key
	pub minisign.PublicKey
	ok  bool
}

func trustedKeys() []trusted {
	ks := []trusted{parse(Current, "current"), parse(Next, "next")}
	if TestKey != "" {
		ks = append(ks, parse(TestKey, "test"))
	}
	return ks
}

func parse(text, role string) trusted {
	var p minisign.PublicKey
	if err := p.UnmarshalText([]byte(strings.TrimSpace(text))); err != nil {
		return trusted{Key: Key{"invalid", role}}
	}
	return trusted{Key{formatID(p.ID()), role}, p, true}
}

// formatID formats a key id as minisign prints it.
func formatID(id uint64) string { return fmt.Sprintf("%016X", id) }

// Keys lists the trusted keys: current, next, and the test key when this
// is a test build. A key that does not parse is listed with the id
// "invalid" and trusts nothing.
func Keys() []Key {
	var out []Key
	for _, k := range trustedKeys() {
		out = append(out, k.Key)
	}
	return out
}

// VerifyChecksums checks sig, the content of checksums.txt.minisig, against
// checksums with the trusted keys. It returns the id of the key that
// signed. Only a prehashed signature (what minisign -S makes by default,
// and what the install script accepts) whose trusted comment names
// checksums.txt passes; the trusted comment's own signature is checked.
func VerifyChecksums(checksums, sig []byte) (keyID string, err error) {
	if len(bytes.TrimSpace(sig)) == 0 {
		return "", ErrUnsigned
	}
	var s minisign.Signature
	if err := s.UnmarshalText(sig); err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	var key *trusted
	for _, k := range trustedKeys() {
		if k.ok && k.pub.ID() == s.KeyID {
			key = &k
			break
		}
	}
	if key == nil {
		return "", fmt.Errorf("%w (key %s)", ErrUntrustedKey, formatID(s.KeyID))
	}
	if s.Algorithm != minisign.HashEdDSA {
		return "", ErrLegacySignature
	}
	// Verify checks the signature of the file, then the global signature
	// over that signature and the trusted comment.
	if !minisign.Verify(key.pub, checksums, sig) {
		return "", ErrBadSignature
	}
	if !hasField(s.TrustedComment, "file:checksums.txt") {
		return "", fmt.Errorf("%w (trusted comment %q)", ErrWrongFile, s.TrustedComment)
	}
	return key.ID, nil
}

// hasField: minisign's trusted comment is tab-separated fields
// (timestamp:…, file:…, hashed).
func hasField(comment, field string) bool {
	for _, f := range strings.Split(comment, "\t") {
		if f == field {
			return true
		}
	}
	return false
}

// ArchiveSum checks that r's SHA-256 is the entry for name in checksums,
// which VerifyChecksums must have accepted first. checksums must be
// goreleaser's format throughout: "<64 lowercase hex>  <name>" lines, each
// name once.
func ArchiveSum(checksums []byte, name string, r io.Reader) error {
	want, err := entry(checksums, name)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("hashing %s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%w: %s has sha256 %s, checksums.txt says %s", ErrMismatch, name, got, want)
	}
	return nil
}

func entry(checksums []byte, name string) (string, error) {
	seen := map[string]bool{}
	var want string
	for i, line := range strings.Split(strings.TrimSuffix(string(checksums), "\n"), "\n") {
		sum, file, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" || file == "" || strings.ContainsAny(file, " \t\r") {
			return "", fmt.Errorf("%w: line %d %q", ErrMalformed, i+1, line)
		}
		if seen[file] {
			return "", fmt.Errorf("%w: %s is listed twice", ErrMalformed, file)
		}
		seen[file] = true
		if file == name {
			want = sum
		}
	}
	if want == "" {
		return "", fmt.Errorf("%w: %q", ErrMissing, name)
	}
	return want, nil
}
