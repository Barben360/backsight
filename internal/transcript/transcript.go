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
	Timestamp time.Time
	Start     int64 // offset of the line
	End       int64 // offset after its newline
	Turns     []Turn
}

// Stats describe one read.
type Stats struct {
	Consumed int64          // offset after the last complete line
	Unknown  map[string]int // record types that are neither user nor assistant
	BadLines int            // lines that are not JSON
}

type rawRecord struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	SessionID        string          `json:"sessionId"`
	CWD              string          `json:"cwd"`
	Timestamp        time.Time       `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
}

type block struct {
	Type  string                     `json:"type"`
	Text  string                     `json:"text"`
	Name  string                     `json:"name"`
	Input map[string]json.RawMessage `json:"input"`
}

// maxLine bounds one transcript line (pasted images can be large).
const maxLine = 64 << 20

// Read normalises the complete lines of r, which starts at byte offset from
// in the transcript. A last line without newline is left for a later read.
func Read(r io.Reader, from int64) ([]Record, Stats, error) {
	st := Stats{Consumed: from, Unknown: map[string]int{}}
	br := bufio.NewReaderSize(r, 1<<16)
	var recs []Record
	off := from
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > maxLine {
			return recs, st, errors.New("transcript line larger than 64 MiB")
		}
		if errors.Is(err, io.EOF) {
			return recs, st, nil // partial or empty last line: not consumed
		}
		if err != nil {
			return recs, st, err
		}
		start := off
		off += int64(len(line))
		st.Consumed = off
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
			continue
		}
		if rr.IsMeta || rr.IsSidechain || rr.IsCompactSummary || len(rr.Message) == 0 {
			continue
		}
		turns := normalise(rr)
		if len(turns) == 0 {
			continue
		}
		recs = append(recs, Record{UUID: rr.UUID, SessionID: rr.SessionID, CWD: rr.CWD, Timestamp: rr.Timestamp,
			Start: start, End: off, Turns: turns})
	}
}

func normalise(rr rawRecord) []Turn {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rr.Message, &msg) != nil {
		return nil
	}
	prose := func(text string) []Turn {
		if rr.Type == "user" {
			text = stripTags(text)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return []Turn{{Role: rr.Type, Content: text, Timestamp: rr.Timestamp}}
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return prose(s)
	}
	var blocks []block
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var texts []string
	var actions []Turn
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			actions = append(actions, Turn{Role: "action", Content: action(b), Timestamp: rr.Timestamp})
		}
	}
	return append(prose(strings.Join(texts, "\n")), actions...)
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
