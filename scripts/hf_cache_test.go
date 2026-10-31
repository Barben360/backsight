package scripts

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

// fakeDocker is a docker on PATH that logs each call and answers from
// files in its directory: inspect-ok (volume inspect succeeds), run-fails
// (docker run exits 1), out.tar (docker run's stdout); docker run's stdin
// lands in stdin.tar.
const fakeDocker = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
case "$1 $2" in
  "volume inspect") [ -e "$d/inspect-ok" ] ;;
  "volume "*) exit 0 ;;
  "run "*)
    [ ! -e "$d/run-fails" ] || exit 1
    case " $* " in
      *" -xpf - "*) cat > "$d/stdin.tar" ;;
      *) cat "$d/out.tar" ;;
    esac ;;
  *) exit 3 ;;
esac
`

// hfCache runs one hf-cache.sh function with the fake docker, as the CI
// scripts do (set -euo pipefail); it returns the docker calls, stderr and
// the shell's error.
func hfCache(t *testing.T, fake, tarPath, call string, env ...string) (calls []string, stderr string, err error) {
	t.Helper()
	root, _ := filepath.Abs("..")
	cmd := exec.Command("bash", "-c", `set -euo pipefail; root=$1; . "$root/scripts/hf-cache.sh"; `+call, "hf", root)
	cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"))
	if tarPath != "" {
		cmd.Env = append(cmd.Env, "BACKSIGHT_HF_CACHE="+tarPath)
	} else {
		cmd.Env = append(cmd.Env, "BACKSIGHT_HF_CACHE=")
	}
	cmd.Env = append(cmd.Env, env...)
	var e bytes.Buffer
	cmd.Stderr = &e
	err = cmd.Run()
	if b, rerr := os.ReadFile(filepath.Join(fake, "calls")); rerr == nil {
		calls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return calls, e.String(), err
}

func newFakeDocker(t *testing.T, flags ...string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		_ = os.WriteFile(filepath.Join(d, f), nil, 0o644)
	}
	return d
}

// tarOf is a tar holding the given file names.
func tarOf(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, n := range names {
		_ = w.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
		_, _ = w.Write([]byte("x\n"))
	}
	_ = w.Close()
	return b.Bytes()
}

func pinnedImage(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../internal/server/render.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(src), "\n") {
		if v, ok := strings.CutPrefix(l, `const pinnedHindsight = "`); ok {
			return strings.TrimSuffix(v, `"`)
		}
	}
	t.Fatal("no pinnedHindsight")
	return ""
}

func TestHFCacheDoesNothingWithoutItsVariable(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	fake := newFakeDocker(t)
	calls, _, err := hfCache(t, fake, "", "hfc_seed bs-x; hfc_save bs-x")
	if err != nil || calls != nil {
		t.Errorf("unset BACKSIGHT_HF_CACHE: err %v, docker calls %q, want none", err, calls)
	}
}

func TestHFCacheSeedsANewVolumeFromTheTar(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	fake := newFakeDocker(t)
	tarPath := filepath.Join(t.TempDir(), "hf_cache.tar")
	want := tarOf(t, "./huggingface/hub/models--a/blob")
	_ = os.WriteFile(tarPath, want, 0o644)
	calls, stderr, err := hfCache(t, fake, tarPath, "hfc_seed bs-x")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if len(calls) != 3 ||
		calls[1] != "volume create --label com.docker.compose.project=bs-x --label com.docker.compose.volume=hf_cache bs-x_hf_cache" ||
		!strings.Contains(calls[2], "-v bs-x_hf_cache:/cache "+pinnedImage(t)+" -xpf - -C /cache") ||
		!strings.Contains(calls[2], "--network none --user 0") {
		t.Errorf("docker calls:\n%s", strings.Join(calls, "\n"))
	}
	if got, _ := os.ReadFile(filepath.Join(fake, "stdin.tar")); !bytes.Equal(got, want) {
		t.Errorf("the volume was not fed the tar (%d bytes, want %d)", len(got), len(want))
	}
}

func TestHFCacheSeedLeavesAnExistingVolumeAndAMissingTarAlone(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	fake := newFakeDocker(t, "inspect-ok")
	tarPath := filepath.Join(t.TempDir(), "hf_cache.tar")
	_ = os.WriteFile(tarPath, tarOf(t, "./huggingface/hub/models--a/blob"), 0o644)
	calls, _, err := hfCache(t, fake, tarPath, "hfc_seed bs-x")
	if err != nil || len(calls) != 1 || !strings.HasPrefix(calls[0], "volume inspect") {
		t.Errorf("existing volume: err %v, docker calls %q, want only the inspect", err, calls)
	}
	fake = newFakeDocker(t)
	calls, stderr, err := hfCache(t, fake, filepath.Join(t.TempDir(), "none.tar"), "hfc_seed bs-x")
	if err != nil || calls != nil || !strings.Contains(stderr, "downloads its models") {
		t.Errorf("no tar (a cache miss): err %v, docker calls %q, stderr %q", err, calls, stderr)
	}
}

// A broken cache is a miss: the half-filled volume goes, the run goes on.
func TestHFCacheSeedFailureRemovesTheVolume(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	fake := newFakeDocker(t, "run-fails")
	tarPath := filepath.Join(t.TempDir(), "hf_cache.tar")
	_ = os.WriteFile(tarPath, []byte("not a tar"), 0o644)
	calls, stderr, err := hfCache(t, fake, tarPath, "hfc_seed bs-x; echo after")
	if err != nil {
		t.Fatalf("a failed seed failed the run: %v\n%s", err, stderr)
	}
	if calls[len(calls)-1] != "volume rm -f bs-x_hf_cache" || !strings.Contains(stderr, "::warning::") {
		t.Errorf("docker calls %q, stderr %q: want the volume removed and a warning", calls, stderr)
	}
}

func TestHFCacheSavesTheVolumeOnce(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	fake := newFakeDocker(t, "inspect-ok")
	out := tarOf(t, "./", "./huggingface/hub/models--a/blob")
	_ = os.WriteFile(filepath.Join(fake, "out.tar"), out, 0o644)
	tarPath := filepath.Join(t.TempDir(), "sub", "hf_cache.tar")
	calls, stderr, err := hfCache(t, fake, tarPath, "hfc_save bs-x")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got, _ := os.ReadFile(tarPath); !bytes.Equal(got, out) {
		t.Errorf("saved %d bytes, want the volume's %d", len(got), len(out))
	}
	if len(calls) != 2 || !strings.Contains(calls[1], "-v bs-x_hf_cache:/cache:ro "+pinnedImage(t)+" -cf - -C /cache .") {
		t.Errorf("docker calls:\n%s", strings.Join(calls, "\n"))
	}
	// Restored by CI: the tar is not written again.
	_ = os.Remove(filepath.Join(fake, "calls"))
	calls, _, err = hfCache(t, fake, tarPath, "hfc_save bs-x")
	if err != nil || calls != nil {
		t.Errorf("tar already there: err %v, docker calls %q, want none", err, calls)
	}
}

func TestHFCacheDoesNotSaveAVolumeWithoutModels(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	for name, flags := range map[string][]string{"no models": {"inspect-ok"}, "no volume": nil, "docker fails": {"inspect-ok", "run-fails"}} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker(t, flags...)
			_ = os.WriteFile(filepath.Join(fake, "out.tar"), tarOf(t, "./", "./huggingface/xet/log"), 0o644)
			dir := t.TempDir()
			tarPath := filepath.Join(dir, "hf_cache.tar")
			_, stderr, err := hfCache(t, fake, tarPath, "hfc_save bs-x; echo after")
			if err != nil {
				t.Fatalf("a failed save failed the run: %v\n%s", err, stderr)
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 || !strings.Contains(stderr, "::warning::") {
				t.Errorf("left %v, stderr %q: want no file and a warning", left, stderr)
			}
		})
	}
}

// CI reads a restored tar once (BACKSIGHT_HF_CACHE_DROP=1): it goes once
// the volume is seeded, frees the runner's disk, and the run does not
// write it again. Without the variable a seeded tar stays.
func TestHFCacheDropsASeededTarOnRequest(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: POSIX script")
	for _, drop := range []bool{true, false} {
		fake := newFakeDocker(t)
		tarPath := filepath.Join(t.TempDir(), "hf_cache.tar")
		_ = os.WriteFile(tarPath, tarOf(t, "./huggingface/hub/models--a/blob"), 0o644)
		_ = os.WriteFile(filepath.Join(fake, "out.tar"), tarOf(t, "./", "./huggingface/hub/models--b/blob"), 0o644)
		var env []string
		if drop {
			env = []string{"BACKSIGHT_HF_CACHE_DROP=1"}
		}
		// The volume exists once seeded, as it would for the save.
		calls, stderr, err := hfCache(t, fake, tarPath, `hfc_seed bs-x; touch "$(dirname "$(command -v docker)")/inspect-ok"; hfc_save bs-x`, env...)
		if err != nil {
			t.Fatalf("drop=%v: %v\n%s", drop, err, stderr)
		}
		_, statErr := os.Stat(tarPath)
		if drop && !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("drop=1: the seeded tar is still there (%v)", statErr)
		}
		if !drop && statErr != nil {
			t.Errorf("no drop: the seeded tar went (%v)", statErr)
		}
		for _, c := range calls {
			if strings.Contains(c, "-cf -") {
				t.Errorf("drop=%v: a seeded run saved the volume again: %s", drop, c)
			}
		}
	}
}
