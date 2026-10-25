package scripts

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// slotPort runs scripts/ports.sh's slot_port with BACKSIGHT_CI_SLOT=slot
// ("" leaves it unset).
func slotPort(t *testing.T, slot string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", `. ./ports.sh && slot_port "$@"`, "slot_port"}, args...)...)
	cmd.Env = os.Environ()
	for i, kv := range cmd.Env {
		if strings.HasPrefix(kv, "BACKSIGHT_CI_SLOT=") {
			cmd.Env = append(cmd.Env[:i:i], cmd.Env[i+1:]...)
			break
		}
	}
	if slot != "" {
		cmd.Env = append(cmd.Env, "BACKSIGHT_CI_SLOT="+slot)
	}
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

// Issue #214: every scratch stack's API and UI ports, for every script that
// starts one and every slot, sit below Linux's ephemeral range, away from
// the live installs, and are never shared.
func TestScratchPortsAreBelowTheEphemeralRangeAndApart(t *testing.T) {
	call := regexp.MustCompile(`slot_port (\d+) "\$\{1:-\}"`)
	owner := map[int]string{}
	for _, script := range []string{"sandbox.sh", "fresh-install.sh", "restore-proof.sh", "agent-install.sh"} {
		src, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		m := call.FindAllSubmatch(src, -1)
		if len(m) != 1 {
			t.Fatalf("%s: want one slot_port call for its default port, found %d", script, len(m))
		}
		for _, slot := range []string{"", "1", "2", "3"} {
			out, err := slotPort(t, slot, string(m[0][1]))
			if err != nil {
				t.Fatalf("%s slot %q: %v", script, slot, err)
			}
			api, err := strconv.Atoi(out)
			if err != nil {
				t.Fatalf("%s slot %q: port %q", script, slot, out)
			}
			for _, p := range []int{api, api + 1111} {
				where := script + " slot " + slot
				if p < 1024 || p >= 32768 {
					t.Errorf("%s: port %d is outside 1024-32767", where, p)
				}
				if other, ok := owner[p]; ok {
					t.Errorf("%s: port %d already taken by %s", where, p, other)
				}
				owner[p] = where
			}
		}
	}
}

func TestSlotPortRefusesLivePortsAndUnknownSlots(t *testing.T) {
	for _, c := range []struct{ slot, port string }{
		{"", "8888"}, {"", "18888"}, {"", "7777"}, {"", "17777"}, // API, or UI = API + 1111, of a live install
		{"4", ""}, {"x", ""}, {"", "abc"},
	} {
		args := []string{"28300"}
		if c.port != "" {
			args = append(args, c.port)
		}
		if out, err := slotPort(t, c.slot, args...); err == nil {
			t.Errorf("slot %q port %q: accepted as %q", c.slot, c.port, out)
		}
	}
	if out, err := slotPort(t, "2", "28300", "31000"); err != nil || out != "31000" {
		t.Errorf("an explicit port: got %q, %v", out, err)
	}
}
