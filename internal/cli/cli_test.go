package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aead.dev/minisign"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/signing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(args, &out, &errb)
	return code, out.String(), errb.String()
}

func setBuild(t *testing.T, version, commit string) {
	t.Helper()
	oldV, oldC := buildinfo.Version, buildinfo.Commit
	buildinfo.Version, buildinfo.Commit = version, commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldV, oldC })
}

func TestVersionJSON(t *testing.T) {
	setBuild(t, "0.1.0-test", "abc1234")
	code, out, _ := run(t, "version", "--json")
	if code != output.OK {
		t.Fatalf("exit %d", code)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		OK            bool   `json:"ok"`
		Data          struct {
			Version, Commit, Go, OS, Arch string
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if env.Command != "version" || !env.OK || env.Data.Version != "0.1.0-test" || env.Data.Commit != "abc1234" || env.Data.OS == "" || env.Data.Arch == "" || env.Data.Go == "" {
		t.Errorf("unexpected: %+v", env)
	}
}

func TestVersionText(t *testing.T) {
	setBuild(t, "0.1.0-test", "abc1234")
	code, out, _ := run(t, "version")
	if code != output.OK || !strings.HasPrefix(out, "backsight 0.1.0-test") {
		t.Errorf("exit %d, out %q", code, out)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	code, _, errOut := run(t, "no-such-command")
	if code != output.Usage {
		t.Errorf("exit %d, want %d", code, output.Usage)
	}
	if !strings.Contains(errOut, "no-such-command") {
		t.Errorf("stderr should name the bad command: %q", errOut)
	}
}

func TestUnknownCommandJSONIsEnvelope(t *testing.T) {
	code, out, _ := run(t, "--json", "no-such-command")
	if code != output.Usage {
		t.Fatalf("exit %d", code)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("--json must print an envelope even on usage errors: %v\n%q", err, out)
	}
	if env["ok"] != false {
		t.Errorf("%v", env)
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	if code, _, _ := run(t, "version", "--bogus"); code != output.Usage {
		t.Errorf("exit %d, want %d", code, output.Usage)
	}
}

func TestExtraArgumentsAreUsageErrors(t *testing.T) {
	for _, cmd := range []string{"version", "paths", "doctor"} {
		if code, _, _ := run(t, cmd, "extra"); code != output.Usage {
			t.Errorf("%s extra: exit %d, want %d", cmd, code, output.Usage)
		}
	}
}

func TestUnknownOrMissingSubcommandIsAUsageError(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"server", "bogus"}, {"client", "config", "bogus"}, {"server"}, {"client"}, {"retain"}} {
		if code, _, _ := run(t, args...); code != output.Usage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	code, out, _ := run(t, "--json", "client", "config", "bogus")
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.Usage || env.OK || env.Error.Code != output.Usage {
		t.Errorf("exit %d envelope %s", code, out)
	}
}

type versionKeys struct {
	Data struct {
		SigningKeys []signing.Key `json:"signing_keys"`
		TestKey     bool          `json:"test_key"`
	} `json:"data"`
}

// version --json lists the embedded keys; the release pipeline reads it.
func TestVersionJSONListsTheSigningKeys(t *testing.T) {
	old := signing.TestKey
	t.Cleanup(func() { signing.TestKey = old })

	signing.TestKey = ""
	_, out, _ := run(t, "version", "--json")
	var v versionKeys
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Data.SigningKeys) != 2 || v.Data.SigningKeys[0].Role != "current" || v.Data.SigningKeys[1].Role != "next" ||
		v.Data.SigningKeys[0].ID == "" || v.Data.SigningKeys[0].ID == v.Data.SigningKeys[1].ID || v.Data.TestKey {
		t.Errorf("release build: %s", out)
	}

	pub, _, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signing.TestKey = pub.String()
	id := fmt.Sprintf("%016X", pub.ID())
	_, out, _ = run(t, "version", "--json")
	v = versionKeys{}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Data.SigningKeys) != 3 || v.Data.SigningKeys[2] != (signing.Key{ID: id, Role: "test"}) || !v.Data.TestKey {
		t.Errorf("test build: %s", out)
	}
	if _, text, _ := run(t, "version"); !strings.Contains(text, "test signing key "+id) {
		t.Errorf("the text output hides the test key: %q", text)
	}
}
