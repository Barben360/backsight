package scripts

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// make's targets run go with JOBS CPUs (default 4): go test -race ./...
// otherwise runs a package per core, each on every core, and a 24-core
// workstation's load passed 100. The probe is a rule added on the command
// line: its recipe prints the GOMAXPROCS every recipe of the Makefile sees.
func makeOut(t *testing.T, env []string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make missing")
	}
	cmd := exec.Command("make", append([]string{"--no-print-directory", "-C", ".."}, args...)...)
	for _, kv := range os.Environ() { // the caller's own JOBS or GOMAXPROCS would skew the cases
		if !strings.HasPrefix(kv, "JOBS=") && !strings.HasPrefix(kv, "GOMAXPROCS=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const probe = `--eval=probe: ; @echo "$$GOMAXPROCS"`

func TestMakeRunsGoOnFourCPUsByDefault(t *testing.T) {
	if got := makeOut(t, nil, probe, "probe"); got != "4" {
		t.Errorf("recipes see GOMAXPROCS=%q, want 4", got)
	}
}

func TestMakeJobsSetsTheCPUs(t *testing.T) {
	if got := makeOut(t, nil, probe, "probe", "JOBS=2"); got != "2" {
		t.Errorf("make JOBS=2: recipes see GOMAXPROCS=%q, want 2", got)
	}
	if got := makeOut(t, []string{"JOBS=3"}, probe, "probe"); got != "3" {
		t.Errorf("JOBS=3 in the environment: recipes see GOMAXPROCS=%q, want 3", got)
	}
}

// A GOMAXPROCS of the caller's (a shell profile's) does not lift the cap:
// JOBS is the one knob.
func TestMakeCapOverridesTheCallersGOMAXPROCS(t *testing.T) {
	if got := makeOut(t, []string{"GOMAXPROCS=24"}, probe, "probe"); got != "4" {
		t.Errorf("GOMAXPROCS=24 in the environment: recipes see %q, want 4", got)
	}
}

// golangci-lint sizes itself by the machine's cores, not by GOMAXPROCS.
func TestMakeLintRunsOnJobsCPUs(t *testing.T) {
	if got := makeOut(t, nil, "-n", "lint"); !strings.Contains(got, "golangci-lint run --concurrency 4 ") {
		t.Errorf("make -n lint: no --concurrency 4:\n%s", got)
	}
	if got := makeOut(t, nil, "-n", "lint", "JOBS=2"); !strings.Contains(got, "--concurrency 2 ") {
		t.Errorf("make -n lint JOBS=2: no --concurrency 2:\n%s", got)
	}
}
