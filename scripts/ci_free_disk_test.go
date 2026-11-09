package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// freeDisk runs ci-free-disk.sh with a PATH holding only fakes: a df
// whose free space on / is avail (as df --output=avail -k prints it), a
// sudo and an rm that only log their arguments, and the real tail and tr.
// Any other command the script ran would not be found, so nothing it does
// reaches the machine running the test. It returns the script's output,
// what it ran through sudo ("" when nothing), and its error.
func freeDisk(t *testing.T, avail string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	logs := filepath.Join(dir, "sudo.log")
	fakes := map[string]string{
		"df":   "#!" + bash + "\nif [ \"$1\" = --output=avail ]; then echo '    Avail'; echo '" + avail + "'; else echo 'Filesystem Size Used Avail Use% Mounted on'; fi\n",
		"sudo": "#!" + bash + "\necho \"$*\" >> '" + logs + "'\n",
		"rm":   "#!" + bash + "\necho \"unsudoed rm $*\" >> '" + logs + "'\n",
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"tail", "tr"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("no %s", name)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, "ci-free-disk.sh")
	cmd.Env = []string{"PATH=" + dir, "HOME=" + dir}
	out, runErr := cmd.CombinedOutput()
	ran, err := os.ReadFile(logs)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(out), strings.TrimSpace(string(ran)), runErr
}

// The runner's preinstalled toolchains go only when less than 20 GiB is
// free; with more the script removes nothing, and a df giving no number
// fails the step rather than pass as enough room.
func TestFreeDiskRemovesOnlyWhenShort(t *testing.T) {
	for _, c := range []struct {
		name, avail string
		removes     bool
	}{
		{"86 GiB free, as on a runner of 2026-11", "90177536", false},
		{"20 GiB free exactly", "20971520", false},
		{"just under 20 GiB free", "20971519", true},
		{"5 GiB free", "5242880", true},
	} {
		out, ran, err := freeDisk(t, c.avail)
		if err != nil {
			t.Errorf("%s: the script failed: %v\n%s", c.name, err, out)
			continue
		}
		if !c.removes {
			if ran != "" {
				t.Errorf("%s: it ran %q, want nothing removed", c.name, ran)
			}
			continue
		}
		for _, s := range []string{"/usr/share/dotnet", "/usr/local/lib/android", "/opt/ghc", "/opt/hostedtoolcache/CodeQL"} {
			if !strings.HasPrefix(ran, "rm -rf ") || !strings.Contains(ran, s) {
				t.Errorf("%s: it does not remove %s as root (ran %q)", c.name, s, ran)
			}
		}
		if strings.Count(out, "Filesystem") < 2 {
			t.Errorf("%s: it does not print df -h / before and after:\n%s", c.name, out)
		}
	}
	for _, avail := range []string{"", "-", "1.5G"} {
		if out, ran, err := freeDisk(t, avail); err == nil || ran != "" {
			t.Errorf("df giving %q: error %v, ran %q, want a failure removing nothing\n%s", avail, err, ran, out)
		}
	}
}
