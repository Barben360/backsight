package retain

import (
	"io"

	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// Harness is what retain needs of the harness (the coding tool) whose
// conversations and notes it sends: its id, the harness: tag its memories
// carry, and its conversation parser. A harness.Adapter is one; the
// caller hands it over, since retain cannot import the adapters (they
// import instructions, whose tests import retain).
type Harness interface {
	Identity
	ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error)
}

// Identity names a harness: its id and its harness: tag.
type Identity interface {
	ID() string
	Tag() string
}

// claudeCode is the harness of a flusher, a segment or a note that names
// none: Claude Code's, as everything was before harnesses had ids. Its id
// and tag are harness.Default()'s (a cli test holds them equal).
type claudeCode struct{}

func (claudeCode) ID() string  { return state.HarnessClaudeCode }
func (claudeCode) Tag() string { return "harness:" + state.HarnessClaudeCode }
func (claudeCode) ReadConversation(r io.Reader, from int64) ([]transcript.Record, transcript.Stats, error) {
	return transcript.Read(r, from)
}

// DefaultHarness is the harness of what names none (Claude Code's).
func DefaultHarness() Harness { return claudeCode{} }
