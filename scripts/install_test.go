// Package scripts holds the tests of the shell scripts shipped with
// backsight: install.sh runs against a release built and signed here.
package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/blake2b"
)

type key struct {
	id   []byte
	priv ed25519.PrivateKey
	pub  string // minisign public key line
}

func newKey(t *testing.T) key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	return key{id, priv, base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), id...), pub...))}
}

// sign writes file.minisig as minisign does by default: a prehashed
// (BLAKE2b-512) signature, then a signature over it and the trusted comment.
func (k key) sign(t *testing.T, file, comment string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	h := blake2b.Sum512(data)
	sig := ed25519.Sign(k.priv, h[:])
	global := ed25519.Sign(k.priv, append(append([]byte{}, sig...), comment...))
	lines := "untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(append(append([]byte("ED"), k.id...), sig...)) + "\n" +
		"trusted comment: " + comment + "\n" + base64.StdEncoding.EncodeToString(global) + "\n"
	if err := os.WriteFile(file+".minisig", []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

var version = "9.9.9"

func goos() string { return runtime.GOOS }

// release builds a release directory for this machine, signed by k.
func release(t *testing.T, k key) (dir, archive string) {
	t.Helper()
	return releaseIn(t, t.TempDir(), k, false)
}

// releaseIn builds the release in dir; link makes the backsight member a
// symlink, as a hostile archive could.
func releaseIn(t *testing.T, dir string, k key, link bool) (string, string) {
	t.Helper()
	archive := "backsight_" + version + "_" + goos() + "_" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho fake backsight " + version + "\n")
	if link {
		_ = tw.WriteHeader(&tar.Header{Name: "backsight", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"})
	} else {
		_ = tw.WriteHeader(&tar.Header{Name: "backsight", Mode: 0o755, Size: int64(len(body))})
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(filepath.Join(dir, archive), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	other := "backsight_" + version + "_plan9_mips.tar.gz"
	checksums := hex.EncodeToString(sum[:]) + "  " + archive + "\n" + strings.Repeat("0", 64) + "  " + other + "\n"
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}
	k.sign(t, filepath.Join(dir, "checksums.txt"), "timestamp:1792845259\tfile:checksums.txt\thashed")
	return dir, archive
}

// toolsWithout is a PATH holding the tools install.sh uses, minisign left
// out so that the openssl path runs.
func toolsWithout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"sh", "openssl", "tar", "gzip", "awk", "sed", "od", "base64", "head", "tail", "cut",
		"sha256sum", "shasum", "mktemp", "mkdir", "rm", "cp", "install", "mv", "ln", "uname", "cat", "basename", "tr"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(dir, tool))
		}
	}
	return dir
}

// installScript runs install.sh as its users do, piped into sh. The test
// reads the script itself, so Go's test cache keys on its content: an
// edited script is never served an old pass (a child's reads are not
// tracked).
func installScript(t *testing.T) *exec.Cmd {
	t.Helper()
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = bytes.NewReader(src)
	return cmd
}

func install(t *testing.T, relDir, pubkey, path string) (home, out string, err error) {
	t.Helper()
	return installIn(t, t.TempDir(), relDir, pubkey, path)
}

func installIn(t *testing.T, home, relDir, pubkey, path string, env ...string) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl missing")
	}
	cmd := installScript(t)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + path, "BACKSIGHT_RELEASE_DIR=" + relDir, "BACKSIGHT_INSTALL_PUBKEY=" + pubkey}, env...)
	b, err := cmd.CombinedOutput()
	return home, string(b), err
}

func dataDir(home string) string {
	if goos() == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "backsight")
	}
	return filepath.Join(home, ".local", "share", "backsight")
}

func installedBinary(home string) string {
	return filepath.Join(dataDir(home), "bin", version, "backsight")
}

// pathEntry checks the command on PATH: link → the shim <data>/bin/current
// (decision 0067 block review: an update moves the shim, so the PATH
// command follows it, and pruning old binaries never breaks it) → binary.
func pathEntry(t *testing.T, link, data, binary string) {
	t.Helper()
	shim := filepath.Join(data, "bin", "current")
	if got, err := os.Readlink(link); err != nil || got != shim {
		t.Errorf("%s → %q (%v), want the shim %s", link, got, err, shim)
	}
	if got, err := os.Readlink(shim); err != nil || got != binary {
		t.Errorf("%s → %q (%v), want %s", shim, got, err, binary)
	}
}

func TestInstallVerifiesAndInstallsWithOpenSSL(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home, out, err := install(t, dir, k.pub, toolsWithout(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	link := filepath.Join(home, ".local", "bin", "backsight")
	pathEntry(t, link, dataDir(home), installedBinary(home))
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight "+version+"\n" {
		t.Errorf("installed binary: %q %v", run, err)
	}
	for _, s := range []string{"backsight " + version + " installed", "export PATH=", "next: backsight setup"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestInstallWithMinisign(t *testing.T) {
	if _, err := exec.LookPath("minisign"); err != nil {
		// On CI a skip would silently drop install.sh's minisign path, the
		// one its users take when they have minisign.
		if os.Getenv("CI") != "" {
			t.Fatal("minisign missing on CI: install.sh's minisign path cannot be proved")
		}
		t.Skip("minisign missing")
	}
	k := newKey(t)
	dir, _ := release(t, k)
	if _, out, err := install(t, dir, k.pub, os.Getenv("PATH")); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	arch := filepath.Join(dir, "backsight_"+version+"_"+goos()+"_"+runtime.GOARCH+".tar.gz")
	_ = os.WriteFile(arch, []byte("swapped archive"), 0o644)
	if _, out, err := install(t, dir, k.pub, os.Getenv("PATH")); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Errorf("minisign path accepted a swapped archive: %v\n%s", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte("tampered\n"), 0o644)
	if _, out, err := install(t, dir, k.pub, os.Getenv("PATH")); err == nil || !strings.Contains(out, "bad signature") {
		t.Errorf("minisign accepted a tampered file: %v\n%s", err, out)
	}
}

func TestInstallRefusesWhatDoesNotVerify(t *testing.T) {
	path := toolsWithout(t)
	cases := []struct {
		name   string
		tamper func(t *testing.T, dir, archive string, k key) string // returns the key to trust
		want   string
	}{
		{"archive changed", func(t *testing.T, dir, archive string, k key) string {
			_ = os.WriteFile(filepath.Join(dir, archive), []byte("not the signed archive"), 0o644)
			return k.pub
		}, "checksum mismatch"},
		{"checksums changed", func(t *testing.T, dir, _ string, k key) string {
			f, _ := os.OpenFile(filepath.Join(dir, "checksums.txt"), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("\n")
			_ = f.Close()
			return k.pub
		}, "bad signature"},
		{"another key", func(t *testing.T, _, _ string, _ key) string { return newKey(t).pub }, "signed by another key"},
		{"trusted comment changed", func(t *testing.T, dir, _ string, k key) string {
			p := filepath.Join(dir, "checksums.txt.minisig")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.Replace(string(b), "timestamp:1792845259", "timestamp:1792845260", 1)), 0o644)
			return k.pub
		}, "bad trusted comment signature"},
		{"legacy signature", func(t *testing.T, dir, _ string, k key) string {
			// "Ed": the signature over the file itself, not its prehash
			f := filepath.Join(dir, "checksums.txt")
			data, _ := os.ReadFile(f)
			sig := ed25519.Sign(k.priv, data)
			comment := "timestamp:1\tfile:checksums.txt"
			global := ed25519.Sign(k.priv, append(append([]byte{}, sig...), comment...))
			_ = os.WriteFile(f+".minisig", []byte("untrusted comment: x\n"+base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), k.id...), sig...))+
				"\ntrusted comment: "+comment+"\n"+base64.StdEncoding.EncodeToString(global)+"\n"), 0o644)
			return k.pub
		}, "not a prehashed"},
		{"signature made for another file", func(t *testing.T, dir, _ string, k key) string {
			k.sign(t, filepath.Join(dir, "checksums.txt"), "timestamp:1792845259\tfile:old-checksums.txt\thashed")
			return k.pub
		}, "made for another file"},
		{"archive name escaping the directory", func(t *testing.T, dir, archive string, k key) string {
			p := filepath.Join(dir, "checksums.txt")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(b), archive, "backsight_../"+archive)), 0o644)
			k.sign(t, p, "timestamp:1\tfile:checksums.txt\thashed")
			return k.pub
		}, "unexpected archive name"},
		{"symlinked binary", func(t *testing.T, dir, _ string, k key) string {
			_, _ = releaseIn(t, dir, k, true)
			return k.pub
		}, "not a regular file"},
		{"missing signature", func(t *testing.T, dir, _ string, k key) string {
			_ = os.Remove(filepath.Join(dir, "checksums.txt.minisig"))
			return k.pub
		}, "checksums.txt.minisig is not in"},
		{"no archive for this machine", func(t *testing.T, dir, archive string, k key) string {
			p := filepath.Join(dir, "checksums.txt")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(b), archive, "backsight_9.9.9_plan9_arm.tar.gz")), 0o644)
			k.sign(t, p, "timestamp:1\tfile:checksums.txt\thashed")
			return k.pub
		}, "no archive for"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKey(t)
			dir, archive := release(t, k)
			trust := c.tamper(t, dir, archive, k)
			home, out, err := install(t, dir, trust, path)
			if err == nil || !strings.Contains(out, c.want) {
				t.Errorf("want a refusal with %q, got %v\n%s", c.want, err, out)
			}
			if _, err := os.Stat(installedBinary(home)); err == nil {
				t.Error("a refused release was installed")
			}
		})
	}
}

func TestInstallUnderBackSightHomeAndUpgrade(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	path := toolsWithout(t)
	home := t.TempDir()
	bh := filepath.Join(home, "bs")
	if _, out, err := installIn(t, home, dir, k.pub, path, "BACKSIGHT_HOME="+bh); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	link := filepath.Join(bh, "bin", "backsight")
	pathEntry(t, link, filepath.Join(bh, "data"), filepath.Join(bh, "data", "bin", version, "backsight"))
	// A newer release replaces the link, the older binary stays in place.
	old := version
	version = "9.9.10"
	t.Cleanup(func() { version = old })
	dir2, _ := release(t, k)
	if _, out, err := installIn(t, home, dir2, k.pub, path, "BACKSIGHT_HOME="+bh); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight 9.9.10\n" {
		t.Errorf("after the upgrade the link runs %q (%v)", run, err)
	}
	pathEntry(t, link, filepath.Join(bh, "data"), filepath.Join(bh, "data", "bin", "9.9.10", "backsight"))
	if _, err := os.Stat(filepath.Join(bh, "data", "bin", old, "backsight")); err != nil {
		t.Error("the previous binary was removed")
	}
}

// A newer release that does not verify leaves the working install alone.
func TestARefusedUpgradeKeepsTheWorkingInstall(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	path := toolsWithout(t)
	home, out, err := install(t, dir, k.pub, path)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	good := installedBinary(home)
	old := version
	version = "9.9.10"
	t.Cleanup(func() { version = old })
	dir2, archive := release(t, k)
	_ = os.WriteFile(filepath.Join(dir2, archive), []byte("not the signed archive"), 0o644)
	if _, out, err := installIn(t, home, dir2, k.pub, path); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("the tampered 9.9.10 was not refused: %v\n%s", err, out)
	}
	link := filepath.Join(home, ".local", "bin", "backsight")
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight "+old+"\n" {
		t.Errorf("after the refused upgrade the link runs %q (%v), want %s", run, err, old)
	}
	if _, err := os.Stat(installedBinary(home)); err == nil {
		t.Error("the refused 9.9.10 was installed")
	}
	if _, err := os.Stat(good); err != nil {
		t.Errorf("the working %s binary is gone: %v", old, err)
	}
}

// The verified checksums.txt and its signature stay next to the binary:
// the binary checks itself against them on its first run (SPEC §11.1).
func TestInstallKeepsTheSignedChecksums(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home, out, err := install(t, dir, k.pub, toolsWithout(t))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"checksums.txt", "checksums.txt.minisig"} {
		want, _ := os.ReadFile(filepath.Join(dir, f))
		got, err := os.ReadFile(filepath.Join(filepath.Dir(installedBinary(home)), f))
		if err != nil || len(want) == 0 || string(got) != string(want) {
			t.Errorf("%s next to the binary: %v", f, err)
		}
	}
}

// An install.sh from before decision 0067's block review linked the PATH
// command to a versioned binary: a new install points it at the shim.
func TestInstallPointsAnOlderPathLinkAtTheShim(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	home := t.TempDir()
	old := filepath.Join(dataDir(home), "bin", "9.9.8", "backsight")
	link := filepath.Join(home, ".local", "bin", "backsight")
	for _, d := range []string{filepath.Dir(old), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho fake backsight 9.9.8\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	if _, out, err := installIn(t, home, dir, k.pub, toolsWithout(t)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pathEntry(t, link, dataDir(home), installedBinary(home))
	if run, err := exec.Command(link).Output(); err != nil || string(run) != "fake backsight "+version+"\n" {
		t.Errorf("the PATH command runs %q (%v)", run, err)
	}
}

// github fakes api.github.com and github.com's release downloads on one
// local HTTPS host (install.sh's BACKSIGHT_INSTALL_GITHUB), so the lookup of
// the newest release runs without the network.
type github struct {
	srv *httptest.Server
	ca  string // the server's certificate, for curl's CURL_CA_BUNDLE

	mu        sync.Mutex
	lookups   []*http.Request
	downloads []string
}

// newGitHub answers the release list with status and body, and serves the
// files of relDir under tag.
func newGitHub(t *testing.T, status int, body, tag, relDir string) *github {
	t.Helper()
	g := &github{}
	dl := "/Barben360/backsight/releases/download/" + tag + "/"
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/Barben360/backsight/releases":
			g.lookups = append(g.lookups, r)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case strings.HasPrefix(r.URL.Path, dl) && relDir != "":
			g.downloads = append(g.downloads, r.URL.Path)
			http.ServeFile(w, r, filepath.Join(relDir, filepath.Base(r.URL.Path)))
		default:
			g.downloads = append(g.downloads, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	g.ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(g.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return g
}

// withCurl is toolsWithout plus curl, which the download path needs.
func withCurl(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl missing")
	}
	dir := toolsWithout(t)
	if err := os.Symlink(p, filepath.Join(dir, "curl")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// install runs install.sh with no release directory: it finds the
// release through g.
func (g *github) install(t *testing.T, pubkey string, env ...string) (home, out string, err error) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl missing")
	}
	home = t.TempDir()
	cmd := installScript(t)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + withCurl(t), "BACKSIGHT_INSTALL_PUBKEY=" + pubkey,
		"BACKSIGHT_INSTALL_GITHUB=" + g.srv.URL, "CURL_CA_BUNDLE=" + g.ca}, env...)
	b, err := cmd.CombinedOutput()
	return home, string(b), err
}

// A release as the API lists it: a pre-release, with decoys a careless
// parser would take for its tag (a name equal to "tag_name", an escaped
// "tag_name" in a string, nested objects).
func releaseJSON(tag string, pretty bool) string {
	s := `[{"url":"https://api.github.com/repos/Barben360/backsight/releases/1",` +
		`"author":{"login":"Barben360","id":1,"type":"User"},` +
		`"name":"tag_name","node_id":"RE_1","target_commitish":"the \"tag_name\": \"v0.0.1\" one",` +
		`"tag_name":"` + tag + `","draft":false,"prerelease":true,` +
		`"assets":[{"name":"checksums.txt","uploader":{"login":"x"}}],"body":"\"tag_name\":\"v0.0.2\""}]`
	if pretty {
		s = strings.NewReplacer(`[{`, "[\n  {\n    ", `,"`, ",\n    \"", `":`, `": `, `}]`, "\n  }\n]\n").Replace(s)
	}
	return s
}

// Without a release URL, install.sh takes the newest release from the API,
// pre-releases included (GitHub's /releases/latest skips them), and still
// verifies it.
func TestInstallFindsTheNewestReleaseIncludingPreReleases(t *testing.T) {
	for _, pretty := range []bool{false, true} {
		t.Run(map[bool]string{false: "compact", true: "pretty"}[pretty], func(t *testing.T) {
			k := newKey(t)
			dir, archive := release(t, k)
			g := newGitHub(t, http.StatusOK, releaseJSON("v9.9.9-m5", pretty), "v9.9.9-m5", dir)
			home, out, err := g.install(t, k.pub)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			pathEntry(t, filepath.Join(home, ".local", "bin", "backsight"), dataDir(home), installedBinary(home))
			if !strings.Contains(out, "v9.9.9-m5") {
				t.Errorf("output does not name the release it found:\n%s", out)
			}
			if len(g.lookups) != 1 {
				t.Fatalf("%d lookups, want 1", len(g.lookups))
			}
			r := g.lookups[0]
			if r.URL.Query().Get("per_page") != "1" || r.Header.Get("Authorization") != "" {
				t.Errorf("lookup %s with Authorization %q: want per_page=1 and no token", r.URL, r.Header.Get("Authorization"))
			}
			want := []string{"checksums.txt", "checksums.txt.minisig", archive}
			for i, w := range want {
				want[i] = "/Barben360/backsight/releases/download/v9.9.9-m5/" + w
			}
			if strings.Join(g.downloads, " ") != strings.Join(want, " ") {
				t.Errorf("downloads %v, want %v", g.downloads, want)
			}
		})
	}
}

// The release found is verified as any other: a tampered archive is refused.
func TestInstallRefusesAFoundReleaseThatDoesNotVerify(t *testing.T) {
	k := newKey(t)
	dir, archive := release(t, k)
	_ = os.WriteFile(filepath.Join(dir, archive), []byte("not the signed archive"), 0o644)
	g := newGitHub(t, http.StatusOK, releaseJSON("v9.9.9-m5", false), "v9.9.9-m5", dir)
	home, out, err := g.install(t, k.pub)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Errorf("want a checksum refusal, got %v\n%s", err, out)
	}
	if _, err := os.Stat(installedBinary(home)); err == nil {
		t.Error("a refused release was installed")
	}
}

// A failed lookup stops before any download, says why, and names the way
// around it.
func TestInstallLookupFailureNamesTheReleaseURL(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"repository not found (private)", http.StatusNotFound, `{"message":"Not Found"}`, "cannot list the releases at"},
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`, "cannot list the releases at"},
		{"no release yet", http.StatusOK, "[]\n", "no release found at"},
		{"not JSON", http.StatusOK, "<html>maintenance</html>", "no release found at"},
		{"tag escaping the URL", http.StatusOK, releaseJSON("v1/../../evil", false), "unexpected release tag at"},
		{"tag with a query", http.StatusOK, releaseJSON("v1?x=1", false), "unexpected release tag at"},
		{"escaped tag", http.StatusOK, releaseJSON(`v1\u002e0`, false), "unexpected release tag at"},
		{"dot-dot tag", http.StatusOK, releaseJSON("..", false), "unexpected release tag at"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKey(t)
			dir, _ := release(t, k)
			g := newGitHub(t, c.status, c.body, "v9.9.9-m5", dir)
			home, out, err := g.install(t, k.pub)
			if err == nil || !strings.Contains(out, c.want) || !strings.Contains(out, "BACKSIGHT_RELEASE_URL") {
				t.Errorf("want a failure saying %q and naming BACKSIGHT_RELEASE_URL, got %v\n%s", c.want, err, out)
			}
			if len(g.lookups) != 1 {
				t.Errorf("%d lookups, want 1: the failure must come from the lookup", len(g.lookups))
			}
			if len(g.downloads) != 0 {
				t.Errorf("downloaded %v after a failed lookup", g.downloads)
			}
			if _, err := os.Stat(installedBinary(home)); err == nil {
				t.Error("installed without a release")
			}
		})
	}
}

// BACKSIGHT_RELEASE_URL names the release: no lookup.
func TestInstallWithAReleaseURLSkipsTheLookup(t *testing.T) {
	k := newKey(t)
	dir, _ := release(t, k)
	g := newGitHub(t, http.StatusInternalServerError, "", "v9.9.8", dir)
	home, out, err := g.install(t, k.pub, "BACKSIGHT_RELEASE_URL="+g.srv.URL+"/Barben360/backsight/releases/download/v9.9.8")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(g.lookups) != 0 {
		t.Errorf("%d lookups with a release URL set", len(g.lookups))
	}
	if _, err := os.Stat(installedBinary(home)); err != nil {
		t.Errorf("not installed: %v", err)
	}
}

// The version taken from the archive name names the install directory: an
// empty one, one starting with a dot or one with another character would
// install outside <data>/bin/<version>, so it is refused before anything
// is written.
func TestInstallRefusesAnUnexpectedVersion(t *testing.T) {
	for _, v := range []string{"..", "", ".9", "9.9.9~1"} {
		t.Run("version "+v, func(t *testing.T) {
			old := version
			version = v
			t.Cleanup(func() { version = old })
			k := newKey(t)
			dir, _ := release(t, k)
			home, out, err := install(t, dir, k.pub, toolsWithout(t))
			if err == nil || !strings.Contains(out, "unexpected release version") {
				t.Fatalf("version %q: want a refusal, got %v\n%s", v, err, out)
			}
			if _, err := os.Stat(dataDir(home)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("version %q: %s written (%v)", v, dataDir(home), err)
			}
			if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "backsight")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("version %q: the PATH link was made (%v)", v, err)
			}
		})
	}
}
