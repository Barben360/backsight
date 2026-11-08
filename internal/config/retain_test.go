package config

import (
	"strings"
	"testing"
	"time"
)

// [retain]: the append gate (decision 0083 C1). Defaults 20,000 characters
// and 6 h; 0 for either turns the gate off; a negative count, or a wait
// that is not a Go duration ≥ 0, is refused with its key.
func TestRetainAppendGateKeys(t *testing.T) {
	r := Default(dirs, "/home/u").Retain
	if r.AppendMinChars != 20_000 || r.AppendMaxWaitDuration() != 6*time.Hour {
		t.Fatalf("defaults %+v", r)
	}
	cfg, err := Load(write(t, "[retain]\nappend_min_chars = 0\nappend_max_wait = \"90m\"\n"), dirs, "/home/u")
	if err != nil || cfg.Retain.AppendMinChars != 0 || cfg.Retain.AppendMaxWaitDuration() != 90*time.Minute {
		t.Fatalf("read: %v %+v", err, cfg.Retain)
	}
	for body, key := range map[string]string{
		"append_min_chars = -1":      "retain.append_min_chars",
		`append_max_wait = "soon"`:   "retain.append_max_wait",
		`append_max_wait = "-1h"`:    "retain.append_max_wait",
		`append_max_wait = ""`:       "retain.append_max_wait",
		"append_max_wait = 6":        "append_max_wait",
		"append_min_chars = \"20k\"": "append_min_chars",
	} {
		_, err := Load(write(t, "[retain]\n"+body+"\n"), dirs, "/home/u")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: got %v, want an error naming %s", body, err, key)
		}
	}
	if _, err := Load(write(t, "[retain]\nappend_max_wait = \"0s\"\n"), dirs, "/home/u"); err != nil {
		t.Errorf("0s refused: %v", err)
	}
}
