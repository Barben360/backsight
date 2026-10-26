// Package transcript reads Claude Code session transcripts (JSONL) and
// normalises them into the turns backsight retains (SPEC §9.1, decision
// 0050, npm plugin parity from research C04).
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/redact"
)

// Turn is one retained line: role user, assistant or action.
type Turn struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Record is a kept user or assistant record with its byte range in the file.
type Record struct {
	UUID      string
	SessionID string
	CWD       string
	// Entrypoint is how Claude Code was started (cli, claude-desktop,
	// sdk-cli…); empty in older transcripts.
	Entrypoint string
	Timestamp  time.Time
	Start      int64 // offset of the line
	End        int64 // offset after its newline
	Turns      []Turn
	Tools      []ToolUse // the record's tool calls, for backfill's replayed notes
}

// ToolUse is one tool call with its raw input.
type ToolUse struct {
	ID    string
	Name  string
	Input map[string]json.RawMessage
}

// Known are the record types Claude Code writes besides user and assistant
// turns, as seen in October 2026 (none carries conversation text). Any
// other type is counted as unknown: a rising count is a format change to
// look at (SPEC §14).
var Known = map[string]bool{
	"attachment": true, "api-request": true, "api-request-blob": true, "api-request-shape": true, "last-prompt": true,
	"atis-latch": true, "mode": true, "ai-title": true, "bridge-session": true, "permission-mode": true,
	"queue-operation": true, "pr-link": true, "custom-title": true, "agent-name": true, "system": true,
	"file-history-snapshot": true, "file-history-delta": true, "cost-state": true, "summary": true, "progress": true,
}

// UnknownLine is a record of a type not in Known, and the offset after it.
type UnknownLine struct {
	Type string
	End  int64
}

// UnknownBefore counts the unknown lines that end at or before offset:
// the ones a cursor at offset has passed for good.
func (s Stats) UnknownBefore(offset int64) map[string]int {
	out := map[string]int{}
	for _, u := range s.UnknownLines {
		if u.End <= offset {
			out[u.Type]++
		}
	}
	return out
}

// Stats describe one read.
type Stats struct {
	Consumed int64          // offset after the last complete line
	Unknown  map[string]int // record types that are neither user nor assistant
	// UnknownLines are the records whose type is not even Known.
	UnknownLines []UnknownLine
	BadLines     int // lines that are not JSON
}

type rawRecord struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	SessionID        string          `json:"sessionId"`
	CWD              string          `json:"cwd"`
	Entrypoint       string          `json:"entrypoint"`
	Timestamp        string          `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
}

type block struct {
	ID    string                     `json:"id"`
	Type  string                     `json:"type"`
	Text  string                     `json:"text"`
	Name  string                     `json:"name"`
	Input map[string]json.RawMessage `json:"input"`
}

// maxLine bounds one transcript line (pasted images can be large); longer
// lines are skipped without being buffered. A variable so tests can shrink it.
var maxLine = 16 << 20

// parseTime accepts RFC 3339 with or without fractions; zero when invalid.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// readLine returns the next complete line; a line longer than maxLine
// comes back as tooLong with its length, its bytes discarded.
func readLine(br *bufio.Reader) (line []byte, n int64, tooLong bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			if int64(len(line))+int64(len(chunk)) > int64(maxLine) {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, n, tooLong, err
	}
}

// Read normalises the complete lines of r, which starts at byte offset from
// in the transcript. A last line without newline is left for a later read.
func Read(r io.Reader, from int64) ([]Record, Stats, error) {
	return readRecords(r, from, false)
}

// ReadAgent is Read for a subagent's own transcript
// (subagents/agent-<id>.jsonl), whose records are all sidechain ones.
func ReadAgent(r io.Reader) ([]Record, Stats, error) {
	return readRecords(r, 0, true)
}

func readRecords(r io.Reader, from int64, sidechain bool) ([]Record, Stats, error) {
	st := Stats{Consumed: from, Unknown: map[string]int{}}
	br := bufio.NewReaderSize(r, 1<<16)
	var recs []Record
	off := from
	var prevTS time.Time
	for {
		line, n, tooLong, err := readLine(br)
		if errors.Is(err, io.EOF) {
			return recs, st, nil // partial or empty last line: not consumed
		}
		if err != nil {
			return recs, st, err
		}
		start := off
		off += n
		st.Consumed = off
		if tooLong {
			st.BadLines++
			continue
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rr rawRecord
		if json.Unmarshal(line, &rr) != nil {
			st.BadLines++
			continue
		}
		if rr.Type != "user" && rr.Type != "assistant" {
			st.Unknown[rr.Type]++
			if !Known[rr.Type] {
				st.UnknownLines = append(st.UnknownLines, UnknownLine{Type: rr.Type, End: off})
			}
			continue
		}
		if rr.IsMeta || (rr.IsSidechain && !sidechain) || rr.IsCompactSummary || len(rr.Message) == 0 {
			continue
		}
		ts := parseTime(rr.Timestamp)
		if ts.IsZero() {
			ts = prevTS // missing or malformed: the previous record's time
		}
		prevTS = ts
		turns, tools := normalise(rr, ts)
		if len(turns) == 0 {
			continue
		}
		recs = append(recs, Record{UUID: rr.UUID, SessionID: rr.SessionID, CWD: rr.CWD, Entrypoint: rr.Entrypoint, Timestamp: ts,
			Start: start, End: off, Turns: turns, Tools: tools})
	}
}

func normalise(rr rawRecord, ts time.Time) ([]Turn, []ToolUse) {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rr.Message, &msg) != nil {
		return nil, nil
	}
	prose := func(text string) []Turn {
		if rr.Type == "user" {
			text = stripTags(text)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return []Turn{{Role: rr.Type, Content: text, Timestamp: ts}}
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return prose(s), nil
	}
	var blocks []block
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil, nil
	}
	var texts []string
	var actions []Turn
	var tools []ToolUse
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			actions = append(actions, Turn{Role: "action", Content: action(b), Timestamp: ts})
			tools = append(tools, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return append(prose(strings.Join(texts, "\n")), actions...), tools
}

var targetKeys = []string{"file_path", "path", "notebook_path", "command", "pattern", "query", "url", "name", "id"}

// action is "<Tool> <target>": the first non-empty string input among
// targetKeys, first line only, at most 100 characters.
func action(b block) string {
	for _, k := range targetKeys {
		var v string
		if json.Unmarshal(b.Input[k], &v) != nil {
			continue
		}
		v, _, _ = strings.Cut(strings.TrimSpace(v), "\n")
		if v == "" {
			continue
		}
		// Redact before cutting: a token cut at the boundary would fall below
		// its pattern's minimum length and leak its prefix.
		v, _ = redact.Text(v)
		if r := []rune(v); len(r) > 100 {
			v = string(r[:100])
		}
		return b.Name + " " + v
	}
	return b.Name
}

// Harness blocks Claude Code injects into user turns.
var tagBlocks = func() []*regexp.Regexp {
	var res []*regexp.Regexp
	for _, tag := range []string{"system-reminder", "command-name", "command-message", "command-args", "command-contents",
		"local-command-stdout", "local-command-stderr", "local-command-caveat", "task-notification",
		"bash-input", "bash-stdout", "bash-stderr", "user-prompt-submit-hook"} {
		res = append(res, regexp.MustCompile(`(?s)<`+tag+`(\s[^>]*)?>.*?</`+tag+`>`))
	}
	return res
}()

func stripTags(s string) string {
	for _, re := range tagBlocks {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

// entrypointScan bounds how much of a transcript Entrypoint reads.
const entrypointScan = 1 << 20

// Entrypoint is the entrypoint of the first record of r that names one,
// within its first MiB; "" when none does.
func Entrypoint(r io.Reader) string {
	br := bufio.NewReaderSize(io.LimitReader(r, entrypointScan), 1<<16)
	for {
		line, _, tooLong, err := readLine(br)
		var rr rawRecord
		if !tooLong && json.Unmarshal(bytes.TrimSpace(line), &rr) == nil && rr.Entrypoint != "" {
			return rr.Entrypoint
		}
		if err != nil {
			return ""
		}
	}
}

// Reply is an assistant message's text, when and where it was written.
type Reply struct {
	Text string
	At   time.Time
	CWD  string
}

// LastReply is the last assistant message of r with text, sidechain
// records included: a subagent's transcript (subagents/agent-<id>.jsonl)
// is all sidechain, and its last reply is the report SubagentStop got as
// last_assistant_message. Zero when there is none.
func LastReply(r io.Reader) Reply {
	br := bufio.NewReaderSize(r, 1<<16)
	var last Reply
	for {
		line, _, tooLong, err := readLine(br)
		var rr rawRecord
		if !tooLong && json.Unmarshal(bytes.TrimSpace(line), &rr) == nil && rr.Type == "assistant" && len(rr.Message) > 0 {
			if text := replyText(rr.Message); text != "" {
				last = Reply{Text: text, At: parseTime(rr.Timestamp), CWD: rr.CWD}
			}
		}
		if err != nil {
			return last
		}
	}
}

// replyText is the text of a message: its string content, or its text
// blocks joined by newlines.
func replyText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []block
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// Scan calls fn with every user or assistant record of r, sidechain ones
// included, whose line contains one of words: a cheap pass over a whole
// transcript for a few commands (decision 0072 reads the worktrees and
// clones a session made). Lines without any of words are not decoded.
// fn returning false stops the scan.
func Scan(r io.Reader, words []string, fn func(Record) bool) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, _, tooLong, err := readLine(br)
		if !tooLong && len(line) > 0 && containsAny(line, words) {
			var rr rawRecord
			if json.Unmarshal(bytes.TrimSpace(line), &rr) == nil && (rr.Type == "user" || rr.Type == "assistant") && len(rr.Message) > 0 {
				turns, tools := normalise(rr, parseTime(rr.Timestamp))
				if !fn(Record{UUID: rr.UUID, SessionID: rr.SessionID, CWD: rr.CWD, Entrypoint: rr.Entrypoint, Timestamp: parseTime(rr.Timestamp), Turns: turns, Tools: tools}) {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func containsAny(line []byte, words []string) bool {
	for _, w := range words {
		if bytes.Contains(line, []byte(w)) {
			return true
		}
	}
	return false
}

// FirstCWD is the directory of the first main (not sidechain) user or
// assistant record of r that has one: where the session started.
func FirstCWD(r io.Reader) string {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, _, tooLong, err := readLine(br)
		var rr rawRecord
		if !tooLong && json.Unmarshal(bytes.TrimSpace(line), &rr) == nil && (rr.Type == "user" || rr.Type == "assistant") &&
			!rr.IsSidechain && rr.CWD != "" {
			return rr.CWD
		}
		if err != nil {
			return ""
		}
	}
}
