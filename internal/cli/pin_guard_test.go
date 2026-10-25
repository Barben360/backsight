package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/setup"
)

// Images ordered around the default pin (Hindsight 1.1.0) in these tests.
const (
	olderImage     = "ghcr.io/vectorize-io/hindsight:1.0.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	unorderedImage = "ghcr.io/vectorize-io/hindsight:1.1.0@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// deployFile writes a compose.yaml whose hindsight service runs image, as
// an earlier binary rendered it.
func deployFile(t *testing.T, home, image string) {
	t.Helper()
	if err := os.MkdirAll(composeDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "name: backsight\n\nservices:\n  db:\n    image: " + server.PostgresImage + "\n  hindsight:\n    image: " + image + "\n"
	if err := os.WriteFile(filepath.Join(composeDir(home), "compose.yaml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// redeploy swaps the hindsight image of the rendered compose.yaml, as
// another binary's server upgrade would have.
func redeploy(t *testing.T, home, image string) {
	t.Helper()
	p := filepath.Join(composeDir(home), "compose.yaml")
	raw, err := os.ReadFile(p)
	old := server.ServiceImage(raw, "hindsight")
	if err != nil || old == "" {
		t.Fatalf("no deployed image in %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "    image: "+old+"\n", "    image: "+image+"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if deployedImage(t, home) != image {
		t.Fatalf("redeploy: %s", deployedImage(t, home))
	}
}

// Decision 0067 block review: only server upgrade (a verified backup
// first, a rollback on failure) and update apply move the server to
// another version. server start, install and render refuse a pin other
// than the deployed one, and name the command that moves it.
func TestServerCommandsRefuseAPinOtherThanTheDeployedOne(t *testing.T) {
	for _, c := range []struct {
		name, pin, want string
	}{
		{"newer pin", newImage, "older than this binary's " + newImage + ": run backsight server upgrade"},
		{"older pin", olderImage, "newer than this binary's " + olderImage + ": run backsight update apply"},
		{"unordered pin", unorderedImage, "backsight server upgrade --allow-unordered"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := devInstall(t, true)
			f := withCompose(t)
			oldTimeout := healthTimeout
			healthTimeout = 50 * time.Millisecond // an install that went on would wait for a server
			t.Cleanup(func() { healthTimeout = oldTimeout })
			deployed := server.HindsightImage
			if code, out, _ := run(t, "server", "render"); code != output.OK {
				t.Fatalf("render: exit %d\n%s", code, out)
			}
			pinImage(t, c.pin)
			for _, args := range [][]string{{"server", "start"}, {"server", "start", "--plan"}, {"server", "install"}, {"server", "render"}} {
				f.calls = nil
				code, out, _ := run(t, append(args, "--json")...)
				var env struct{ Error *output.Error }
				_ = json.Unmarshal([]byte(out), &env)
				if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, c.want) || !strings.Contains(env.Error.Message, deployed) {
					t.Errorf("%v: exit %d, want 20 naming %q\n%s", args, code, c.want, out)
				}
				if got := deployedImage(t, home); got != deployed {
					t.Errorf("%v deployed %s", args, got)
				}
				if len(f.calls) != 0 {
					t.Errorf("%v ran compose: %q", args, f.subcommands())
				}
			}
		})
	}
}

// The deployed version equal to the pin, or no server yet: they run.
func TestServerCommandsRunOnTheDeployedPin(t *testing.T) {
	home := devInstall(t, true)
	f := withCompose(t)
	if code, out, _ := run(t, "server", "start"); code != output.OK { // nothing deployed yet
		t.Fatalf("first start: exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "server", "start"); code != output.OK {
		t.Fatalf("start on the pin: exit %d\n%s", code, out)
	}
	if got := strings.Join(f.subcommands(), " | "); got != "up -d --wait | up -d --wait" || deployedImage(t, home) != server.HindsightImage {
		t.Errorf("compose %s, deployed %s", got, deployedImage(t, home))
	}
}

// server.digest in config.toml is the user's own choice of image (restore
// names it for an archive of another version): server start deploys it.
func TestServerStartDeploysTheConfiguredDigest(t *testing.T) {
	home := devInstall(t, true)
	withCompose(t)
	deployFile(t, home, newImage)
	digest := "sha256:" + strings.Repeat("d", 64)
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(closedPort(t))+"\"\nui_port = 19999\ndigest = \""+digest+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "server", "start", "--json"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := deployedImage(t, home); got != "ghcr.io/vectorize-io/hindsight@"+digest {
		t.Errorf("deployed %s", got)
	}
}

// Setup's server step refuses before it records that it started a server
// (undo would stop the one running) and before server install runs.
func TestSetupServerStepRefusesAnotherDeployedVersion(t *testing.T) {
	home, f := setupMachine(t)
	withLLMKey(t, home)
	_ = os.WriteFile(pathsOf(home).ConfigFile(), []byte("schema_version = 1\n"), 0o600) // adopted
	deployFile(t, home, olderImage)
	code, out, _ := run(t, "setup", "--answer", "consent=yes", "--non-interactive", "--json")
	env := parseSetup(t, out)
	if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "older than this binary's "+server.HindsightImage+": run backsight server upgrade") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, c := range f.commands() {
		if c == "server install" {
			t.Errorf("server install ran: %v", f.commands())
		}
	}
	if rec, _ := setup.Load(pathsOf(home).SetupFile()); rec != nil && rec.Undo.ServerStarted {
		t.Error("setup recorded a server it did not start")
	}
	if deployedImage(t, home) != olderImage {
		t.Errorf("deployed %s", deployedImage(t, home))
	}
}

// Pins that cannot be ordered (same tag, another digest: a rebuild) are
// not an upgrade server upgrade can tell from a downgrade: refused unless
// --allow-unordered.
func TestServerUpgradeRefusesUnorderedPinsUnlessAllowed(t *testing.T) {
	home, dk, _, oldImage := upgradeSetup(t)
	pinImage(t, unorderedImage)
	for _, args := range [][]string{{"server", "upgrade"}, {"server", "upgrade", "--plan"}} {
		dk.calls = nil
		code, out, _ := run(t, append(args, "--json")...)
		env := decodeUpgrade(t, out)
		if code != output.Blocked || env.Error == nil || !strings.Contains(env.Error.Message, "--allow-unordered") || !strings.Contains(env.Error.Message, oldImage) {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
		if len(dk.calls) != 0 || deployedImage(t, home) != oldImage || len(backupsIn(home)) != 1 {
			t.Errorf("%v acted: calls %q, deployed %s", args, dk.calls, deployedImage(t, home))
		}
	}
	code, out, errOut := run(t, "server", "upgrade", "--allow-unordered", "--json")
	if env := decodeUpgrade(t, out); code != output.OK || !env.Data.Upgrade || env.Data.Target != unorderedImage {
		t.Fatalf("--allow-unordered: exit %d\n%s%s", code, out, errOut)
	}
	if got := deployedImage(t, home); got != unorderedImage {
		t.Errorf("deployed %s", got)
	}
}

// update apply passes --allow-unordered on to the new binary's server
// upgrade; without it the update stops at the server step and rolls back.
func TestUpdateApplyPassesAllowUnordered(t *testing.T) {
	home, _, _, _, bins, oldImage := serverInstalledAt(t, "0.1.1", "0.2.1")
	bins.images["0.2.1"] = unorderedImage
	src := t.TempDir()
	signedRelease(t, src, "0.2.1", trustTestKey(t), relOpts{})

	code, out, _ := run(t, "update", "apply", "--plan", "--source", src, "--json")
	if code != output.Blocked || !strings.Contains(out, "--allow-unordered") {
		t.Errorf("plan without the flag: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "update", "apply", "--plan", "--allow-unordered", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || env.Data.Server == nil || env.Data.Server.Target != unorderedImage {
		t.Fatalf("plan with the flag: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "update", "apply", "--source", src, "--json")
	if code != output.RolledBack || !strings.Contains(out, "--allow-unordered") || deployedImage(t, home) != oldImage {
		t.Errorf("without the flag: exit %d, deployed %s\n%s", code, deployedImage(t, home), out)
	}
	bins.calls = nil
	code, out, errOut := run(t, "update", "apply", "--allow-unordered", "--source", src, "--json")
	if env := decodeApply(t, out); code != output.OK || env.Data.Server == nil || !env.Data.Server.Upgraded {
		t.Fatalf("with the flag: exit %d\n%s%s", code, out, errOut)
	}
	if deployedImage(t, home) != unorderedImage || bins.ran("0.2.1 server upgrade --allow-unordered") != 1 || bins.ran("0.2.1 server upgrade --plan --allow-unordered") != 1 {
		t.Errorf("deployed %s, calls %q", deployedImage(t, home), bins.calls)
	}
}
