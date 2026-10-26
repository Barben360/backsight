package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRecallPrompt answers recall-prompt like backsight would for a
// billing prompt: with recall.families on, the family's memory comes from
// the project scope; off, from the global one, beside a foreign memory.
const fakeRecallPrompt = `#!/bin/sh
cat > /dev/null
cfg="$BACKSIGHT_HOME/config/config.toml"
printf '%s\n' "$*" >> "$(dirname "$0")/calls"
if grep -q '^families = true$' "$cfg"; then
  echo '{"data":{"project":"billing","linked":["billing-spec"],"family":["billing-spec"],"block":"b","injected":["m1","fam"],"ranked":[
    {"id":"m1","text":"own fact","project":"billing","scope":"project","score":0.9},
    {"id":"fam","text":"spec fact","project":"billing-spec","scope":"project","score":0.7}]}}'
else
  echo '{"data":{"project":"billing","linked":[],"family":["billing-spec"],"block":"b","injected":["m1","fam","alien"],"ranked":[
    {"id":"m1","text":"own fact","project":"billing","scope":"project","score":0.9},
    {"id":"fam","text":"spec fact","project":"billing-spec","scope":"global","score":0.7},
    {"id":"alien","text":"foreign fact","project":"garden","scope":"global","score":0.66}]}}'
fi
`

func TestReplayFamiliesComparesBothVariants(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	root, _ := filepath.Abs("..")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o700)
	_ = os.WriteFile(filepath.Join(bin, "backsight"), []byte(fakeRecallPrompt), 0o700)
	src := filepath.Join(dir, "src")
	_ = os.MkdirAll(filepath.Join(src, "config"), 0o700)
	_ = os.MkdirAll(filepath.Join(src, "state"), 0o700)
	cfg := "[server]\nurl = \"http://127.0.0.1:1\"\n\n[recall]\nmin_score = 0.5\nfamilies = false\n\n[llm]\nprovider = \"mock\"\n"
	_ = os.WriteFile(filepath.Join(src, "config", "config.toml"), []byte(cfg), 0o600)
	_ = os.WriteFile(filepath.Join(src, "config", "secrets.toml"), []byte("api_key = \"k\"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "state", "families.json"), []byte(`{"of":{"billing":["billing-spec"]}}`), 0o600)
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o700)
	_ = os.WriteFile(filepath.Join(data, "prompts.json"), []byte(`[{"qid":"q1","cwd":"/w/billing","text":"How do invoices round?","lang":"en"},{"qid":"q2","cwd":"/w/billing","text":"And taxes?","lang":"en"}]`), 0o600)

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "replay-families.sh"), data, filepath.Join(bin, "backsight"))
	cmd.Env = append(os.Environ(), "BACKSIGHT_SOURCE_CONFIG="+filepath.Join(src, "config"), "BACKSIGHT_SOURCE_STATE="+filepath.Join(src, "state"), "KEEP_HOMES=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "base: 2 prompts (0 failed), 2 with a family, 6 injections, 2 out of family") ||
		!strings.Contains(string(out), "families: 2 prompts (0 failed), 2 with a family, 4 injections, 0 out of family") {
		t.Errorf("summary:\n%s", out)
	}
	var base map[string]struct {
		Ranked   [][]any           `json:"ranked"`
		Injected []string          `json:"injected"`
		Projects map[string]string `json:"projects"`
	}
	raw, _ := os.ReadFile(filepath.Join(data, "replay-base.json"))
	if err := json.Unmarshal(raw, &base); err != nil || len(base["q1"].Ranked) != 3 || base["q1"].Ranked[0][0] != "m1" || base["q1"].Projects["alien"] != "garden" {
		t.Errorf("replay-base.json (the bench's format) %s %v", raw, err)
	}
	mem, _ := os.ReadFile(filepath.Join(data, "mem.json"))
	if !strings.Contains(string(mem), `"alien": "foreign fact"`) && !strings.Contains(string(mem), `"alien":"foreign fact"`) {
		t.Errorf("mem.json %s", mem)
	}
	for variant, want := range map[string]string{"base": "families = false", "families": "families = true"} {
		c, _ := os.ReadFile(filepath.Join(data, "homes", variant, "config", "config.toml"))
		if strings.Count(string(c), "families =") != 1 || !strings.Contains(string(c), want) || !strings.Contains(string(c), "min_score = 0.5") {
			t.Errorf("%s config:\n%s", variant, c)
		}
		if _, err := os.Stat(filepath.Join(data, "homes", variant, "state", "families.json")); err != nil {
			t.Errorf("%s: families file not copied: %v", variant, err)
		}
	}
	if c, _ := os.ReadFile(filepath.Join(src, "config", "config.toml")); string(c) != cfg {
		t.Errorf("the source config was changed:\n%s", c)
	}
	// without KEEP_HOMES the homes, and their copy of the secrets, are gone
	cmd = exec.Command("bash", filepath.Join(root, "scripts", "replay-families.sh"), data, filepath.Join(bin, "backsight"))
	cmd.Env = append(os.Environ(), "BACKSIGHT_SOURCE_CONFIG="+filepath.Join(src, "config"), "BACKSIGHT_SOURCE_STATE="+filepath.Join(src, "state"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(data, "homes")); err == nil {
		t.Error("the throwaway homes, with a copy of the secrets, were left behind")
	}
}
