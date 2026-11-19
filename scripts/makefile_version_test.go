package scripts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// make build names its version as a release build does: goreleaser writes the tag without its v
// ("0.1.27-m7"), git describe with it ("v0.1.27-m7"), and the two spellings made two rows of one version on
// the Releases page. The probe prints the VERSION the Makefile computes, with a git on PATH that answers
// describe as given; a version given on the command line is kept as it is.
func TestMakeVersionDropsTheTagsV(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake git is a shell script")
	}
	for _, c := range []struct{ describe, want string }{
		{"v0.1.27-m7", "0.1.27-m7"},
		{"v1.6.0-3-g65e358c8-dirty", "1.6.0-3-g65e358c8-dirty"},
		{"65e358c8", "65e358c8"}, // no tag: the commit, as it is
	} {
		dir := t.TempDir()
		fake := "#!/bin/sh\ncase \"$1\" in describe) echo '" + c.describe + "' ;; *) echo abc123 ;; esac\n"
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		env := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
		if got := makeOut(t, env, `--eval=vprobe: ; @echo "$(VERSION)"`, "vprobe"); got != c.want {
			t.Errorf("git describe %q: VERSION %q, want %q", c.describe, got, c.want)
		}
	}
	if got := makeOut(t, nil, `--eval=vprobe: ; @echo "$(VERSION)"`, "vprobe", "VERSION=v9.9.9"); got != "v9.9.9" {
		t.Errorf("VERSION=v9.9.9 on the command line: %q, want it kept", got)
	}
}
