package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/server/nativelock"
)

// What a native install downloads, as Downloads lists it, and about how
// much each weighs (DownloadMB).
var (
	NativeUV     = "uv " + nativelock.UVVersion
	NativePython = "Python " + nativelock.Python()
	NativeWheels = fmt.Sprintf("hindsight-api-slim %s (%d packages)", nativelock.Hindsight(), len(nativelock.Versions()))
	// NativeUnpack is pg0's Postgres, unpacked from the venv's pg0: no
	// download of its own.
	NativeUnpack = "pg0's " + nativelock.Postgres + " (unpacked from the venv)"
)

const (
	NativeModels = "embedding and reranker models"

	NativeUVMB     = 20
	NativePythonMB = 100
	NativeWheelsMB = 1100
)

// uvBaseURL and uvSHA256 are where the pinned uv comes from and what an
// OS's archive must hash to; tests serve another.
var (
	uvBaseURL = "https://github.com/astral-sh/uv/releases/download"
	uvSHA256  = func(p nativelock.Platform) string { return p.UVSHA256 }
)

// uvClient fetches uv: a stalled download fails instead of hanging.
var uvClient = &http.Client{Timeout: 10 * time.Minute}

// uvURL is n's OS's uv archive: Linux's tarball, Windows' zip.
func (n Native) uvURL() string {
	return uvBaseURL + "/" + nativelock.UVVersion + "/" + n.pins().UVAsset
}

// hfModel is a model fetched by commit (Task 1 C5).
type hfModel struct{ Repo, Revision string }

// nativeModels are the reranker (decision 0078 requirement 8) and the
// embedding model, with the files Hindsight's onnx providers read; Task 1
// C5 measured the set (941 MB).
var (
	nativeModels = []hfModel{
		{"cross-encoder/mmarco-mMiniLMv2-L12-H384-v1", "1427fd652930e4ba29e8149678df786c240d8825"},
		{"intfloat/multilingual-e5-small", "614241f622f53c4eeff9890bdc4f31cfecc418b3"},
	}
	nativeModelFiles = []string{"config.json", "onnx/model.onnx", "sentencepiece.bpe.model",
		"special_tokens_map.json", "tokenizer_config.json", "tokenizer.json"}
)

const (
	// venvComplete is written in a venv once it is fully installed (ruling
	// R2): a venv without it is incomplete, deleted and rebuilt, never run.
	venvComplete = ".backsight-complete"
	// modelsDone in hf/ holds the lock hash the models were fetched for.
	modelsDone = ".backsight-models"
	// maxUV bounds the uv archive and binary read (about 20 MB each).
	maxUV = 256 << 20
)

// passedThrough are the only settings of the user's environment a tool
// gets: what it needs to reach the network through a proxy, in both the
// upper and the lower case Linux users set (ruling R27).
var passedThrough = []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "ALL_PROXY",
	"https_proxy", "http_proxy", "no_proxy", "all_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// toolEnv is a tool's whole environment: set, a fixed PATH and LANG, and
// the proxy settings when the user has them. Nothing else is inherited:
// not PYTHONPATH, VIRTUAL_ENV, an index URL or a Hugging Face setting.
func toolEnv(set ...string) []string {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	for _, k := range passedThrough {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return append(env, set...)
}

func (n Native) uvCache() string { return n.join(n.Root(), "uv-cache") }

// uvEnv is the pinned uv's environment: no configuration file, managed
// Pythons only, its Pythons and cache under Root() (Windows: toolEnvFor's,
// which carries uv's two settings).
func (n Native) uvEnv() []string {
	if n.goos() == "windows" {
		return append(n.toolEnvFor(filepath.Dir(n.UV())), "UV_PYTHON_INSTALL_DIR="+n.Python(), "UV_CACHE_DIR="+n.uvCache())
	}
	return toolEnv("HOME="+n.Home(), "UV_NO_CONFIG=1", "UV_PYTHON_PREFERENCE=only-managed",
		"UV_PYTHON_INSTALL_DIR="+n.Python(), "UV_CACHE_DIR="+n.uvCache())
}

// Fetch installs what Version runs without changing what runs: the pinned
// uv, Python, Version's venv, the Postgres pg0 unpacks from it and the
// models, each skipped when present and intact.
func (n Native) Fetch(ctx context.Context) error { return n.fetch(ctx, io.Discard) }

// FetchAll is Fetch: the database (pg0) comes with the venv, and Fetch
// unpacks its Postgres.
func (n Native) FetchAll(ctx context.Context) error { return n.Fetch(ctx) }

var _ Downloader = Native{}

// Downloads lists what Fetch still has to download; the models are one of
// the items (models is always false: Download fetches them, not the
// server's first start).
func (n Native) Downloads(context.Context) ([]string, bool, error) {
	if n.Err != nil {
		return nil, false, n.Err
	}
	var items []string
	if !n.uvInstalled() {
		items = append(items, NativeUV)
	}
	if !isRegular(n.PythonBin()) {
		items = append(items, NativePython)
	}
	if !n.venvInstalled(n.Version()) {
		items = append(items, NativeWheels)
	}
	if n.pgMissing() != "" {
		items = append(items, NativeUnpack)
	}
	if !n.modelsInstalled() {
		items = append(items, NativeModels)
	}
	return items, false, nil
}

// Download is Fetch with each step announced on w: it installs whatever
// is missing, items being what Downloads listed.
func (n Native) Download(ctx context.Context, _ []string, w io.Writer) error {
	return n.fetch(ctx, w)
}

func (n Native) fetch(ctx context.Context, w io.Writer) error {
	if n.Err != nil {
		return n.Err
	}
	v := n.Version()
	if !nativeTag.MatchString(v) {
		return fmt.Errorf("%q is not a native server version (<hindsight version>-<lock hash>)", v)
	}
	pinned := Native{}.Version()
	if v != pinned && !n.venvInstalled(v) {
		return fmt.Errorf("server version %s is not installed, and this backsight installs %s only", v, pinned)
	}
	if err := n.fetchUV(ctx, w); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(n.uvCache()) }()
	if err := n.fetchPython(ctx, w); err != nil {
		return err
	}
	if err := n.fetchVenv(ctx, w, v); err != nil {
		return err
	}
	if err := n.unpackPostgres(ctx, w, v); err != nil {
		return err
	}
	return n.fetchModels(ctx, w, v)
}

func isRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func (n Native) uvInstalled() bool {
	return fsutil.Executable(n.UV())
}

func (n Native) venvInstalled(v string) bool {
	return isRegular(filepath.Join(n.VenvDir(v), venvComplete))
}

func (n Native) modelsInstalled() bool {
	b, err := os.ReadFile(filepath.Join(n.HF(), modelsDone))
	return err == nil && strings.TrimSpace(string(b)) == nativelock.Hash12()
}

// fetchUV downloads the pinned uv's archive, checks its sha256 and only
// then extracts uv alone to UV().
func (n Native) fetchUV(ctx context.Context, w io.Writer) error {
	if n.uvInstalled() {
		return nil
	}
	fmt.Fprintf(w, "Downloading %s (about %d MB)\n", NativeUV, NativeUVMB)
	if err := n.ensureRoot(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(n.Root(), ".uv-download-*")
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	url, want := n.uvURL(), uvSHA256(n.pins())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := uvClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading uv %s: %w", nativelock.UVVersion, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading uv %s from %s: %s", nativelock.UVVersion, url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxUV)); err != nil {
		return fmt.Errorf("downloading uv %s: %w", nativelock.UVVersion, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the uv download does not match the sha256 this backsight carries: nothing was installed (%s has sha256 %s, want %s)", url, got, want)
	}
	if n.goos() == "windows" {
		return extractUVZip(tmp, n.UV())
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractUV(tmp, n.UV())
}

// extractUV writes the archive's uv (and nothing else) to dst, 0755.
func extractUV(archive io.Reader, dst string) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("the uv archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the uv archive holds no uv")
		}
		if err != nil {
			return fmt.Errorf("the uv archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "uv" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(dst), ".uv-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }() // gone once renamed
		if _, err := io.Copy(f, io.LimitReader(tr, maxUV)); err != nil {
			_ = f.Close()
			return fmt.Errorf("the uv archive: %w", err)
		}
		if err := f.Chmod(0o755); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(f.Name(), dst)
	}
}

// ensureHome creates the units' HOME, private: the tools run with it, and
// pg0 keeps its instance there.
// ensureRoot makes Root() this user's alone (0700), as home/ is: it holds
// server.env, the database and its identity.
func (n Native) ensureRoot() error {
	if err := fsutil.MkdirPrivate(n.Root()); err != nil {
		return err
	}
	return fsutil.MakePrivate(n.Root())
}

func (n Native) ensureHome() error {
	if err := fsutil.MkdirPrivate(n.Home()); err != nil {
		return err
	}
	return fsutil.MakePrivate(n.Home())
}

// fetchPython installs the pinned Python with the pinned uv into Python().
func (n Native) fetchPython(ctx context.Context, w io.Writer) error {
	if isRegular(n.PythonBin()) {
		return nil
	}
	fmt.Fprintf(w, "Downloading %s (about %d MB)\n", NativePython, NativePythonMB)
	if err := n.ensureHome(); err != nil {
		return err
	}
	// --no-bin: no python3.11 link in the user's ~/.local/bin; on Windows
	// --no-registry too: no PEP 514 entry under HKCU (E34 ruling 6).
	if n.goos() == "windows" {
		if err := n.uvPythonInstallWindows(ctx); err != nil {
			return err
		}
	} else if _, err := n.Tools.Run(ctx, n.uvEnv(), nil, n.UV(), "python", "install", "--no-bin", nativelock.Python()); err != nil {
		return fmt.Errorf("uv python install %s: %w", nativelock.Python(), err)
	}
	if !isRegular(n.PythonBin()) {
		return fmt.Errorf("uv installed Python %s, but %s is missing", nativelock.Python(), n.PythonBin())
	}
	return nil
}

// fetchVenv builds version v's venv where it will run from (a venv is not
// relocatable) and marks it complete last (ruling R2). A failed build
// leaves no venv.
func (n Native) fetchVenv(ctx context.Context, w io.Writer, v string) (err error) {
	if n.venvInstalled(v) {
		return nil
	}
	fmt.Fprintf(w, "Downloading %s (about %d MB)\n", NativeWheels, NativeWheelsMB)
	if err := n.ensureHome(); err != nil {
		return err
	}
	venv := n.VenvDir(v)
	if err := os.RemoveAll(venv); err != nil { // an incomplete one
		return err
	}
	if err := os.MkdirAll(filepath.Dir(venv), 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(venv)
		}
	}()
	req, err := os.CreateTemp(n.Root(), ".requirements-*.txt")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(req.Name()) }()
	if _, err := req.Write(n.pins().Requirements); err != nil {
		_ = req.Close()
		return err
	}
	if err := req.Close(); err != nil {
		return err
	}
	if _, err := n.Tools.Run(ctx, n.uvEnv(), nil, n.UV(), "venv", "--no-project", "--python", n.venvPython(), venv); err != nil {
		return fmt.Errorf("uv venv %s: %w", venv, err)
	}
	if _, err := n.Tools.Run(ctx, n.uvEnv(), nil, n.UV(), "pip", "install", "--require-hashes", "--no-deps",
		"--python", n.venvBin(v, "python"), "-r", req.Name()); err != nil {
		return fmt.Errorf("uv pip install (the locked %s): %w", NativeWheels, err)
	}
	if n.goos() == "windows" {
		// Before pg0's first run, which needs them, and onnxruntime's
		// import (E34 W5).
		if err := n.copyRuntimeDLLs(v, n.pg0BinDir(v), n.onnxCapi(v)); err != nil {
			return err
		}
	}
	return fsutil.WriteAtomic(filepath.Join(venv, venvComplete), []byte(nativelock.Hash12()+"\n"), 0o644)
}

// fetchModels downloads the models by commit with v's hf into HF(), then
// writes each refs/main: Hindsight asks for main, which offline resolves
// only through it (Task 1 C5).
func (n Native) fetchModels(ctx context.Context, w io.Writer, v string) error {
	if n.modelsInstalled() {
		return nil
	}
	fmt.Fprintf(w, "Downloading the %s (about %d MB)\n", NativeModels, ModelsMB)
	if err := os.MkdirAll(n.HF(), 0o755); err != nil {
		return err
	}
	hf := n.venvBin(v, "hf")
	env := toolEnv("HOME="+n.Home(), "HF_HOME="+n.HF())
	if n.goos() == "windows" {
		env = append(n.toolEnvFor(filepath.Dir(hf)), "HF_HOME="+n.HF())
	}
	for _, m := range nativeModels {
		args := []string{"download", m.Repo, "--revision", m.Revision}
		for _, f := range nativeModelFiles {
			args = append(args, "--include", f)
		}
		if _, err := n.Tools.Run(ctx, env, nil, hf, args...); err != nil {
			return fmt.Errorf("hf download %s: %w", m.Repo, err)
		}
		refs := filepath.Join(n.HF(), "hub", "models--"+strings.ReplaceAll(m.Repo, "/", "--"), "refs")
		if err := os.MkdirAll(refs, 0o755); err != nil {
			return err
		}
		if err := fsutil.WriteAtomic(filepath.Join(refs, "main"), []byte(m.Revision), 0o644); err != nil {
			return err
		}
	}
	return fsutil.WriteAtomic(filepath.Join(n.HF(), modelsDone), []byte(nativelock.Hash12()+"\n"), 0o644)
}

// pgUnpackInstance is the throwaway pg0 instance unpackPostgres starts and
// drops: never pgInstance, the server's database.
const pgUnpackInstance = "backsight-unpack"

// pgUnpacked, in the installation, says unpackPostgres found it complete
// (or completed it); without every pgRequired file it counts for nothing.
const pgUnpacked = ".backsight-unpacked"

// pgRequired are the installation's files the native runtime runs: the db
// unit (postgres, pgvector), the identity check (pg_controldata),
// VerifyDump (initdb, postgres), the database tools (psql, pg_dump,
// pg_restore) and pg0's own pg_ctl.
var pgRequired = []string{"bin/initdb", "bin/postgres", "bin/pg_ctl", "bin/pg_controldata", "bin/psql", "bin/pg_dump",
	"bin/pg_restore", "share/extension/vector.control"}

// pgMissing is the first of pgRequired missing from the installation, ""
// when all are there.
func (n Native) pgMissing() string { return n.pgMissingIn(n.installation()) }

// pgMissingIn is pgMissing for the installation at dir: on Windows the
// programs are .exe.
func (n Native) pgMissingIn(dir string) string {
	for _, f := range pgRequired {
		if strings.HasPrefix(f, "bin/") {
			f = n.exe(f)
		}
		if p := n.join(dir, filepath.FromSlash(f)); !isRegular(p) {
			return p
		}
	}
	return ""
}

func (n Native) unpackUnit() string { return n.project() + "-pg-unpack.service" }

// stopUnpack stops the unpack unit and clears its failed state, on a
// context of its own (a cancelled one would run nothing). A unit that is
// not there is no error worth reporting.
func (n Native) stopUnpack(ctx context.Context) {
	n.stopTransient(ctx, n.unpackUnit())
}

// unpackPostgres has pg0 extract its Postgres into installation(), which
// pg0 does at its first start only, while a switch to native runs those
// binaries (VerifyDump) before the server's database ever starts. pg0 has
// no install-only command: it starts a throwaway instance of its own on a
// free loopback port and drops it, in a transient unit (as createInstance
// runs pg0: HOME=home/, a private TMPDIR under it removed after, ruling
// R21), so that no instance, no postgres and no temporary file is left.
// The server's instance is never named.
//
// An installation with every pgRequired file is complete: marked if it
// is not (createInstance's first start extracted it), and nothing runs.
// A partial one (a cut extraction) is removed first: pg0 extracts only
// where there is none. The unit is stopped before its files are removed,
// whatever happens: a cancel kills systemd-run's client, not the unit.
func (n Native) unpackPostgres(ctx context.Context, w io.Writer, v string) (err error) {
	if n.goos() == "windows" {
		return n.unpackPostgresWindows(ctx, w, v)
	}
	marker := filepath.Join(n.installation(), pgUnpacked)
	if n.pgMissing() == "" {
		if isRegular(marker) {
			return nil
		}
		return fsutil.WriteAtomic(marker, []byte(nativelock.PG0+"\n"), 0o644)
	}
	fmt.Fprintf(w, "Unpacking %s\n", NativeUnpack)
	if err := n.ensureHome(); err != nil {
		return err
	}
	port, err := n.verifyPort()
	if err != nil {
		return err
	}
	n.stopUnpack(ctx) // a cancelled run's, still running
	tmp := filepath.Join(n.Home(), "tmp-unpack")
	throwaway := filepath.Join(n.Home(), ".pg0", "instances", pgUnpackInstance)
	for _, d := range []string{tmp, throwaway, n.installation()} { // a killed run's, a partial extraction
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	defer func() {
		n.stopUnpack(ctx)
		for _, d := range []string{tmp, throwaway} {
			if rmErr := os.RemoveAll(d); rmErr != nil {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	pg0 := filepath.Join(n.sitePackages(v), "pg0", "bin", "pg0")
	steps := [][]string{
		{pg0, "start", "--name", pgUnpackInstance, "--port", strconv.Itoa(port), "-c", "listen_addresses=127.0.0.1", stepNoStdin, stepNoStdout},
		{pg0, "drop", "--force", "--name", pgUnpackInstance, stepNoStdin, stepNoStdout},
	}
	env := []string{"HOME=" + n.Home(), "TMPDIR=" + tmp, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	if err := n.host().Transient(ctx, n.unpackUnit(), env, nil, steps); err != nil {
		return fmt.Errorf("unpacking %s with pg0 into %s: %w", NativePostgres, n.installation(), err)
	}
	if m := n.pgMissing(); m != "" {
		return fmt.Errorf("pg0 ran, but the %s it unpacks is incomplete: %s is missing", NativePostgres, m)
	}
	return fsutil.WriteAtomic(marker, []byte(nativelock.PG0+"\n"), 0o644)
}
