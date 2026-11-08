package retain

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/transcript"
)

// TestReplayAppendGate replays this machine's transcripts through the
// jobs tick's send rule, old (settled or long-open) and new (the append
// gate at its defaults), and counts the sends of each (decision 0083 C1).
// It reads only, and runs only when asked:
//
//	BACKSIGHT_REPLAY_PROJECTS=~/.claude/projects \
//	BACKSIGHT_REPLAY_TELEMETRY=~/.local/state/backsight/telemetry \
//	go test ./internal/retain -run TestReplayAppendGate -v
//
// One document per session (a session's projects are not split), ticks
// every 15 minutes from its first turn, sdk-* entrypoints left out. Two
// endings bound SessionEnd: never seen (ticks go on until all is sent),
// or seen a minute after the last turn (a forced send of what is left).
// With BACKSIGHT_REPLAY_TELEMETRY, the conversation sends the telemetry
// logged for the same sessions are counted beside, for the old rule.
func TestReplayAppendGate(t *testing.T) {
	root := os.Getenv("BACKSIGHT_REPLAY_PROJECTS")
	if root == "" {
		t.Skip("BACKSIGHT_REPLAY_PROJECTS not set")
	}
	files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	gate := AppendGate{MinChars: 20_000, MaxWait: 6 * time.Hour}
	if n, err := strconv.Atoi(os.Getenv("BACKSIGHT_REPLAY_MIN_CHARS")); err == nil {
		gate.MinChars = n // another threshold to compare
	}
	if d, err := time.ParseDuration(os.Getenv("BACKSIGHT_REPLAY_MAX_WAIT")); err == nil {
		gate.MaxWait = d
	}
	t.Logf("gate: %s", gate.Rule())
	var oldOpen, newOpen, oldEnd, newEnd replayTally
	var since time.Time
	sids := map[string]bool{}
	for _, file := range files {
		fh, err := os.Open(file)
		if err != nil {
			continue
		}
		recs, _, _ := transcript.Read(fh, 0)
		_ = fh.Close()
		var kept []transcript.Record
		for _, r := range recs {
			if !strings.HasPrefix(r.Entrypoint, "sdk-") && !r.Timestamp.IsZero() && len(r.Turns) > 0 {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			continue
		}
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].Timestamp.Before(kept[j].Timestamp) })
		sids[strings.TrimSuffix(filepath.Base(file), ".jsonl")] = true
		if since.IsZero() || kept[0].Timestamp.Before(since) {
			since = kept[0].Timestamp
		}
		size := make([]int, len(kept))
		for i := range kept {
			size[i] = RenderedChars(kept[i : i+1])
		}
		oldOpen.add(replaySession(kept, size, AppendGate{}, false))
		newOpen.add(replaySession(kept, size, gate, false))
		oldEnd.add(replaySession(kept, size, AppendGate{}, true))
		newEnd.add(replaySession(kept, size, gate, true))
	}
	report := func(name string, old, new replayTally) {
		cut := 0.0
		if old.appends > 0 {
			cut = 100 * float64(old.appends-new.appends) / float64(old.appends)
		}
		t.Logf("%s: %d sessions, %d first sends; appends old %d (avg %d chars) -> new %d (avg %d chars; %d by size, %d by wait, %d at the end): -%.0f %%",
			name, old.sessions, old.first, old.appends, old.avg(), new.appends, new.avg(), new.bySize, new.byWait, new.atEnd, cut)
	}
	t.Logf("%d transcripts, first turn %s", len(files), since.Format(time.DateOnly))
	report("no SessionEnd seen", oldOpen, newOpen)
	report("SessionEnd 1 min after the last turn", oldEnd, newEnd)
	if dir := os.Getenv("BACKSIGHT_REPLAY_TELEMETRY"); dir != "" {
		sends, docs := telemetrySends(t, dir, sids)
		t.Logf("telemetry, same sessions: %d conversation sends on %d documents: %d appends logged (old rule, real)", sends, docs, sends-docs)
	}
}

type replayTally struct {
	sessions, first, appends, chars int
	bySize, byWait, atEnd           int // what let each new-rule append go
}

func (x *replayTally) add(y replayTally) {
	x.sessions++
	x.first += y.first
	x.appends += y.appends
	x.chars += y.chars
	x.bySize += y.bySize
	x.byWait += y.byWait
	x.atEnd += y.atEnd
}

func (x replayTally) avg() int {
	if x.appends == 0 {
		return 0
	}
	return x.chars / x.appends
}

// replaySession runs the tick's rule over recs (sorted by time, each of
// size[i] rendered characters): its first send, appends, and the
// characters the appends carried. end: a forced send a minute after the
// last turn.
func replaySession(recs []transcript.Record, size []int, gate AppendGate, end bool) replayTally {
	const tick = 15 * time.Minute
	var out replayTally
	chars := func(from, to int) int { // RenderedChars(recs[from:to]): a newline between records
		n := to - from - 1
		for _, s := range size[from:to] {
			n += s
		}
		return n
	}
	sent := 0
	var last time.Time
	send := func(n int, at time.Time) {
		if sent == 0 {
			out.first++
		} else {
			out.appends++
			out.chars += chars(sent, sent+n)
		}
		sent += n
		last = at
	}
	lastTurn := recs[len(recs)-1].Timestamp
	stop := lastTurn.Add(gate.MaxWait + LongOpenAfter + tick)
	if end {
		stop = lastTurn.Add(time.Minute)
	}
	for now := recs[0].Timestamp.Truncate(tick).Add(tick); now.Before(stop) && sent < len(recs); now = now.Add(tick) {
		visible := sent
		for visible < len(recs) && !recs[visible].Timestamp.After(now) {
			visible++
		}
		n := settledCount(recs[sent:visible], now)
		if n == 0 {
			continue
		}
		if sent > 0 && gate.MinChars > 0 {
			c := chars(sent, sent+n)
			if gate.Holds(c, last, now) {
				continue
			}
			if c >= gate.MinChars {
				out.bySize++
			} else {
				out.byWait++
			}
		}
		send(n, now)
	}
	if end && sent < len(recs) {
		if sent > 0 && gate.MinChars > 0 {
			out.atEnd++
		}
		send(len(recs)-sent, stop)
	}
	return out
}

// telemetrySends counts the conversation sends (retain events, status ok)
// the telemetry under dir logged for the sessions sids, and their
// documents.
func telemetrySends(t *testing.T, dir string, sids map[string]bool) (sends, docs int) {
	files, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
	seen := map[string]bool{}
	for _, file := range files {
		fh, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var ev struct {
				Event, Status, Document string
			}
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "retain" || ev.Status != "ok" || !strings.HasPrefix(ev.Document, "conversation:") {
				continue
			}
			sid, _, _ := strings.Cut(strings.TrimPrefix(ev.Document, "conversation:"), "@")
			sid, _, _ = strings.Cut(sid, "#")
			if !sids[sid] {
				continue
			}
			sends++
			if !seen[ev.Document] {
				seen[ev.Document] = true
				docs++
			}
		}
		_ = fh.Close()
	}
	return sends, docs
}
