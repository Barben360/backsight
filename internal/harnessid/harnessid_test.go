package harnessid

import "testing"

func TestOr(t *testing.T) {
	if got := Or(""); got != ClaudeCode {
		t.Errorf("Or(\"\") = %q, want %q", got, ClaudeCode)
	}
	if got := Or(Codex); got != Codex {
		t.Errorf("Or(codex) = %q, want codex", got)
	}
}
