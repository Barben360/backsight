package retain

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/transcript"
)

// The readers of one conversation, whichever harness wrote it. Claude
// Code's go through its transcript package exactly as before; another
// harness's (Codex's rollouts, decision 0085 D7) through its adapter's
// Open and ReadConversation, the records standing in for what Claude
// Code's cheaper scans read from the raw lines.

// harnessOf is the harness among hs whose conversation path is: the first
// one other than Claude Code's that recognises it (SessionOf), else
// Claude Code's (whose SessionOf takes any file name), fallback when
// non-nil, else the default one.
func harnessOf(hs []Harness, fallback Harness, path string) Harness {
	if fallback != nil && !isClaude(fallback) && fallback.SessionOf(path) != "" {
		return fallback
	}
	for _, h := range hs {
		if h != nil && !isClaude(h) && h.SessionOf(path) != "" {
			return h
		}
	}
	if fallback != nil && isClaude(fallback) {
		return fallback
	}
	for _, h := range hs {
		if h != nil && isClaude(h) {
			return h
		}
	}
	return claudeCode{}
}

func isClaude(h Identity) bool { return h.ID() == harnessid.ClaudeCode }

// HarnessOf is the harness among hs whose conversation path is: the one
// other than Claude Code's that recognises it, else Claude Code's.
func HarnessOf(hs []Harness, path string) Harness { return harnessOf(hs, nil, path) }

// SessionOf is the session id of the conversation at path, read by its
// harness (harnessFor).
func (f *Flusher) SessionOf(path string) string { return f.harnessFor(path).SessionOf(path) }

// ReadConversation is every complete record of the conversation at path,
// read by h.
func ReadConversation(h Harness, path string) ([]transcript.Record, transcript.Stats, error) {
	return readConversation(h, path)
}

// EntrypointOf is how the conversation at path, read by h, was started
// (Claude Code's entrypoint, Codex's originator); "" when unknown.
func EntrypointOf(h Harness, path string) string { return entrypointOf(h, path) }

// harnessFor is the harness of the conversation at path: f.Harness when
// it is that harness's, else one of f.Harnesses (harnessOf).
func (f *Flusher) harnessFor(path string) Harness { return harnessOf(f.Harnesses, f.Harness, path) }

// harnessFor is the harness of the conversation at path, among a's.
func (a Attributor) harnessFor(path string) Harness { return harnessOf(a.Harnesses, nil, path) }

// readConversation is every complete record of the conversation at path.
func readConversation(h Harness, path string) ([]transcript.Record, transcript.Stats, error) {
	fh, err := h.Open(path, 0)
	if err != nil {
		return nil, transcript.Stats{}, err
	}
	defer func() { _ = fh.Close() }()
	return h.ReadConversation(fh, 0)
}

// readAgent is a subagent's own conversation: Claude Code's sidechain
// transcript, or another harness's subagent thread, read as any of its
// conversations.
func readAgent(h Harness, path string) ([]transcript.Record, transcript.Stats, error) {
	if isClaude(h) {
		fh, err := os.Open(path)
		if err != nil {
			return nil, transcript.Stats{}, err
		}
		defer func() { _ = fh.Close() }()
		return transcript.ReadAgent(fh)
	}
	return readConversation(h, path)
}

// firstCWD is the directory a conversation started in; "" when unknown.
func firstCWD(h Harness, path string) string {
	if isClaude(h) {
		fh, err := os.Open(path)
		if err != nil {
			return ""
		}
		defer func() { _ = fh.Close() }()
		return transcript.FirstCWD(fh)
	}
	recs, _, _ := readConversation(h, path)
	for _, r := range recs {
		if r.CWD != "" {
			return r.CWD
		}
	}
	return ""
}

// EntrypointReader is a harness that tells how a conversation was started
// without reading it all (Codex: its first line).
type EntrypointReader interface {
	Entrypoint(path string) string
}

// entrypointOf is how a conversation was started; "" when unknown.
func entrypointOf(h Harness, path string) string {
	if e, ok := h.(EntrypointReader); ok && !isClaude(h) {
		return e.Entrypoint(path)
	}
	if isClaude(h) {
		fh, err := os.Open(path)
		if err != nil {
			return ""
		}
		defer func() { _ = fh.Close() }()
		return transcript.Entrypoint(fh)
	}
	recs, _, _ := readConversation(h, path)
	for _, r := range recs {
		if r.Entrypoint != "" {
			return r.Entrypoint
		}
	}
	return ""
}

// lastReply is a subagent conversation's last assistant text, when and
// where it was written (transcript.LastReply's).
func lastReply(h Harness, path string) transcript.Reply {
	if isClaude(h) {
		fh, err := os.Open(path)
		if err != nil {
			return transcript.Reply{}
		}
		defer func() { _ = fh.Close() }()
		return transcript.LastReply(fh)
	}
	recs, _, _ := readConversation(h, path)
	for i := len(recs) - 1; i >= 0; i-- {
		var texts []string
		for _, t := range recs[i].Turns {
			if t.Role == "assistant" {
				texts = append(texts, t.Content)
			}
		}
		if text := strings.TrimSpace(strings.Join(texts, "\n")); text != "" {
			return transcript.Reply{Text: text, At: recs[i].Timestamp, CWD: recs[i].CWD}
		}
	}
	return transcript.Reply{}
}

// scanConversation calls fn with the records of the conversation at path
// that may hold one of words (transcript.Scan's pass for Claude Code; every
// record with a tool call for another harness) until fn returns false.
func scanConversation(h Harness, path string, words []string, fn func(transcript.Record) bool) {
	if isClaude(h) {
		fh, err := os.Open(path)
		if err != nil {
			return
		}
		defer func() { _ = fh.Close() }()
		transcript.Scan(fh, words, fn)
		return
	}
	recs, _, _ := readConversation(h, path)
	for _, r := range recs {
		if len(r.Tools) > 0 && !fn(r) {
			return
		}
	}
}

// SubagentLister is a harness whose subagents' conversations are files
// of their own, found from their session's (Codex's subagent threads).
type SubagentLister interface {
	// Subagents are the subagents' conversations of the session whose
	// conversation is path.
	Subagents(path string) []string
	// AgentType is a subagent conversation's agent type.
	AgentType(path string) string
}

// agentFile is one subagent's own conversation.
type agentFile struct {
	id, path, agentType string
}

// subagentsOf are the subagents' conversations of session transcript
// path: Claude Code's <session>/subagents/agent-<id>.jsonl beside it, or
// what the harness lists (SubagentLister), its subagent's id being its
// conversation's session id. Ids that are not valid session ids are left
// out.
func subagentsOf(h Harness, path string) []agentFile {
	var out []agentFile
	if l, ok := h.(SubagentLister); ok && !isClaude(h) {
		for _, p := range l.Subagents(path) {
			if id := h.SessionOf(p); session.ValidID(id) {
				out = append(out, agentFile{id: id, path: p, agentType: l.AgentType(p)})
			}
		}
		return out
	}
	files, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-*.jsonl"))
	for _, p := range files {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "agent-"), ".jsonl")
		if session.ValidID(id) {
			out = append(out, agentFile{id: id, path: p, agentType: agentTypeOf(p)})
		}
	}
	return out
}

// sessionPaths maps the session id of each of transcripts to its path.
func (f *Flusher) sessionPaths(transcripts []string) map[string]string {
	paths := map[string]string{}
	for _, p := range transcripts {
		paths[f.harnessFor(p).SessionOf(p)] = p
	}
	return paths
}

// agentPathOf is subagent agent's own conversation in session transcript
// path: Claude Code's subagents/agent-<id>.jsonl beside it, or the one
// the harness lists; "" when it has none.
func agentPathOf(h Harness, path, agent string) string {
	if isClaude(h) {
		return filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-"+agent+".jsonl")
	}
	for _, a := range subagentsOf(h, path) {
		if a.id == agent {
			return a.path
		}
	}
	return ""
}

// Mover is a harness whose conversation files move in place (Codex
// compresses a rollout to the same name plus .zst): Current is where the
// conversation path names is now.
type Mover interface {
	Current(path string) string
}

// currentOf is where h's conversation at path is now: path for a harness
// whose files never move.
func currentOf(h Harness, path string) string {
	if m, ok := h.(Mover); ok {
		return m.Current(path)
	}
	return path
}

// CurrentPath is where the conversation at path is now, among hs's
// harnesses (HarnessOf).
func CurrentPath(hs []Harness, path string) string { return currentOf(HarnessOf(hs, path), path) }

// samePlace says whether paths a and b name the same conversation of h:
// equal, or one is where the other moved to.
func samePlace(h Harness, a, b string) bool {
	return a == b || currentOf(h, a) == currentOf(h, b)
}
