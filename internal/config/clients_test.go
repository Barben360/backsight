package config

import (
	"strings"
	"testing"
)

// Per-tool tables (issue 277): Claude Code's settings live in
// [clients.claude_code]; a file written before keeps them in [client],
// which is still read, and refused with its own key name.

func TestClientsTableIsRead(t *testing.T) {
	for name, body := range map[string]string{
		"new table":    "[clients.claude_code]\nretention_days = 90\n",
		"older table":  "[client]\nretention_days = 90\n",
		"both, equal":  "[client]\nretention_days = 90\n[clients.claude_code]\nretention_days = 90\n",
		"dotted":       "clients.claude_code.retention_days = 90\n",
		"older dotted": "client.retention_days = 90\n",
	} {
		cfg, err := Load(write(t, body), dirs, "/home/u")
		if err != nil || cfg.Clients.ClaudeCode.RetentionDays != 90 {
			t.Errorf("%s: retention %d, %v", name, cfg.Clients.ClaudeCode.RetentionDays, err)
		}
	}
	// Codex has a table, with no key yet.
	if _, err := Load(write(t, "[clients.codex]\n"), dirs, "/home/u"); err != nil {
		t.Errorf("an empty [clients.codex] is refused: %v", err)
	}
}

func TestClientsTableRefusals(t *testing.T) {
	for body, want := range map[string]string{
		"[clients.claude_code]\nretention_days = -1\n":                                "clients.claude_code.retention_days: must be ≥ 0, got -1",
		"[client]\nretention_days = -1\n":                                             "client.retention_days: must be ≥ 0, got -1",
		"[client]\nretention_days = 30\n[clients.claude_code]\nretention_days = 90\n": "client.retention_days = 30 and clients.claude_code.retention_days = 90 disagree",
		"[client]\nretention_days = \"long\"\n":                                       "retention_days",
		"[client]\ncodex = 1\n":                                                       "unknown keys: client.codex",
		"[clients.codex]\nretention_days = 1\n":                                       "unknown keys: clients.codex.retention_days",
		"[clients.cursor]\n":                                                          "unknown keys: clients.cursor",
	} {
		_, err := Load(write(t, body), dirs, "/home/u")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error with %q", body, err, want)
		}
	}
}

func TestMigrateClientTable(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"renamed in place", "schema_version = 1\n# mine\n[client]\nretention_days = 3650 # legal hold\n\n[server]\nbank = \"b\"\n",
			"schema_version = 1\n# mine\n[clients.claude_code]\nretention_days = 3650 # legal hold\n\n[server]\nbank = \"b\"\n"},
		{"header comment and spaces", "  [ client ]  # Claude\r\nretention_days = 7\r\n", "  [clients.claude_code]  # Claude\r\nretention_days = 7\r\n"},
		{"empty table", "[client]\n[llm]\n", "[clients.claude_code]\n[llm]\n"},
		{"into the new table", "[client]\nretention_days = 30\n\n[clients.claude_code]\n[llm]\n",
			"\n[clients.claude_code]\nretention_days = 30\n[llm]\n"},
		{"the same value twice", "[clients.claude_code]\nretention_days = 3650\n[client]\nretention_days = 3_650\n",
			"[clients.claude_code]\nretention_days = 3650\n"},
		{"a comment keeps the older header", "[client]\n# why\nretention_days = 30\n[clients.claude_code]\n",
			"[client]\n# why\n[clients.claude_code]\nretention_days = 30\n"},
	}
	for _, c := range cases {
		got, changed, err := MigrateClientTable([]byte(c.in))
		if err != nil || !changed || string(got) != c.want {
			t.Errorf("%s:\ngot  %q (changed %v, %v)\nwant %q", c.name, got, changed, err, c.want)
			continue
		}
		before, err1 := Parse([]byte(c.in), "before", dirs, "/home/u")
		after, err2 := Parse(got, "after", dirs, "/home/u")
		if err1 != nil || err2 != nil || !SameValues(before, after) || after.legacyClient {
			t.Errorf("%s: the migration changed what the file says: %v %v\n%+v\n%+v", c.name, err1, err2, before.Clients, after.Clients)
		}
	}
	for _, in := range []string{"", "[llm]\n", "[clients.claude_code]\nretention_days = 1\n", "x = \"\"\"\nmulti\n\"\"\"\n"} {
		if got, changed, err := MigrateClientTable([]byte(in)); err != nil || changed || string(got) != in {
			t.Errorf("%q: nothing to move, got %q changed %v %v", in, got, changed, err)
		}
	}
	for _, in := range []string{
		"client.retention_days = 30\n",
		"client = { retention_days = 30 }\n",
		"[client]\nretention_days = 30\nx = '''\n'''\n",
		"[client]\nretention_days = 30\n[clients.claude_code]\nretention_days = 90\n",
	} {
		if got, _, err := MigrateClientTable([]byte(in)); err == nil {
			t.Errorf("%q: accepted, gave %q", in, got)
		}
	}
}

// client retention writes the new table, moving an older one first, and
// never leaves the value in both.
func TestSetClientRetentionMovesTheOlderTable(t *testing.T) {
	got, err := SetClientRetention([]byte("[client]\nretention_days = 30\n[clients.claude_code]\n"), 90)
	if want := "[clients.claude_code]\nretention_days = 90\n"; err != nil || string(got) != want {
		t.Errorf("got %q (%v), want %q", got, err, want)
	}
	if _, err := SetClientRetention([]byte("[client]\nretention_days = 30\n[clients.claude_code]\nretention_days = 90\n"), 90); err == nil {
		t.Error("two different values were merged")
	}
	if _, err := SetClientRetention([]byte("clients.claude_code.retention_days = 30\n"), 90); err == nil {
		t.Error("a dotted key was edited")
	}
}
