package scripts

// install.ps1 is the Windows bootstrap (decision 0080): neither Windows
// PowerShell 5.1 nor .NET checks Ed25519 or BLAKE2b, so the script carries
// its own minisign verifier. These tests dot-source it with
// BACKSIGHT_INSTALL_LIB=1 (its functions, no install) and run it on every
// PowerShell its users have: Windows PowerShell 5.1 and PowerShell 7 on
// Windows, pwsh elsewhere.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// powerShells are the PowerShells install.ps1 must run on that this
// machine has. On CI a missing one fails: a skip would silently drop the
// shell its users run (Windows runners ship both, Ubuntu's pwsh).
func powerShells(t *testing.T) []string {
	t.Helper()
	want := []string{"pwsh"}
	if runtime.GOOS == "windows" {
		want = []string{"powershell.exe", "pwsh"}
	}
	var have []string
	for _, s := range want {
		p, err := exec.LookPath(s)
		if err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s missing on CI: install.ps1 cannot be proved on it", s)
			}
			t.Logf("%s missing: not tested on it", s)
			continue
		}
		have = append(have, p)
	}
	if len(have) == 0 {
		t.Skip("no PowerShell on this machine")
	}
	return have
}

// shellName names a subtest after the shell: powershell or pwsh.
func shellName(shell string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(shell)), ".exe")
}

// psQuote is s as a PowerShell single-quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// psArgs runs a script file: no profile, no prompt, and on Windows past
// the execution policy, as a downloaded script is run.
func psArgs(file string) []string {
	args := []string{"-NoProfile", "-NonInteractive"}
	if runtime.GOOS == "windows" {
		args = append(args, "-ExecutionPolicy", "Bypass")
	}
	return append(args, "-File", file)
}

// The driver's helpers: hex in, hex out.
const psHex = `
function FromHex([string]$h) {
	$b = New-Object byte[] ($h.Length / 2)
	for ($i = 0; $i -lt $b.Length; $i++) { $b[$i] = [Convert]::ToByte($h.Substring(2 * $i, 2), 16) }
	return ,$b
}
function ToHex([byte[]]$b) { return (($b | ForEach-Object { $_.ToString('x2') }) -join '') }
`

// psLib dot-sources install.ps1 with BACKSIGHT_INSTALL_LIB=1 in shell,
// runs body and returns its output. The test reads the script itself, so
// Go's test cache keys on its content.
func psLib(t *testing.T, shell, body string) string {
	t.Helper()
	if _, err := os.ReadFile("install.ps1"); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(t.TempDir(), "driver.ps1")
	src := "$ErrorActionPreference = 'Stop'\n$env:BACKSIGHT_INSTALL_LIB = '1'\n. " + psQuote(script) + "\n" + psHex + body + "\n"
	if err := os.WriteFile(driver, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(shell, psArgs(driver)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", shellName(shell), err, out)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}

// The published script is ASCII: Windows PowerShell 5.1 reads a file
// without a byte order mark in the ANSI code page.
func TestInstallPs1IsASCII(t *testing.T) {
	b, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range b {
		if c > 0x7e || (c < 0x20 && c != '\n' && c != '\t' && c != '\r') {
			t.Fatalf("byte %d is 0x%02x: install.ps1 must be ASCII", i, c)
		}
	}
}

// Both install scripts trust the release key, verbatim from minisign.pub,
// and nothing else by default (minisign.next.pub's key is the binary's
// rotation path, not the bootstrap's).
func TestInstallScriptsEmbedTheReleaseKey(t *testing.T) {
	pub := strings.TrimSpace(strings.Split(readFile(t, "../minisign.pub"), "\n")[1])
	next := strings.TrimSpace(strings.Split(readFile(t, "../minisign.next.pub"), "\n")[1])
	for _, f := range []string{"install.sh", "install.ps1"} {
		src := readFile(t, f)
		if strings.Count(src, pub) != 1 {
			t.Errorf("%s does not embed minisign.pub's key %s once", f, pub)
		}
		if strings.Contains(src, next) {
			t.Errorf("%s embeds minisign.next.pub's key", f)
		}
	}
}

// BLAKE2b-512 (minisign's prehash): RFC 7693's "abc" vector, the empty
// input, and lengths around the 128-byte block, against x/crypto.
func TestBlake2bVectors(t *testing.T) {
	const (
		abc   = "ba80a53f981c4d0d6a2797b69f12f6e94c212f14685ac4b74b12bb6fdbffa2d17d87c5392aab792dc252d5de4533cc9518d38aa8dbf1925ab92386edd4009923"
		empty = "786a02f742015903c6c6fd852552d272912f4740e15847618a86e217f71f5419d25e1031afee585313896444934eb04b903a685b1448b755d56f701afe9be2ce"
	)
	type vec struct{ in, want string }
	vecs := []vec{{"", empty}, {hex.EncodeToString([]byte("abc")), abc}}
	for _, n := range []int{1, 64, 127, 128, 129, 255, 256, 257, 1000} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + 3)
		}
		sum := blake2b.Sum512(b)
		vecs = append(vecs, vec{hex.EncodeToString(b), hex.EncodeToString(sum[:])})
	}
	var body strings.Builder
	for _, v := range vecs {
		fmt.Fprintf(&body, "ToHex (Get-Blake2b512 (FromHex '%s'))\n", v.in)
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			got := strings.Fields(psLib(t, shell, body.String()))
			if len(got) != len(vecs) {
				t.Fatalf("%d hashes for %d inputs: %q", len(got), len(vecs), got)
			}
			for i, v := range vecs {
				if got[i] != v.want {
					t.Errorf("BLAKE2b-512 of %d bytes = %s, want %s", len(v.in)/2, got[i], v.want)
				}
			}
		})
	}
}

// Ed25519 verification as RFC 8032 and libsodium do it (cofactorless,
// canonical S only): RFC 8032's tests 1-3 and random keys verify; a flipped
// bit anywhere, and S + L (the same signature, non-canonical), do not.
func TestEd25519Vectors(t *testing.T) {
	type vec struct {
		pub, msg, sig string
		want          bool
		name          string
	}
	vecs := []vec{
		{"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", "",
			"e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b", true, "RFC 8032 test 1"},
		{"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c", "72",
			"92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00", true, "RFC 8032 test 2"},
		{"fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025", "af82",
			"6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a", true, "RFC 8032 test 3"},
	}
	flip := func(h string, byteIdx int) string {
		b, _ := hex.DecodeString(h)
		b[byteIdx] ^= 0x01
		return hex.EncodeToString(b)
	}
	for i, n := range []int{0, 1, 64, 200} {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		msg := make([]byte, n)
		_, _ = rand.Read(msg)
		sig := ed25519.Sign(priv, msg)
		v := vec{hex.EncodeToString(pub), hex.EncodeToString(msg), hex.EncodeToString(sig), true, fmt.Sprintf("random key %d, %d bytes", i, n)}
		vecs = append(vecs, v)
		if n > 0 {
			vecs = append(vecs, vec{v.pub, flip(v.msg, n-1), v.sig, false, v.name + ", message bit flipped"})
		}
		vecs = append(vecs,
			vec{v.pub, v.msg, flip(v.sig, 3), false, v.name + ", R bit flipped"},
			vec{v.pub, v.msg, flip(v.sig, 40), false, v.name + ", S bit flipped"},
			vec{flip(v.pub, 5), v.msg, v.sig, false, v.name + ", public key bit flipped"})
	}
	// S + L verifies with an implementation that reduces S modulo L:
	// RFC 8032 and libsodium refuse it.
	rfc1 := vecs[0]
	sig, _ := hex.DecodeString(rfc1.sig)
	s := new(big.Int).SetBytes(reverse(sig[32:]))
	l, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
	s.Add(s, l)
	sl := append(append([]byte{}, sig[:32]...), reverse(s.FillBytes(make([]byte, 32)))...)
	vecs = append(vecs, vec{rfc1.pub, rfc1.msg, hex.EncodeToString(sl), false, "RFC 8032 test 1 with S + L"})
	// The vectors themselves, through Go's verifier.
	for _, v := range vecs {
		pub, _ := hex.DecodeString(v.pub)
		msg, _ := hex.DecodeString(v.msg)
		sig, _ := hex.DecodeString(v.sig)
		if ed25519.Verify(pub, msg, sig) != v.want {
			t.Fatalf("%s: Go's verifier disagrees with the vector", v.name)
		}
	}

	var body strings.Builder
	for _, v := range vecs {
		fmt.Fprintf(&body, "[string](Test-Ed25519 (FromHex '%s') (FromHex '%s') (FromHex '%s'))\n", v.pub, v.msg, v.sig)
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			got := strings.Fields(psLib(t, shell, body.String()))
			if len(got) != len(vecs) {
				t.Fatalf("%d answers for %d vectors: %q", len(got), len(vecs), got)
			}
			for i, v := range vecs {
				if got[i] != map[bool]string{true: "True", false: "False"}[v.want] {
					t.Errorf("%s: Test-Ed25519 = %s, want %v", v.name, got[i], v.want)
				}
			}
		})
	}
}

func reverse(b []byte) []byte {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return r
}

// releaseKey is minisign.pub's key line.
func releaseKey(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(strings.Split(readFile(t, "../minisign.pub"), "\n")[1])
}

// fixture is a file of testdata/minisign, absolute.
func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "minisign", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// psMinisign is a driver line: Test-Minisign's answer, or REFUSED and its
// message.
func psMinisign(file, sig, key string) string {
	return fmt.Sprintf("try { $ok = Test-Minisign %s %s %s; 'ANSWER ' + $ok } catch { 'REFUSED ' + $_.Exception.Message }\n",
		psQuote(file), psQuote(sig), psQuote(key))
}

// A real release, v0.1.18-m7, verifies against the release key. Its time
// is logged: the whole check must stay well under 10 s on a Windows PC.
func TestMinisignRealRelease(t *testing.T) {
	body := "$t = [Diagnostics.Stopwatch]::StartNew()\n" +
		psMinisign(fixture(t, "v0.1.18-m7/checksums.txt"), fixture(t, "v0.1.18-m7/checksums.txt.minisig"), releaseKey(t)) +
		"'MS ' + $t.ElapsedMilliseconds\n"
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			out := psLib(t, shell, body)
			if !strings.Contains(out, "ANSWER True\n") {
				t.Fatalf("the real release does not verify:\n%s", out)
			}
			_, ms, _ := strings.Cut(out, "MS ")
			t.Logf("%s: %s ms", shellName(shell), strings.TrimSpace(ms))
		})
	}
}

// Every refusal says its own reason, as install.sh does.
func TestMinisignRefusals(t *testing.T) {
	dir := t.TempDir()
	realSig := readFile(t, fixture(t, "v0.1.18-m7/checksums.txt.minisig"))
	lines := strings.Split(realSig, "\n")
	// Key id mismatch: the release key's own valid signature, its key id
	// bytes changed. Only the key id check can refuse it.
	raw, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		t.Fatal(err)
	}
	raw[2] ^= 0xff
	idChanged := filepath.Join(dir, "id.minisig")
	writeLines(t, idChanged, lines[0], base64.StdEncoding.EncodeToString(raw), lines[2], lines[3])
	// The trusted comment changed: the file's signature still holds, the
	// global signature over it does not.
	commentChanged := filepath.Join(dir, "comment.minisig")
	writeLines(t, commentChanged, lines[0], lines[1], strings.Replace(lines[2], "timestamp:1793379364", "timestamp:1793379365", 1), lines[3])
	// No trusted comment line.
	noComment := filepath.Join(dir, "nocomment.minisig")
	writeLines(t, noComment, lines[0], lines[1])

	keyOf := func(f string) string {
		return strings.TrimSpace(strings.Split(readFile(t, fixture(t, f)), "\n")[1])
	}
	sums := fixture(t, "v0.1.18-m7/checksums.txt")
	cases := []struct {
		name, file, sig, key, want string
	}{
		{"tampered file", fixture(t, "tampered/checksums.txt"), fixture(t, "tampered/checksums.txt.minisig"), releaseKey(t),
			"checksums.txt: bad signature"},
		{"wrong key", fixture(t, "other-key/checksums.txt"), fixture(t, "other-key/checksums.txt.minisig"), releaseKey(t),
			"checksums.txt.minisig: signed by another key (key 15AF315BAB4E6A02)"},
		{"missing file:checksums.txt", fixture(t, "no-file-field/checksums.txt"), fixture(t, "no-file-field/checksums.txt.minisig"), keyOf("no-file-field/key.pub"),
			"checksums.txt.minisig was made for another file"},
		{"key id mismatch", sums, idChanged, releaseKey(t),
			"id.minisig: signed by another key (key CB9E8D3622A9CE21)"},
		{"trusted comment changed", sums, commentChanged, releaseKey(t),
			"comment.minisig: bad trusted comment signature"},
		{"no trusted comment", sums, noComment, releaseKey(t),
			"nocomment.minisig: no trusted comment"},
		{"legacy signature", fixture(t, "other-key/checksums.txt"), fixture(t, "other-key/checksums.txt.legacy.minisig"), keyOf("other-key/key.pub"),
			"checksums.txt.legacy.minisig: not a prehashed minisign signature"},
		{"bad public key", sums, fixture(t, "v0.1.18-m7/checksums.txt.minisig"), "RWTnot-a-key",
			"bad public key"},
	}
	var body strings.Builder
	for _, c := range cases {
		body.WriteString(psMinisign(c.file, c.sig, c.key))
	}
	// The fixtures' own keys do accept them: each refusal is the check
	// named, not a broken fixture.
	body.WriteString(psMinisign(fixture(t, "other-key/checksums.txt"), fixture(t, "other-key/checksums.txt.minisig"), keyOf("other-key/key.pub")))
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			got := strings.Split(strings.TrimSpace(psLib(t, shell, body.String())), "\n")
			if len(got) != len(cases)+1 {
				t.Fatalf("%d answers for %d cases:\n%s", len(got), len(cases)+1, strings.Join(got, "\n"))
			}
			for i, c := range cases {
				if got[i] != "REFUSED "+c.want {
					t.Errorf("%s: %q, want a refusal %q", c.name, got[i], c.want)
				}
			}
			if got[len(cases)] != "ANSWER True" {
				t.Errorf("other-key's fixture with its own key: %q, want ANSWER True", got[len(cases)])
			}
		})
	}
}

// install.ps1 installs on Windows only, and says where the other systems'
// installer is.
func TestInstallPs1RefusesAnUnsupportedSystem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the refusal of a system other than Windows")
	}
	for _, shell := range powerShells(t) {
		t.Run(shellName(shell), func(t *testing.T) {
			home := t.TempDir()
			out, err := runInstallPs1(t, shell, []string{"BACKSIGHT_HOME=" + filepath.Join(home, "bs"), "BACKSIGHT_RELEASE_DIR=" + fixture(t, "v0.1.18-m7")})
			if err == nil || !strings.Contains(out, "backsight install: unsupported system") || !strings.Contains(out, "install.sh") {
				t.Errorf("%v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(home, "bs")); err == nil {
				t.Error("BACKSIGHT_HOME written on a refused system")
			}
		})
	}
}

// runInstallPs1 runs install.ps1 as its users do, its text piped into
// Invoke-Expression (irm <url> | iex), with env added to this process's
// environment.
func runInstallPs1(t *testing.T, shell string, env []string) (string, error) {
	t.Helper()
	if _, err := os.ReadFile("install.ps1"); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-NoProfile", "-NonInteractive"}
	if runtime.GOOS == "windows" {
		args = append(args, "-ExecutionPolicy", "Bypass")
	}
	args = append(args, "-Command", "Get-Content -Raw -LiteralPath "+psQuote(script)+" | Invoke-Expression")
	cmd := exec.Command(shell, args...)
	// None of this machine's own overrides: only the test's.
	cmd.Env = append(os.Environ(), "BACKSIGHT_INSTALL_PUBKEY=", "BACKSIGHT_INSTALL_GITHUB=", "BACKSIGHT_RELEASE_URL=",
		"BACKSIGHT_RELEASE_DIR=", "BACKSIGHT_INSTALL_LIB=", "BACKSIGHT_HOME=")
	cmd.Env = append(cmd.Env, env...)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	return strings.ReplaceAll(string(out), "\r\n", "\n"), err
}
