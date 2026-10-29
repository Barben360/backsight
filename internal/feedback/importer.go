package feedback

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// KindImport: shows and credits found in earlier history (telemetry and
// retained transcripts), each with its show's time, journaled through
// AppendFolded. Folded like a settle's shown and credit outcomes, and
// counted apart in Entry.Imported; never a miss. An import event with
// Outcome ImportComplete and no items marks an import run that went
// through every source: the ledger's HistoryImported.
const (
	KindImport     = "import"
	ImportComplete = "complete"
)

// ImportShow is one memory a turn of earlier history showed: Ref is what
// names it in its session (a transcript record's uuid, or a telemetry
// prompt's number), Path prompt or search.
type ImportShow struct {
	Session, Ref, Path, ID string
	Rank                   int
	At                     time.Time
	Credit                 bool
}

// Key names the show: session|ref|path|id. Ids and numbers only.
func (s ImportShow) Key() string { return s.Session + "|" + s.Ref + "|" + s.Path + "|" + s.ID }

// ImportTurn is one turn of earlier history: what it showed, in order,
// and whether its reply cited memory.
type ImportTurn struct {
	Shows []ImportShow
	Cited bool
}

// Credited is t's shows, one per key, with a credit on the last one when
// the reply cited memory and the turn showed exactly one memory: the only
// case where the memory used is known. Never a miss: older replies were
// mostly silent, and silence says nothing.
func (t ImportTurn) Credited() []ImportShow {
	var out []ImportShow
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, s := range t.Shows {
		if seen[s.Key()] {
			continue
		}
		seen[s.Key()] = true
		s.Credit = false
		out = append(out, s)
		ids[s.ID] = true
	}
	if t.Cited && len(ids) == 1 {
		out[len(out)-1].Credit = true
	}
	return out
}

// ImportEvent is the import event of one source's shows of one session:
// each item a show's key, id, rank, time and outcome (shown or credit),
// with the half-lives of p, so that every fold decays it alike.
func ImportEvent(source, session string, shows []ImportShow, p Params) Event {
	e := Event{Kind: KindImport, Session: session, Source: source, Params: p.record()}
	for _, s := range shows {
		outcome := OutcomeShown
		if s.Credit {
			outcome = OutcomeCredit
		}
		e.Settled = append(e.Settled, Settled{Key: s.Key(), ID: s.ID, At: s.At.UTC(), Outcome: outcome, Rank: s.Rank})
	}
	return e
}

// DropImports is events without the shows of their import events: the
// journal as if nothing had been imported. The marks of complete imports
// stay, so that nothing imports the same history again.
func DropImports(events []Event) []Event {
	return slices.DeleteFunc(slices.Clone(events), func(e Event) bool { return e.Kind == KindImport && e.Outcome != ImportComplete })
}

// importHitRE: a memory_search hit line of any version: its metadata in
// a last parenthesis that ends with the memory id and the score, a "#n"
// rank first since the ranks were printed. The id must be a memory id:
// a line without one is skipped.
var importHitRE = regexp.MustCompile(`^(?:- #([0-9]+) )?.*[(,] ?([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}), (-?[0-9]+\.[0-9]+)\)\r?$`)

// SearchHits is the hits of a memory_search output, each id once, with
// its rank: its "#n" when every hit has one, else its place by printed
// score, best first, a tie in print order.
func SearchHits(text string) []Item {
	type hit struct {
		id     string
		marker int
		score  float64
	}
	var hits []hit
	seen := map[string]bool{}
	marked := true
	for _, line := range strings.Split(text, "\n") {
		m := importHitRE.FindStringSubmatch(line)
		if m == nil || seen[m[2]] {
			continue
		}
		score, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		marked = marked && n > 0
		seen[m[2]] = true
		hits = append(hits, hit{m[2], n, score})
	}
	if !marked {
		slices.SortStableFunc(hits, func(a, b hit) int {
			switch {
			case a.score > b.score:
				return -1
			case a.score < b.score:
				return 1
			}
			return 0
		})
	}
	items := make([]Item, 0, len(hits))
	for i, h := range hits {
		rank := i + 1
		if marked {
			rank = h.marker
		}
		items = append(items, Item{ID: h.id, Rank: rank})
	}
	return items
}

// TelemetryEvent is the part of a telemetry line the import reads: a
// prompt's injected ids, a memory tool's returned ids, how a reply (or
// the next prompt's outcome of it) used memory.
type TelemetryEvent struct {
	TS                   time.Time
	Event, Session, Name string
	N                    int
	Reason               string   // prompt: machine for a message from Claude Code
	IDs                  []string // prompt: injected, in order
	MemoryIDs            []string // tool_use: returned, in print order
	Credit               string   // reply
	Credited             *string  // outcome
}

// citesMemory: how a reply used memory says it cited one.
func citesMemory(kind string) bool { return kind == "exact" || kind == "free" }

// TelemetryTurns is the turns of telemetry events, by session and prompt
// number: the prompt's injected ids (path prompt) and memory_search's
// returned ids (path search), ranked by position, keyed by the prompt's
// number; cited when the reply, or the next prompt's outcome of it, says
// it cited memory.
func TelemetryTurns(events []TelemetryEvent) []ImportTurn {
	type turnKey struct {
		session string
		n       int
	}
	turns := map[turnKey]*ImportTurn{}
	var order []turnKey
	turn := func(k turnKey) *ImportTurn {
		if turns[k] == nil {
			turns[k] = &ImportTurn{}
			order = append(order, k)
		}
		return turns[k]
	}
	add := func(e TelemetryEvent, path string, ids []string) {
		t := turn(turnKey{e.Session, e.N})
		for i, id := range ids {
			t.Shows = append(t.Shows, ImportShow{Session: e.Session, Ref: strconv.Itoa(e.N), Path: path, ID: id, Rank: i + 1, At: e.TS})
		}
	}
	for _, e := range events {
		if e.Session == "" {
			continue
		}
		switch {
		case e.Event == "prompt" && e.N >= 1 && e.Reason != "machine":
			add(e, "prompt", e.IDs)
		case e.Event == "tool_use" && e.N >= 0 && strings.HasSuffix(e.Name, "memory_search"):
			add(e, "search", e.MemoryIDs)
		case e.Event == "reply" && citesMemory(e.Credit), e.Event == "outcome" && e.Credited != nil && citesMemory(*e.Credited):
			turn(turnKey{e.Session, e.N}).Cited = true
		}
	}
	slices.SortStableFunc(order, func(a, b turnKey) int {
		if c := strings.Compare(a.session, b.session); c != 0 {
			return c
		}
		return a.n - b.n
	})
	out := make([]ImportTurn, 0, len(order))
	for _, k := range order {
		if len(turns[k].Shows) > 0 {
			out = append(out, *turns[k])
		}
	}
	return out
}

// maxTranscriptLine bounds one transcript line the import decodes; a
// longer one (a pasted image) is skipped.
const maxTranscriptLine = 16 << 20

// TranscriptTurns is the turns of a Claude Code transcript's main thread:
// each prompt (a user record with text) begins one; the hits of each
// memory_search result in it are its shows (path search), keyed by the
// record that holds the result, at its time. Never cited: a transcript
// does not name the memories injected with the prompt, so a turn that
// seems to show one memory may have shown more, and its credit would be
// a guess. Sidechain, meta and compaction records are skipped, and lines
// that do not parse.
func TranscriptTurns(r io.Reader) ([]ImportTurn, error) {
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Text      string          `json:"text"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	var out []ImportTurn
	var cur *ImportTurn
	end := func() {
		if cur != nil && len(cur.Shows) > 0 {
			out = append(out, *cur)
		}
		cur = nil
	}
	searches := map[string]bool{} // memory_search tool_use ids
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := readTranscriptLine(br)
		if len(line) > 0 {
			var rec struct {
				Type, UUID, SessionID, Timestamp string
				IsMeta, IsSidechain              bool
				IsCompactSummary                 bool
				Message                          struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &rec) == nil && !rec.IsMeta && !rec.IsSidechain && !rec.IsCompactSummary {
				var text string
				var blocks []block
				if json.Unmarshal(rec.Message.Content, &text) != nil {
					_ = json.Unmarshal(rec.Message.Content, &blocks)
				}
				switch rec.Type {
				case "assistant":
					for _, b := range blocks {
						if b.Type == "tool_use" && strings.HasSuffix(b.Name, "memory_search") {
							searches[b.ID] = true
						}
					}
				case "user":
					results := slices.ContainsFunc(blocks, func(b block) bool { return b.Type == "tool_result" })
					prompt := strings.TrimSpace(text) != "" || (!results && slices.ContainsFunc(blocks, func(b block) bool {
						return b.Type == "text" && strings.TrimSpace(b.Text) != ""
					}))
					if prompt {
						end()
						cur = &ImportTurn{}
					}
					at, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
					for _, b := range blocks {
						if b.Type != "tool_result" || !searches[b.ToolUseID] || cur == nil || at.IsZero() {
							continue
						}
						for _, it := range SearchHits(resultText(b.Content)) {
							cur.Shows = append(cur.Shows, ImportShow{Session: rec.SessionID, Ref: rec.UUID, Path: "search", ID: it.ID, Rank: it.Rank, At: at.UTC()})
						}
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			end()
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// resultText is a tool result's text: a string, or its text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// readTranscriptLine is the next line of br, trimmed; nil for one longer
// than maxTranscriptLine (read through and dropped).
func readTranscriptLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxTranscriptLine {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSpace(line), err
	}
}
