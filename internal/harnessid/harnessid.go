// Package harnessid names the coding tools (harnesses) whose activity
// backsight records, as the `client` field of telemetry events, feedback
// journal lines and the metrics index. It is a leaf package so that the
// writers and the index can share the ids without importing each other.
package harnessid

// The ids, also the values of the `harness:` tag.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
)

// Or is id, or ClaudeCode when id is empty: every record written before
// the field existed came from Claude Code.
func Or(id string) string {
	if id == "" {
		return ClaudeCode
	}
	return id
}
