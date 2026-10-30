package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// toolCall is one ToolRunner call as Fetch made it.
type toolCall struct {
	Env   []string
	Path  string
	Args  []string
	Stdin []byte
	// Req is the content of the -r file of a pip install, read during the
	// call (Fetch removes the file afterwards).
	Req []byte
}

// fakeTools records each call; Fail makes a call fail, Effect does what the
// real tool would to the disk, Out is what it prints.
type fakeTools struct {
	Calls  []toolCall
	Fail   func(c toolCall) error
	Effect func(c toolCall)
	Out    func(c toolCall) []byte
}

func (f *fakeTools) Run(_ context.Context, env []string, stdin io.Reader, path string, args ...string) ([]byte, error) {
	c := toolCall{Env: env, Path: path, Args: args}
	if stdin != nil {
		c.Stdin, _ = io.ReadAll(stdin)
	}
	if i := slices.Index(args, "-r"); i >= 0 && i+1 < len(args) {
		c.Req, _ = os.ReadFile(args[i+1])
	}
	f.Calls = append(f.Calls, c)
	if f.Fail != nil {
		if err := f.Fail(c); err != nil {
			return nil, err
		}
	}
	if f.Effect != nil {
		f.Effect(c)
	}
	if f.Out != nil {
		return f.Out(c), nil
	}
	return nil, nil
}

func newNative(t *testing.T, tools ToolRunner) Native {
	t.Helper()
	cfg := config.Default(paths.Dirs{Data: t.TempDir()}, "/home/u")
	return Native{Cfg: cfg, Tools: tools}
}

func touch(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

// realEffects does to the disk what uv and hf do: Python's binary, the
// venv's interpreter and hf, a model file.
func realEffects(t *testing.T, n Native) func(toolCall) {
	return func(c toolCall) {
		switch {
		case len(c.Args) > 1 && c.Args[0] == "python" && c.Args[1] == "install":
			touch(t, n.PythonBin(), 0o755)
		case len(c.Args) > 0 && c.Args[0] == "venv":
			v := c.Args[len(c.Args)-1]
			touch(t, filepath.Join(v, "bin", "python"), 0o755)
			touch(t, filepath.Join(v, "bin", "hf"), 0o755)
		case len(c.Args) > 1 && c.Args[0] == "pip":
			py := c.Args[slices.Index(c.Args, "--python")+1]
			touch(t, filepath.Join(filepath.Dir(filepath.Dir(py)), "lib", "python3.11", "site-packages", "x.py"), 0o644)
		case len(c.Args) > 1 && c.Args[0] == "download":
			touch(t, filepath.Join(n.HF(), "hub", "models--"+strings.ReplaceAll(c.Args[1], "/", "--"), "blobs", "x"), 0o644)
		}
	}
}

func installedUV(t *testing.T, n Native) { touch(t, n.UV(), 0o755) }

func under(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && filepath.IsAbs(p) && rel != ".." && !strings.HasPrefix(rel, "../")
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// The pinned lock is the image's set: the same image and Hindsight version
// as the Docker runtime (render.go's pinnedHindsight). In package server:
// nativelock never imports it (ruling R8).
func TestNativeLockMatchesThePin(t *testing.T) {
	if nativelock.Image() != pinnedHindsight {
		t.Errorf("the lock was made from %q, the pinned image is %q: run scripts/native-lock.sh", nativelock.Image(), pinnedHindsight)
	}
	if nativelock.Hindsight() != PinnedVersion() {
		t.Errorf("the lock installs hindsight-api-slim %q, the pin is %q", nativelock.Hindsight(), PinnedVersion())
	}
	if want := "https://github.com/astral-sh/uv/releases/download/0.13.1/uv-x86_64-unknown-linux-gnu.tar.gz"; uvURL() != want {
		t.Errorf("uv is fetched from %q, want %q", uvURL(), want)
	}
	if uvSHA256 != nativelock.UVSHA256 {
		t.Errorf("Fetch checks uv against %q, not the pin %q", uvSHA256, nativelock.UVSHA256)
	}
}

func TestNativeVersionAndOrder(t *testing.T) {
	n := newNative(t, nil)
	want := "1.1.0-" + nativelock.Hash12()
	if n.Version() != want {
		t.Errorf("Version() = %q, want %q", n.Version(), want)
	}
	if HindsightVersionOf(n.Version()) != "1.1.0" {
		t.Errorf("HindsightVersionOf(%q) = %q", n.Version(), HindsightVersionOf(n.Version()))
	}
	// Ruling R9: WithVersion is honoured as Docker's is.
	other := n.WithVersion("1.0.2-0123456789ab")
	if other.Version() != "1.0.2-0123456789ab" || n.Version() != want {
		t.Errorf("WithVersion: %q (the original now %q)", other.Version(), n.Version())
	}
	if n.WithVersion("").Version() != want {
		t.Errorf("WithVersion(\"\") = %q, want the pin", n.WithVersion("").Version())
	}
	if n.Kind() != "native" {
		t.Errorf("Kind() = %q", n.Kind())
	}
	for _, tc := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"1.1.0-aaaaaaaaaaaa", "1.2.0-bbbbbbbbbbbb", -1, true},
		{"1.2.0-bbbbbbbbbbbb", "1.1.0-aaaaaaaaaaaa", 1, true},
		{"1.10.0-aaaaaaaaaaaa", "1.9.3-aaaaaaaaaaaa", 1, true},
		{"1.1.0-aaaaaaaaaaaa", "1.1.0-bbbbbbbbbbbb", 0, false},
		{"1.1.0-aaaaaaaaaaaa", "1.1.0-aaaaaaaaaaaa", 0, true},
		{"", "1.1.0-aaaaaaaaaaaa", 0, false},
		{"garbage", "1.1.0-aaaaaaaaaaaa", 0, false},
	} {
		cmp, ok := n.ComparePins(tc.a, tc.b)
		if cmp != tc.cmp || ok != tc.ok {
			t.Errorf("ComparePins(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, cmp, ok, tc.cmp, tc.ok)
		}
	}
}

func TestNativePathsAreUnderComposeDir(t *testing.T) {
	n := newNative(t, nil)
	root := filepath.Join(n.Cfg.Server.ComposeDir, "native")
	for name, got := range map[string][2]string{
		"Root":         {n.Root(), root},
		"UV":           {n.UV(), filepath.Join(root, "uv", "0.13.1", "uv")},
		"Python":       {n.Python(), filepath.Join(root, "python")},
		"PythonBin":    {n.PythonBin(), filepath.Join(root, "python", "cpython-"+nativelock.Python()+"-linux-x86_64-gnu", "bin", "python3.11")},
		"VenvDir":      {n.VenvDir("1.1.0-0123456789ab"), filepath.Join(root, "venv", "1.1.0-0123456789ab")},
		"Home":         {n.Home(), filepath.Join(root, "home")},
		"HF":           {n.HF(), filepath.Join(root, "hf")},
		"EnvFile":      {n.EnvFile(), filepath.Join(root, "server.env")},
		"IdentityFile": {n.IdentityFile(), filepath.Join(root, "identity")},
	} {
		if got[0] != got[1] {
			t.Errorf("%s() = %q, want %q", name, got[0], got[1])
		}
	}
}

// uvTarball is a release archive as astral ships it: a directory holding
// uv and uvx.
func uvTarball(t *testing.T, uv []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"uv-x86_64-unknown-linux-gnu/", nil}, {"uv-x86_64-unknown-linux-gnu/uvx", []byte("uvx")}, {"uv-x86_64-unknown-linux-gnu/uv", uv}} {
		h := &tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(f.name, "/") {
			h.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.body)
	}
	_ = tw.Close()
	_ = gz.Close()
	return b.Bytes()
}

func serveUV(t *testing.T, body []byte) *[]string {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldSum := uvBaseURL, uvSHA256
	uvBaseURL = srv.URL + "/astral-sh/uv/releases/download"
	t.Cleanup(func() { uvBaseURL, uvSHA256 = oldBase, oldSum })
	return &got
}

func TestNativeFetchVerifiesUV(t *testing.T) {
	uvBin := []byte("#!/bin/sh\necho uv 0.13.1\n")
	tarball := uvTarball(t, uvBin)

	t.Run("a sha256 other than the pin installs nothing", func(t *testing.T) {
		serveUV(t, tarball) // uvSHA256 stays the real uv's
		tools := &fakeTools{}
		n := newNative(t, tools)
		err := n.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "the uv download does not match the sha256 this backsight carries: nothing was installed") {
			t.Fatalf("Fetch = %v, want the sha256 refusal", err)
		}
		if exists(filepath.Join(n.Root(), "uv")) {
			t.Error("a uv/ directory exists after a refused download")
		}
		if len(tools.Calls) != 0 {
			t.Errorf("tools ran after a refused uv: %+v", tools.Calls)
		}
		left, _ := os.ReadDir(n.Root())
		if len(left) != 0 {
			t.Errorf("the download left %v in %s", left, n.Root())
		}
	})

	t.Run("the pinned bytes give uv alone, 0755", func(t *testing.T) {
		got := serveUV(t, tarball)
		sum := sha256.Sum256(tarball)
		uvSHA256 = hex.EncodeToString(sum[:])
		tools := &fakeTools{}
		n := newNative(t, tools)
		tools.Effect = realEffects(t, n)
		if err := n.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if want := "/astral-sh/uv/releases/download/0.13.1/uv-x86_64-unknown-linux-gnu.tar.gz"; len(*got) != 1 || (*got)[0] != want {
			t.Errorf("requests %v, want one GET of %s", *got, want)
		}
		st, err := os.Stat(n.UV())
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Errorf("uv mode %v, want 0755", st.Mode().Perm())
		}
		if b, _ := os.ReadFile(n.UV()); !bytes.Equal(b, uvBin) {
			t.Errorf("uv holds %q, want the archive's uv", b)
		}
		if entries, _ := os.ReadDir(filepath.Dir(n.UV())); len(entries) != 1 {
			t.Errorf("uv/0.13.1 holds %v, want uv alone", entries)
		}
		if len(tools.Calls) == 0 || tools.Calls[0].Path != n.UV() {
			t.Errorf("the next steps did not run the fetched uv: %+v", tools.Calls)
		}
	})
}

func TestNativeFetchInstallsTheLockedVenv(t *testing.T) {
	tools := &fakeTools{}
	n := newNative(t, tools)
	tools.Effect = realEffects(t, n)
	installedUV(t, n)
	venv := n.VenvDir(n.Version())
	if err := n.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tools.Calls) != 5 {
		t.Fatalf("%d calls, want 5 (python, venv, pip, two models): %+v", len(tools.Calls), tools.Calls)
	}
	c := tools.Calls
	if c[0].Path != n.UV() || !slices.Equal(c[0].Args, []string{"python", "install", "--no-bin", nativelock.Python()}) {
		t.Errorf("call 1: %s %q, want uv python install --no-bin %s", c[0].Path, c[0].Args, nativelock.Python())
	}
	if !slices.Contains(c[0].Env, "UV_PYTHON_INSTALL_DIR="+n.Python()) {
		t.Errorf("python install env %q lacks UV_PYTHON_INSTALL_DIR=%s", c[0].Env, n.Python())
	}
	// Ruling R2: the venv is built where it runs from, never renamed.
	if c[1].Path != n.UV() || !slices.Equal(c[1].Args, []string{"venv", "--no-project", "--python", n.PythonBin(), venv}) {
		t.Errorf("call 2: %s %q, want uv venv --no-project --python %s %s", c[1].Path, c[1].Args, n.PythonBin(), venv)
	}
	pip := c[2].Args
	if c[2].Path != n.UV() || len(pip) != 8 || !slices.Equal(pip[:7], []string{"pip", "install", "--require-hashes", "--no-deps", "--python", filepath.Join(venv, "bin", "python"), "-r"}) {
		t.Errorf("call 3: %s %q, want uv pip install --require-hashes --no-deps --python <venv>/bin/python -r <file>", c[2].Path, pip)
	}
	if !bytes.Equal(c[2].Req, nativelock.Requirements) {
		t.Errorf("pip install read %d bytes, not the embedded lock (%d bytes)", len(c[2].Req), len(nativelock.Requirements))
	}
	for i, m := range [][2]string{
		{"cross-encoder/mmarco-mMiniLMv2-L12-H384-v1", "1427fd652930e4ba29e8149678df786c240d8825"},
		{"intfloat/multilingual-e5-small", "614241f622f53c4eeff9890bdc4f31cfecc418b3"},
	} {
		call := c[3+i]
		want := []string{"download", m[0], "--revision", m[1], "--include", "config.json", "--include", "onnx/model.onnx",
			"--include", "sentencepiece.bpe.model", "--include", "special_tokens_map.json", "--include", "tokenizer_config.json",
			"--include", "tokenizer.json"}
		if call.Path != filepath.Join(venv, "bin", "hf") || !slices.Equal(call.Args, want) {
			t.Errorf("model %s: %s %q, want <venv>/bin/hf %q", m[0], call.Path, call.Args, want)
		}
		if !slices.Contains(call.Env, "HF_HOME="+n.HF()) {
			t.Errorf("model %s env %q lacks HF_HOME", m[0], call.Env)
		}
		// Task 1 C5: offline, Hindsight resolves main through refs/main.
		ref := filepath.Join(n.HF(), "hub", "models--"+strings.ReplaceAll(m[0], "/", "--"), "refs", "main")
		if b, err := os.ReadFile(ref); err != nil || string(b) != m[1] {
			t.Errorf("%s = %q, %v; want %s", ref, b, err, m[1])
		}
	}
	for _, call := range c {
		if !filepath.IsAbs(call.Path) || !under(n.Root(), call.Path) {
			t.Errorf("%s is not under %s", call.Path, n.Root())
		}
		for _, a := range call.Args {
			if filepath.IsAbs(a) && !under(n.Root(), a) {
				t.Errorf("argument %s of %s is not under %s", a, call.Path, n.Root())
			}
		}
	}
	if !exists(filepath.Join(venv, ".backsight-complete")) {
		t.Error("no .backsight-complete in the venv")
	}
	if b, _ := os.ReadFile(filepath.Join(n.HF(), ".backsight-models")); strings.TrimSpace(string(b)) != nativelock.Hash12() {
		t.Errorf("hf/.backsight-models = %q, want the lock hash %s", b, nativelock.Hash12())
	}
	if exists(filepath.Join(n.Root(), "uv-cache")) {
		t.Error("uv-cache left after the install")
	}
	if left, _ := filepath.Glob(filepath.Join(n.Root(), "*requirements*")); len(left) != 0 {
		t.Errorf("the requirements file is left: %v", left)
	}

	t.Run("a second Fetch with everything present runs nothing", func(t *testing.T) {
		before := len(tools.Calls)
		if err := n.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(tools.Calls) != before {
			t.Errorf("ran %+v", tools.Calls[before:])
		}
	})

	t.Run("a failing pip install leaves no venv", func(t *testing.T) {
		tools := &fakeTools{Fail: func(c toolCall) error {
			if c.Args[0] == "pip" {
				return errors.New("exit status 1: hash mismatch")
			}
			return nil
		}}
		n := newNative(t, tools)
		tools.Effect = realEffects(t, n)
		installedUV(t, n)
		err := n.Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
			t.Fatalf("Fetch = %v, want pip's error", err)
		}
		if exists(n.VenvDir(n.Version())) {
			t.Error("the failed venv is left")
		}
		if exists(n.VenvDir(n.Version()) + ".partial") {
			t.Error("a .partial venv is left")
		}
		if exists(filepath.Join(n.Root(), "uv-cache")) {
			t.Error("uv-cache left after a failure")
		}
		if got := tools.Calls[len(tools.Calls)-1].Args[0]; got != "pip" {
			t.Errorf("ran %q after the failed pip install", got)
		}
	})

	t.Run("a venv without its marker is rebuilt", func(t *testing.T) {
		tools := &fakeTools{}
		n := newNative(t, tools)
		tools.Effect = realEffects(t, n)
		installedUV(t, n)
		touch(t, n.PythonBin(), 0o755)
		stale := filepath.Join(n.VenvDir(n.Version()), "lib", "stale.py")
		touch(t, stale, 0o644)
		if err := n.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if exists(stale) {
			t.Error("the incomplete venv was kept, not rebuilt")
		}
		if len(tools.Calls) < 2 || tools.Calls[0].Args[0] != "venv" || tools.Calls[1].Args[0] != "pip" {
			t.Errorf("calls %+v, want uv venv then pip install (Python present)", tools.Calls)
		}
	})

	t.Run("another version's venv cannot be built from this lock", func(t *testing.T) {
		tools := &fakeTools{}
		n := newNative(t, tools)
		installedUV(t, n)
		touch(t, n.PythonBin(), 0o755)
		err := n.WithVersion("1.0.2-0123456789ab").Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "1.0.2-0123456789ab") || !strings.Contains(err.Error(), n.Version()) {
			t.Fatalf("Fetch = %v, want a refusal naming both versions", err)
		}
		if len(tools.Calls) != 0 {
			t.Errorf("ran %+v", tools.Calls)
		}
		if err := n.WithVersion("../../etc").Fetch(context.Background()); err == nil {
			t.Error("a version that is a path was accepted")
		}
	})

	t.Run("a construction error is returned first", func(t *testing.T) {
		tools := &fakeTools{}
		n := newNative(t, tools)
		n.Err = errors.New("no data directory")
		if err := n.Fetch(context.Background()); err == nil || err.Error() != "no data directory" {
			t.Errorf("Fetch = %v", err)
		}
		if len(tools.Calls) != 0 || exists(n.Root()) {
			t.Error("Fetch acted despite Err")
		}
	})
}

// execTools runs each tool for real with exactly env, as the CLI's runner
// does, and records the call.
type execTools struct{ Calls []toolCall }

func (e *execTools) Run(ctx context.Context, env []string, stdin io.Reader, path string, args ...string) ([]byte, error) {
	e.Calls = append(e.Calls, toolCall{Env: env, Path: path, Args: args})
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out, nil
}

// RF 4: the user's Python and Hugging Face settings never reach the
// install; uv is the pinned one, by absolute path, never PATH's.
func TestNativeFetchIgnoresTheUsersPythonEnvironment(t *testing.T) {
	fakeDir := t.TempDir()
	ranMarker := filepath.Join(fakeDir, "ran")
	touch(t, filepath.Join(fakeDir, "uv"), 0o755)
	if err := os.WriteFile(filepath.Join(fakeDir, "uv"), []byte("#!/bin/sh\n: > "+ranMarker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	user := map[string]string{
		"UV_INDEX_URL":            "https://evil.invalid/simple",
		"PIP_INDEX_URL":           "https://evil.invalid/pip",
		"PIP_REQUIRE_VIRTUALENV":  "true",
		"PYTHONPATH":              "/evil/pythonpath",
		"VIRTUAL_ENV":             "/evil/venv",
		"HF_HUB_OFFLINE":          "1",
		"XDG_CONFIG_HOME":         "/evil/config",
		"UV_PYTHON_INSTALL_DIR":   "/evil/python",
		"UV_CACHE_DIR":            "/evil/cache",
		"HF_HOME":                 "/evil/hf",
		"HOME":                    "/evil/home",
		"UV_PYTHON_PREFERENCE":    "system",
		"PATH":                    fakeDir + ":" + os.Getenv("PATH"),
		"HTTPS_PROXY":             "http://proxy.example:3128",
		"https_proxy":             "http://lower.example:3128",
		"http_proxy":              "http://lower.example:3129",
		"no_proxy":                "localhost,.internal.example",
		"ALL_PROXY":               "socks5://all.example:1080",
		"all_proxy":               "socks5://lower-all.example:1080",
		"SSL_CERT_FILE":           "/etc/ssl/certs/ca.pem",
		"REQUESTS_CA_BUNDLE":      "/evil/ca.pem",
		"UV_EXTRA_INDEX_URL":      "https://evil.invalid/extra",
		"UV_NO_VERIFY_HASHES":     "1",
		"UV_INSECURE_HOST":        "evil.invalid",
		"PIP_NO_VERIFY_HASHES":    "1",
		"HF_ENDPOINT":             "https://evil.invalid",
		"PYTHONSTARTUP":           "/evil/startup.py",
		"UV_PYTHON_DOWNLOADS_URL": "https://evil.invalid/python",
	}
	for k, v := range user {
		t.Setenv(k, v)
	}

	tools := &execTools{}
	n := newNative(t, tools)
	// The pinned uv and the venv's hf: shell fakes doing what the real
	// ones do to the disk.
	uvFake := fmt.Sprintf(`#!/bin/sh
case "$1" in
python) mkdir -p %[1]q && : > %[1]q/python3.11 ;;
venv) for a; do v=$a; done; mkdir -p "$v/bin" && : > "$v/bin/python" && printf '#!/bin/sh\nexit 0\n' > "$v/bin/hf" && chmod 755 "$v/bin/hf" ;;
esac
`, filepath.Dir(n.PythonBin()))
	touch(t, n.UV(), 0o755)
	if err := os.WriteFile(n.UV(), []byte(uvFake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := n.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tools.Calls) != 5 {
		t.Fatalf("%d calls, want 5: %+v", len(tools.Calls), tools.Calls)
	}
	// Ruling R27: the lowercase and ALL_PROXY forms reach the tools too.
	proxies := []string{"HTTPS_PROXY", "https_proxy", "http_proxy", "no_proxy", "ALL_PROXY", "all_proxy"}
	allowed := map[string]bool{"PATH": true, "LANG": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true,
		"https_proxy": true, "http_proxy": true, "no_proxy": true, "ALL_PROXY": true, "all_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "UV_NO_CONFIG": true, "UV_PYTHON_PREFERENCE": true,
		"UV_PYTHON_INSTALL_DIR": true, "UV_CACHE_DIR": true, "HOME": true, "HF_HOME": true}
	for _, c := range tools.Calls {
		if !under(n.Root(), c.Path) {
			t.Errorf("ran %s, not under %s", c.Path, n.Root())
		}
		env := map[string]string{}
		for _, kv := range c.Env {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
			if !allowed[k] {
				t.Errorf("%s %s got %s from the user's environment", c.Path, c.Args[0], kv)
			}
			if strings.Contains(v, "evil") {
				t.Errorf("%s %s got the user's value %s", c.Path, c.Args[0], kv)
			}
		}
		if env["PATH"] != "/usr/bin:/bin" || env["LANG"] != "C.UTF-8" {
			t.Errorf("%s: PATH %q LANG %q", c.Path, env["PATH"], env["LANG"])
		}
		for _, k := range append(proxies, "SSL_CERT_FILE") {
			if env[k] != user[k] {
				t.Errorf("%s %s: %s = %q, want the user's %q", c.Path, c.Args[0], k, env[k], user[k])
			}
		}
		if env["HOME"] != n.Home() {
			t.Errorf("%s: HOME %q, want %s", c.Path, env["HOME"], n.Home())
		}
		if c.Path == n.UV() {
			if env["UV_NO_CONFIG"] != "1" || env["UV_PYTHON_PREFERENCE"] != "only-managed" ||
				env["UV_CACHE_DIR"] != filepath.Join(n.Root(), "uv-cache") || env["UV_PYTHON_INSTALL_DIR"] != n.Python() {
				t.Errorf("uv %s env %q", c.Args[0], c.Env)
			}
		} else if env["HF_HOME"] != n.HF() {
			t.Errorf("hf env %q", c.Env)
		}
	}
	if exists(ranMarker) {
		t.Error("the uv on PATH ran")
	}
	if st, err := os.Stat(n.Home()); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("home: %v, %v; want a 0700 directory", st, err)
	}
}

func TestNativeDownloadsListsWhatIsMissing(t *testing.T) {
	tools := &fakeTools{}
	n := newNative(t, tools)
	items, models, err := n.Downloads(context.Background())
	if err != nil || models {
		t.Fatalf("Downloads = %v, %v, %v", items, models, err)
	}
	want := []string{"uv 0.13.1", "Python " + nativelock.Python(),
		fmt.Sprintf("hindsight-api-slim %s (%d packages)", nativelock.Hindsight(), len(nativelock.Versions())),
		"embedding and reranker models"}
	if !slices.Equal(items, want) || !slices.Equal(want, []string{NativeUV, NativePython, NativeWheels, NativeModels}) {
		t.Errorf("Downloads = %q, want %q", items, want)
	}
	if mb := DownloadMB(items, models); mb != 20+100+1100+ModelsMB {
		t.Errorf("DownloadMB = %d, want %d", mb, 20+100+1100+ModelsMB)
	}
	if len(tools.Calls) != 0 {
		t.Errorf("Downloads ran %+v", tools.Calls)
	}

	installedUV(t, n)
	touch(t, n.PythonBin(), 0o755)
	if items, _, _ := n.Downloads(context.Background()); !slices.Equal(items, want[2:]) {
		t.Errorf("uv and Python present: %q, want %q", items, want[2:])
	}
	// A venv without its marker is still to download (ruling R2).
	touch(t, filepath.Join(n.VenvDir(n.Version()), "bin", "python"), 0o755)
	if items, _, _ := n.Downloads(context.Background()); !slices.Equal(items, want[2:]) {
		t.Errorf("incomplete venv: %q, want %q", items, want[2:])
	}

	tools.Effect = realEffects(t, n)
	var progress bytes.Buffer
	if err := n.Download(context.Background(), items, &progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress.String(), NativeWheels) || !strings.Contains(progress.String(), NativeModels) {
		t.Errorf("Download's progress %q names neither step", progress.String())
	}
	if items, models, err := n.Downloads(context.Background()); len(items) != 0 || models || err != nil {
		t.Errorf("all present: %q, %v, %v; want none", items, models, err)
	}
	// Models fetched for another lock are fetched again.
	if err := os.WriteFile(filepath.Join(n.HF(), ".backsight-models"), []byte("0123456789ab\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if items, _, _ := n.Downloads(context.Background()); !slices.Equal(items, []string{NativeModels}) {
		t.Errorf("models of another lock: %q", items)
	}
}

// Docker's sizes are unchanged by the native items.
func TestDownloadMBDocker(t *testing.T) {
	if mb := DownloadMB([]string{HindsightImage, PostgresImage}, true); mb != HindsightImageMB+PostgresImageMB+ModelsMB {
		t.Errorf("DownloadMB = %d", mb)
	}
}
