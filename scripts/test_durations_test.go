package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGH is a gh that answers test-durations.sh's two calls and logs
// them: run list prints run 777, run download writes the times of
// $FAKE_SLICES slices (each one test of 2 s, slice 1's failed when
// $FAKE_FAIL is set) into the --dir given.
const fakeGH = `#!/usr/bin/env bash
echo "$*" >> "$GH_LOG"
case "$1 $2" in
"run list") echo 777 ;;
"run download")
  while [ $# -gt 0 ]; do [ "$1" = --dir ] && dir=$2; shift; done
  for s in $(seq 0 $((FAKE_SLICES - 1))); do
    mkdir -p "$dir/test-race-times-$s"
    action=pass
    if [ -n "${FAKE_FAIL:-}" ] && [ "$s" = 1 ]; then action=fail; fi
    printf '{"Action":"%s","Package":"m/p","Test":"TestSlice%s","Elapsed":2}\n' "$action" "$s" > "$dir/test-race-times-$s/test-race-$s.json"
  done ;;
*) exit 2 ;;
esac
`

// testDurations runs test-durations.sh with the fake gh, its file and
// downloads in dir: its output, the gh calls logged since the last run,
// and its error.
func testDurations(t *testing.T, dir string, env []string, args ...string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "gh.log")
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"test-durations.sh"}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_LOG=" + log,
		"OUT=" + filepath.Join(dir, "durations.txt"),
		"CACHE=" + filepath.Join(dir, "cache"),
	}, env...)...)
	out, err := cmd.CombinedOutput()
	calls, rerr := os.ReadFile(log)
	if rerr != nil {
		t.Fatal(rerr)
	}
	return string(out), string(calls), err
}

// The durations come from every test-race slice of a CI run, main's latest
// by default, read again from the downloads for the same run; a missing
// slice, a failed test or a run id that is not a number leaves the file
// as it was.
func TestTestDurationsRecordsEverySliceOfACIRun(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "durations.txt")
	out, calls, err := testDurations(t, dir, []string{"FAKE_SLICES=4"})
	if err != nil {
		t.Fatalf("test-durations.sh: %v\n%s", err, out)
	}
	if !strings.Contains(calls, "run list --workflow ci --branch main --event push --status success") || !strings.Contains(calls, "run download 777 --pattern test-race-times-*") {
		t.Errorf("gh calls:\n%s\nwant main's latest successful ci run, then its test-race times", calls)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for s := range 4 {
		if !strings.Contains(string(b), fmt.Sprintf("2.00 m/p TestSlice%d\n", s)) {
			t.Errorf("durations.txt lacks slice %d's test:\n%s", s, b)
		}
	}

	// the same run again: no download
	if out, calls, err := testDurations(t, dir, []string{"FAKE_SLICES=4"}, "777"); err != nil || strings.Contains(calls, "download") {
		t.Errorf("run 777 again: error %v, gh calls %q, want the downloads read again\n%s", err, calls, out)
	}

	for _, c := range []struct {
		name string
		env  []string
		args []string
	}{
		{"a slice missing", []string{"FAKE_SLICES=3"}, []string{"801"}},
		{"a failed test", []string{"FAKE_SLICES=4", "FAKE_FAIL=1"}, []string{"802"}},
		{"a run id that is not a number", []string{"FAKE_SLICES=4"}, []string{"main"}},
	} {
		if err := os.WriteFile(file, []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, _, err := testDurations(t, dir, c.env, c.args...)
		if err == nil {
			t.Errorf("%s: test-durations.sh passed, want a failure\n%s", c.name, out)
		}
		if b, _ := os.ReadFile(file); string(b) != "kept\n" {
			t.Errorf("%s: durations.txt became %q, want it left as it was", c.name, b)
		}
	}
}

// The durations come from the slices test-race runs and uploads: the
// script reads as many as the job's matrix holds, under the names the
// job uploads them, and each slice keeps its run's go test -json.
func TestCIRaceSlicesUploadTheTimesTestDurationsReads(t *testing.T) {
	j, ok := ciJobs(t)["test-race"]
	if !ok {
		t.Fatal("ci.yml has no test-race job")
	}
	for _, want := range []string{
		`make test-shard SHARD="$SHARD" SHARDS=4 TEST_JSON="$RUNNER_TEMP/test-race-$SHARD.json"`,
		"uses: actions/upload-artifact@",
		"name: test-race-times-${{ matrix.shard }}",
		"path: ${{ runner.temp }}/test-race-${{ matrix.shard }}.json",
		"if-no-files-found: error",
	} {
		if !strings.Contains(j.raw, want) {
			t.Errorf("ci.yml job test-race lacks %q", want)
		}
	}
	sh, err := os.ReadFile("test-durations.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"slices=4 ", `f="$dir/test-race-times-$s/test-race-$s.json"`, "--pattern 'test-race-times-*'"} {
		if !strings.Contains(string(sh), want) {
			t.Errorf("test-durations.sh lacks %q: it does not read what test-race uploads", want)
		}
	}
	mk, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mk), `bash -o pipefail -c`) || !strings.Contains(string(mk), `| go run ./scripts/testshard -report "$$1"' "$$pattern" "$(TEST_JSON)"`) {
		t.Error("make test-shard does not keep go test's status and its JSON in TEST_JSON (bash -o pipefail, testshard -report)")
	}
}
