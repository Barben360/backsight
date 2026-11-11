package harness

import (
	"os"
	"path/filepath"
	"strings"
)

// codexRolloutHome is a Codex home holding the recorded root rollout where
// Codex keeps it, and that rollout's path: what a real Conversations finds.
func codexRolloutHome() (home, rollout string) {
	home, _ = os.MkdirTemp("", "codex-home-")
	rollout = filepath.Join(home, "sessions", "2026", "11", "10", "rollout-2026-11-10T09-00-00-01a1c72b-9533-7bb0-9002-52dca295a387.jsonl")
	raw, _ := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "rollouts", "root.jsonl"))
	_ = os.MkdirAll(filepath.Dir(rollout), 0o700)
	_ = os.WriteFile(rollout, raw, 0o600)
	return home, rollout
}

// codex_rollout.go's ledger: block 6, https://github.com/Barben360/backsight/issues/281.
func init() {
	codexOwns("codex_rollout.go", codexPart{
		stubs: map[string]func() bool{
			"Conversations": func() bool {
				home, _ := codexRolloutHome()
				defer func() { _ = os.RemoveAll(home) }()
				return (Codex{}).Conversations(Env{Home: home, Profile: home}) == nil
			},
			"SessionOf": func() bool {
				return (Codex{}).SessionOf("/x/rollout-2026-11-10T09-00-00-01a1c72b-9533-7bb0-9002-52dca295a387.jsonl") == ""
			},
			"Size": func() bool {
				home, rollout := codexRolloutHome()
				defer func() { _ = os.RemoveAll(home) }()
				_, err := (Codex{}).Size(rollout)
				return notImplemented(err)
			},
			"Open": func() bool {
				home, rollout := codexRolloutHome()
				defer func() { _ = os.RemoveAll(home) }()
				_, err := (Codex{}).Open(rollout, 0)
				return notImplemented(err)
			},
			"ReadConversation": func() bool {
				raw, _ := os.ReadFile(filepath.Join("..", "testdata", "codex", "0.167.0", "rollouts", "root.jsonl"))
				_, _, err := (Codex{}).ReadConversation(strings.NewReader(string(raw)), 0)
				return notImplemented(err)
			},
		},
	})
}
