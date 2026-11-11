package config

import (
	"slices"
	"testing"
)

// The former default, as setup wrote it, becomes today's on its own line,
// its comment and the file's other lines kept; it then reads as the
// default does.
func TestMigrateSkipEntrypointsReplacesTheFormerDefault(t *testing.T) {
	for _, old := range []string{
		"[server]\nbank = \"backsight\"\n\n[privacy]\nexclude_paths = []\nskip_entrypoints = [\"sdk-*\"] # headless runs\n\n[components]\nread = true\n",
		"[privacy]\r\nskip_entrypoints=[ 'sdk-*' ]\r\n",
	} {
		out, changed, err := MigrateSkipEntrypoints([]byte(old))
		if err != nil || !changed {
			t.Fatalf("%q: changed %v, %v", old, changed, err)
		}
		cfg, err := Parse(out, "config.toml", dirs, "/home/u")
		if err != nil || !slices.Equal(cfg.Privacy.SkipEntrypoints, DefaultSkipEntrypoints()) {
			t.Errorf("%q migrated reads %v, %v\n%s", old, cfg.Privacy.SkipEntrypoints, err, out)
		}
		before, _ := Parse([]byte(old), "config.toml", dirs, "/home/u")
		before.Privacy.SkipEntrypoints = DefaultSkipEntrypoints()
		if !SameValues(before, cfg) {
			t.Errorf("%q: another value changed\n%s", old, out)
		}
	}
	out, _, _ := MigrateSkipEntrypoints([]byte("[privacy]\nskip_entrypoints = [\"sdk-*\"] # headless runs\n"))
	if want := "[privacy]\nskip_entrypoints = [\"sdk-*\", \"codex_exec\", \"codex_sdk_*\", \"codex-app-server-sdk\", \"codex_mcp_server\"] # headless runs\n"; string(out) != want {
		t.Errorf("edited line:\n%s\nwant\n%s", out, want)
	}
}

// Any other value is the user's choice: a list of their own, an empty one
// (send everything), today's default; a file without the key reads the
// default anyway; a form a line edit cannot change stays as written. None
// is changed.
func TestMigrateSkipEntrypointsLeavesOtherValues(t *testing.T) {
	for name, content := range map[string]string{
		"own list":      "[privacy]\nskip_entrypoints = [\"sdk-cli\"]\n",
		"own superset":  "[privacy]\nskip_entrypoints = [\"sdk-*\", \"codex_exec\"]\n",
		"empty":         "[privacy]\nskip_entrypoints = []\n",
		"today's":       "[privacy]\nskip_entrypoints = [\"sdk-*\", \"codex_exec\", \"codex_sdk_*\", \"codex-app-server-sdk\", \"codex_mcp_server\"]\n",
		"absent key":    "[privacy]\nexclude_paths = []\n",
		"no privacy":    "[server]\nbank = \"backsight\"\n",
		"empty file":    "",
		"dotted key":    "privacy.skip_entrypoints = [\"sdk-*\"]\n",
		"inline table":  "privacy = { skip_entrypoints = [\"sdk-*\"] }\n",
		"several lines": "[privacy]\nskip_entrypoints = [\n  \"sdk-*\",\n]\n",
		"not toml":      "[privacy\nskip_entrypoints = [\"sdk-*\"]\n",
	} {
		out, changed, err := MigrateSkipEntrypoints([]byte(content))
		if err != nil || changed || string(out) != content {
			t.Errorf("%s: changed %v, %v\n%s", name, changed, err, out)
		}
	}
}
