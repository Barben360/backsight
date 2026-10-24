package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
)

func TestStoreSendsAtOnce(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	before := retained.Load()
	code, out, _ := run(t, "store", "--kind", "decision", "--project", "billing", "Billing", "keeps", "PostgreSQL", "16.", "--json")
	var env struct {
		Data storeResult `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || env.Data.Status != "stored" || env.Data.Project != "billing" || !strings.HasPrefix(env.Data.Document, "note:") || retained.Load() != before+1 {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 0 {
		t.Errorf("%d notes left in the outbox", n)
	}
}

func TestStoreQueuesWhenServerDown(t *testing.T) {
	home := devInstall(t, true) // closed port
	code, out, _ := run(t, "store", "--kind", "lesson", "--project", "p", "Retry with backoff.")
	if code != output.OK || !strings.Contains(out, "queued") {
		t.Errorf("exit %d\n%s", code, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 1 {
		t.Errorf("want 1 queued note, got %d", n)
	}
}

func TestStoreRefusals(t *testing.T) {
	devInstall(t, true)
	secret := "sk-" + "ant-" + strings.Repeat("Ab3dE5gH9k", 3)
	cases := map[string][]string{
		"bad kind":   {"store", "--kind", "idea", "text"},
		"no text":    {"store", "--kind", "lesson"},
		"blank text": {"store", "--kind", "lesson", "  "},
		"secret":     {"store", "--kind", "lesson", "use", secret},
	}
	for name, args := range cases {
		code, out, errOut := run(t, args...)
		if code != output.Usage || strings.Contains(out+errOut, secret) {
			t.Errorf("%s: exit %d %q %q", name, code, out, errOut)
		}
	}
}
