package retain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/redact"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/transcript"
)

// ReportChars caps a subagent's report (SPEC §6.2).
const ReportChars = 4000

// NotesOutbox is the outbox of notes and subagent reports.
func NotesOutbox(d paths.Dirs) Outbox {
	return Outbox{Path: filepath.Join(d.OutboxDir(), "notes.jsonl"), LockPath: filepath.Join(d.LocksDir(), "outbox.lock"),
		QuarantinePath: filepath.Join(d.QuarantineDir(), "notes.jsonl")}
}

// SubagentReport is a subagent's final report as SubagentStop keeps it:
// redacted, at most ReportChars.
func SubagentReport(text string) string {
	text, _ = redact.Text(strings.TrimSpace(text))
	if r := []rune(text); len(r) > ReportChars {
		text = string(r[:ReportChars])
	}
	return text
}

// rebuildReports queues again the reports of session sid's subagents that
// the server lacks (a restore lost them, M5 review rules), rebuilt from
// <session>/subagents/agent-<id>.jsonl beside its transcript as
// SubagentStop builds them: never one a purge deleted (its document or its
// text), one the outbox already holds, nor one the hook's rules keep out
// (a paused session or stretch, an opted-out directory, a headless parent
// session). tombs nil reads this client's. It returns the documents
// queued.
func (f *Flusher) rebuildReports(ctx context.Context, sid, transcriptPath string, tombs []state.Tombstone) ([]string, error) {
	if f.NoReports {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents", "agent-*.jsonl"))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	if tombs == nil {
		if tombs, err = f.tombstones(); err != nil {
			return nil, err
		}
	}
	ob := f.outbox()
	pending, err := ob.read()
	if err != nil {
		return nil, err
	}
	held, err := Outbox{Path: ob.QuarantinePath}.read() // refused before: retry-quarantine's
	if err != nil {
		return nil, err
	}
	pending = append(pending, held...)
	paused, _, spans := f.pauseState(sid)
	if wholePause(paused, spans) {
		return nil, nil
	}
	if f.headless(parentEntrypoint(transcriptPath)) {
		return nil, nil
	}
	var queued []string
	for _, p := range files {
		agent := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "agent-"), ".jsonl")
		if !session.ValidID(agent) {
			continue
		}
		doc := "subagent:" + sid + ":" + agent
		if slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.Document == doc }) ||
			slices.ContainsFunc(pending, func(n Note) bool { return n.DocumentID == doc }) {
			continue
		}
		_, err := f.Client.GetDocument(ctx, f.Bank, doc)
		var apiErr *hindsight.APIError
		switch {
		case err == nil:
			continue // the server holds it
		case !errors.As(err, &apiErr) || apiErr.Kind != hindsight.NotFound:
			return queued, errors.New(errSummary(err))
		}
		fh, err := os.Open(p)
		if err != nil {
			continue
		}
		reply := transcript.LastReply(fh)
		_ = fh.Close()
		text := SubagentReport(reply.Text)
		if text == "" || session.InSpans(spans, reply.At) {
			continue
		}
		if out, _ := f.Resolver.OptedOut(reply.CWD, f.Exclude); out {
			continue
		}
		h := ContentHash(text)
		if slices.ContainsFunc(tombs, func(t state.Tombstone) bool { return t.ContentSHA256 == h }) {
			continue
		}
		project := f.Resolver.Project(reply.CWD)
		if f.ReportProject != nil {
			project = f.ReportProject(reply.CWD)
		}
		n := NewSubagentNote(f.Bank, sid, agent, agentTypeOf(p), project, text, reply.At)
		if _, err := ob.Append(n); err != nil {
			return queued, err
		}
		queued = append(queued, doc)
	}
	return queued, nil
}

// outbox is f.Outbox, else the notes outbox of f.Dirs.
func (f *Flusher) outbox() Outbox {
	if f.Outbox.Path != "" {
		return f.Outbox
	}
	return NotesOutbox(f.Dirs)
}

// agentTypeOf reads the agent type of a subagent's transcript from the
// agent-<id>.meta.json Claude Code writes beside it; "" when unknown.
func agentTypeOf(transcriptPath string) string {
	raw, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var m struct {
		AgentType string `json:"agentType"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.AgentType
}

// parentEntrypoint is how session transcript path was started, read from
// its head; "" when unknown.
func parentEntrypoint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return transcript.Entrypoint(f)
}
