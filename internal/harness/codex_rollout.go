package harness

import (
	"io"

	"github.com/Barben360/backsight/internal/transcript"
)

// Codex's conversation store, its rollouts: block 6
// (https://github.com/Barben360/backsight/issues/281), 0085 D7. Every
// method is a stub until then.

// Conversations are $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread
// id>.jsonl and their .jsonl.zst compressed forms. Block 6; none until
// then.
func (Codex) Conversations(env Env) []string { return nil }

// SessionOf is the thread id ending a rollout's name. Block 6; "" (not one
// of Codex's) until then.
func (Codex) SessionOf(path string) string { return "" }

// Size is the rollout's content length, decompressed for .jsonl.zst.
// Block 6.
func (Codex) Size(path string) (int64, error) { return 0, notYet("rollout size") }

// Open is the rollout's content from offset from, decompressed for
// .jsonl.zst. Block 6.
func (Codex) Open(path string, from int64) (io.ReadCloser, error) {
	return nil, notYet("rollout reading")
}

// ReadConversation normalises rollout records (session_meta,
// response_item, event_msg…), unknown types counted. Block 6.
func (Codex) ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error) {
	return nil, transcript.Stats{}, notYet("rollout reading")
}
