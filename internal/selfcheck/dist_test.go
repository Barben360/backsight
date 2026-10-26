package selfcheck

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/licenses"
	"github.com/Barben360/backsight/internal/signing"
)

// TestGoreleaserDist checks a real goreleaser release directory, signed
// by minisign with a throwaway key: CI's snapshot job builds one and sets
// BACKSIGHT_TEST_DIST (the dist directory) and BACKSIGHT_TEST_DIST_PUBKEY
// (the throwaway public key file). Every file of checksums.txt verifies,
// and this machine's bare binary, installed as install.sh does, verifies
// itself.
func TestGoreleaserDist(t *testing.T) {
	dist, pubFile := os.Getenv("BACKSIGHT_TEST_DIST"), os.Getenv("BACKSIGHT_TEST_DIST_PUBKEY")
	if dist == "" || pubFile == "" {
		t.Skip("BACKSIGHT_TEST_DIST and BACKSIGHT_TEST_DIST_PUBKEY are not set (CI's snapshot job sets them)")
	}
	pub := read(t, pubFile)
	_, key, _ := strings.Cut(strings.TrimSpace(string(pub)), "\n")
	old := signing.TestKey
	signing.TestKey = key
	t.Cleanup(func() { signing.TestKey = old })

	sums := read(t, filepath.Join(dist, ChecksumsFile))
	if _, err := signing.VerifyChecksums(sums, read(t, filepath.Join(dist, SignatureFile))); err != nil {
		t.Fatalf("goreleaser's checksums.txt: %v", err)
	}

	// artifacts.json maps each published name to its file in dist.
	var artifacts []struct{ Name, Path, Type string }
	if err := json.Unmarshal(read(t, filepath.Join(dist, "artifacts.json")), &artifacts); err != nil {
		t.Fatal(err)
	}
	where := map[string]string{}
	for _, a := range artifacts {
		if a.Type == "Archive" || a.Type == "Binary" {
			where[a.Name] = filepath.Join(dist, "..", a.Path)
		}
	}
	var meta struct{ Version string }
	if err := json.Unmarshal(read(t, filepath.Join(dist, "metadata.json")), &meta); err != nil {
		t.Fatal(err)
	}
	entries := strings.Split(strings.TrimSpace(string(sums)), "\n")
	if len(entries) != 8 {
		t.Errorf("%d entries, want 4 archives and 4 bare binaries:\n%s", len(entries), sums)
	}
	for _, line := range entries {
		_, name, _ := strings.Cut(line, "  ")
		f, err := os.Open(where[name])
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err := signing.ArchiveSum(sums, name, f); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		_ = f.Close()
	}

	// This machine's binary, as install.sh lays it out.
	bin := BinaryName(meta.Version, runtime.GOOS, runtime.GOARCH)
	if where[bin] == "" {
		t.Fatalf("no bare binary %s in the release", bin)
	}
	dir := filepath.Join(t.TempDir(), "bin", meta.Version)
	_ = os.MkdirAll(dir, 0o755)
	for name, src := range map[string]string{"backsight": where[bin], ChecksumsFile: filepath.Join(dist, ChecksumsFile), SignatureFile: filepath.Join(dist, SignatureFile)} {
		if err := os.WriteFile(filepath.Join(dir, name), read(t, src), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := Check(filepath.Join(dir, "backsight"), meta.Version, runtime.GOOS, runtime.GOARCH, time.Now()); got.Status != Verified {
		t.Errorf("the released binary does not verify itself: %+v", got)
	}
}

// TestGoreleaserDistLicenses: every archive of the same release directory
// carries THIRD_PARTY_LICENSES at its root, and this machine's bare binary
// prints the same text (version --licenses): both are the committed file.
func TestGoreleaserDistLicenses(t *testing.T) {
	dist := os.Getenv("BACKSIGHT_TEST_DIST")
	if dist == "" {
		t.Skip("BACKSIGHT_TEST_DIST is not set (CI's snapshot job sets it)")
	}
	var artifacts []struct{ Name, Path, Type, Goos, Goarch string }
	if err := json.Unmarshal(read(t, filepath.Join(dist, "artifacts.json")), &artifacts); err != nil {
		t.Fatal(err)
	}
	archives, bin := 0, ""
	for _, a := range artifacts {
		path := filepath.Join(dist, "..", a.Path)
		switch {
		case a.Type == "Archive":
			archives++
			if got := archiveFile(t, path, "THIRD_PARTY_LICENSES"); got != licenses.Text {
				t.Errorf("%s: THIRD_PARTY_LICENSES is not the committed file (%d bytes, want %d)", a.Name, len(got), len(licenses.Text))
			}
		case a.Type == "Binary" && a.Goos == runtime.GOOS && a.Goarch == runtime.GOARCH:
			bin = path
		}
	}
	if archives != 4 {
		t.Errorf("%d archives, want 4", archives)
	}
	if bin == "" {
		t.Fatal("no bare binary for this machine")
	}
	out, err := exec.Command(bin, "version", "--licenses").Output()
	if err != nil || string(out) != licenses.Text {
		t.Errorf("%s version --licenses: %v, %d bytes, want the committed file (%d)", bin, err, len(out), len(licenses.Text))
	}
}

// archiveFile returns the content of name in a .tar.gz or .zip archive
// ("" when it is not there).
func archiveFile(t *testing.T, path, name string) string {
	t.Helper()
	if strings.HasSuffix(path, ".zip") {
		z, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = z.Close() }()
		for _, f := range z.File {
			if f.Name == name {
				r, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = r.Close() }()
				b, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
		}
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == name {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
