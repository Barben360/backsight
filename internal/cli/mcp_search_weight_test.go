package cli

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/session"
)

// What tells Claude how to read a shortened or id-only hit follows the
// answer's own words: memory_read's description names the rank, the
// ellipsis and the words of an id-only line, and memory_credit's says its
// answer is the line to print.
func TestToolTextsDescribeTheAnswersShapes(t *testing.T) {
	read := instructions.ReadDescription
	for _, want := range []string{fmt.Sprintf("past #%d", searchWholeRanks), "(…)", searchShownBefore, "id only"} {
		if !strings.Contains(read, want) {
			t.Errorf("memory_read's description lacks %q: %s", want, read)
		}
	}
	if !strings.Contains(instructions.CreditDescription, "returns the line to print") {
		t.Errorf("memory_credit's description: %s", instructions.CreditDescription)
	}
}

// weightID is the id of hit i of weightHits: a UUID, as the hook's seen
// set reads them.
func weightID(i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012d", 0xc0ffee00+i, i) }

// weightText is hit i's text: 300 characters, its marker first and its
// tail last, so a shortened line has the one and not the other.
func weightText(i int) string {
	head := fmt.Sprintf("memory %02d ", i)
	tail := fmt.Sprintf(" TAIL%02d", i)
	return head + strings.Repeat("x", 300-len(head)-len(tail)) + tail
}

// weightHits answers n hits of project proj with long texts, hit i scoring
// 0.99 - i/100 (best first) unless scores names another score for it;
// short names the hits whose text is short.
func weightHits(n int, scores map[int]float64, short ...int) {
	var hits []string
	for i := 1; i <= n; i++ {
		text := weightText(i)
		for _, s := range short {
			if s == i {
				text = fmt.Sprintf("memory %02d short TAIL%02d", i, i)
			}
		}
		score, ok := scores[i]
		if !ok {
			score = 0.99 - float64(i)*0.01
		}
		hits = append(hits, fmt.Sprintf(`{"id":%q,"text":%q,"type":"observation","tags":["project:proj","source:session"],"mentioned_at":"2026-10-20T09:00:00Z","scores":{"reranker":%.3f}}`,
			weightID(i), text, score))
	}
	recallAnswer = "[" + strings.Join(hits, ",") + "]"
}

// hitLine is the line of rank in a memory_search answer, "" without one.
func hitLine(out string, rank int) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, fmt.Sprintf("- #%d ", rank)) {
			return l
		}
	}
	return ""
}

// whole: the line carries hit i's whole text, its date, kinds slot and
// age, as every line did before.
func whole(line string, i int) bool {
	return strings.Contains(line, weightText(i)) && strings.Contains(line, " ago, "+weightID(i)+", ")
}

// shortened: the line of rank is hit i's text cut at 120 characters, its
// date, source, id and score, and no age.
func shortened(line string, rank, i int) bool {
	text, meta, ok := strings.Cut(strings.TrimPrefix(line, fmt.Sprintf("- #%d ", rank)), " (2026-10-20")
	return ok && strings.HasPrefix(text, fmt.Sprintf("memory %02d ", i)) && strings.HasSuffix(text, "…") && utf8.RuneCountInString(text) == searchCompactChars &&
		!strings.Contains(line, "TAIL") && regexp.MustCompile(`^, from [a-z]+, `+weightID(i)+`, [0-9.]+\)$`).MatchString(meta)
}

// idOnly: the line of rank is hit i's id and score, no text.
func idOnly(line string, rank, i int) bool {
	return regexp.MustCompile(fmt.Sprintf(`^- #%d \(%s, %s, [0-9.]+\)$`, rank, regexp.QuoteMeta(searchShownBefore), weightID(i))).MatchString(line)
}

func searchQ(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "scope": "all"})
	if isErr {
		t.Fatalf("search: %s", out)
	}
	return out
}

// searchAsHook is a memory_search whose answer the PostToolUse hook of
// session sess (agent, when given: a subagent's call) then reads, as
// Claude Code runs it.
func searchAsHook(t *testing.T, cs *mcp.ClientSession, sess string, agent ...string) string {
	t.Helper()
	out := searchQ(t, cs)
	in := map[string]any{"session_id": sess, "tool_name": "mcp__plugin_backsight_memory__memory_search",
		"tool_input": map[string]any{"queries": []string{"q"}}, "tool_response": []map[string]string{{"type": "text", "text": out}}}
	if len(agent) > 0 {
		in["agent_id"] = agent[0]
	}
	fire(t, "post-tool-use", in)
	return out
}

// Ranks 1 to 8 keep their whole line; from rank 9 each hit is one line,
// its text cut at 120 characters, with its id and score: exactly 8 hits
// are all whole, a 9th is the first shortened, and a short text past 8
// stays as it is (nothing to cut).
func TestSearchPastRankEightIsOneShortLine(t *testing.T) {
	cs, _ := creditSession(t)
	weightHits(8, nil)
	out := searchQ(t, cs)
	for i := 1; i <= 8; i++ {
		if !whole(hitLine(out, i), i) {
			t.Errorf("8 hits: #%d not whole: %s", i, hitLine(out, i))
		}
	}
	weightHits(9, nil)
	out = searchQ(t, cs)
	if !whole(hitLine(out, 8), 8) || !shortened(hitLine(out, 9), 9, 9) {
		t.Errorf("9 hits: #8 whole, #9 shortened:\n%s", out)
	}
	weightHits(12, nil, 11)
	out = searchQ(t, cs)
	for i := 1; i <= 8; i++ {
		if !whole(hitLine(out, i), i) {
			t.Errorf("#%d (rank 8 or better) not whole: %s", i, hitLine(out, i))
		}
	}
	for _, i := range []int{9, 10, 12} {
		if !shortened(hitLine(out, i), i, i) {
			t.Errorf("#%d (past rank 8) not one shortened line: %s", i, hitLine(out, i))
		}
	}
	if l := hitLine(out, 11); !regexp.MustCompile(`^- #11 memory 11 short TAIL11 \(2026-10-20, from [a-z]+, ` + weightID(11) + `, [0-9.]+\)$`).MatchString(l) {
		t.Errorf("#11, a short text past 8: %s", l)
	}
	if strings.Count(out, "\n- #") != 12 || len(searchShows(out)) != 12 || len(searchTexts(out)) != 12 {
		t.Errorf("every hit keeps its line and the hook reads all 12:\n%s", out)
	}
}

// A hit already shown whole in this session comes back as its id only, at
// rank 8 or better and past it; one shown only shortened comes back whole
// once it ranks 8 or better. Ranks stay, so the hook journals every show,
// and memory_credit and memory_read take the short ids.
func TestSearchRepeatsComeBackAsIDOnly(t *testing.T) {
	cs, home := creditSession(t)
	memories[weightID(2)] = `{"id":"` + weightID(2) + `","text":"` + weightText(2) + `","type":"observation","tags":["project:proj"]}`
	weightHits(12, nil)
	searchAsHook(t, cs, "S") // 1-8 whole, 9-12 shortened
	// hit 10 (shown shortened) first, hit 2 (shown whole) last
	weightHits(12, map[int]float64{10: 0.999, 2: 0.5})
	out := searchAsHook(t, cs, "S")
	if !whole(hitLine(out, 1), 10) {
		t.Errorf("#1, shown only shortened before, must be whole: %s", hitLine(out, 1))
	}
	for rank, i := range map[int]int{2: 1, 3: 3, 8: 8, 12: 2} {
		if !idOnly(hitLine(out, rank), rank, i) {
			t.Errorf("#%d (hit %d, shown whole before) must be its id only: %s", rank, i, hitLine(out, rank))
		}
	}
	for rank, i := range map[int]int{9: 9, 10: 11, 11: 12} {
		if !shortened(hitLine(out, rank), rank, i) {
			t.Errorf("#%d (hit %d, shortened before, past 8 again) must be shortened: %s", rank, i, hitLine(out, rank))
		}
	}
	shows := eventsOf(journalEvents(t, home), "shows")
	if len(shows) != 2 || len(shows[1].Items) != 12 || shows[1].Items[1].ID != weightID(1) || shows[1].Items[1].Rank != 2 || shows[1].Items[11].ID != weightID(2) {
		t.Fatalf("the hook journals every hit of both searches, each repeat at its rank: %+v", shows)
	}
	if got, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{weightID(2)[:8], weightID(12)[:8]}}); isErr || !strings.Contains(got, weightID(2)[:8]+", "+weightID(12)[:8]) {
		t.Errorf("credit of an id-only and a shortened hit: %v %s", isErr, got)
	}
	if got, isErr := call(t, cs, "memory_read", map[string]any{"id": weightID(2)[:8]}); isErr || !strings.Contains(got, "TAIL02") {
		t.Errorf("memory_read gives the id-only hit whole: %v %s", isErr, got)
	}
}

// Only this session's main context counts: another session's shows and a
// subagent's do not make a hit id only.
func TestSearchRepeatsAreOnlyThisSessionsMainContexts(t *testing.T) {
	cs, _ := creditSession(t)
	weightHits(4, nil)
	searchAsHook(t, cs, "S2")
	if out := searchQ(t, cs); !whole(hitLine(out, 1), 1) || strings.Contains(out, searchShownBefore) {
		t.Errorf("shown in another session: must be whole here:\n%s", out)
	}
	searchAsHook(t, cs, "S", "a1")
	if out := searchQ(t, cs); !whole(hitLine(out, 1), 1) || strings.Contains(out, searchShownBefore) {
		t.Errorf("shown to a subagent only: must be whole for the main context:\n%s", out)
	}
	searchAsHook(t, cs, "S")
	if out := searchQ(t, cs); !idOnly(hitLine(out, 1), 1, 1) || !idOnly(hitLine(out, 4), 4, 4) {
		t.Errorf("shown in the main context: id only:\n%s", out)
	}
}

// A compaction keeps in the context only what it replayed (the seen set
// SessionStart compact leaves): the rest comes back whole.
func TestSearchRepeatsAfterACompaction(t *testing.T) {
	cs, home := creditSession(t)
	weightHits(4, nil)
	searchAsHook(t, cs, "S")
	_ = sessions(home).Update("S", func(st *session.State) error { st.Seen = []string{weightID(2)}; return nil })
	out := searchQ(t, cs)
	if !whole(hitLine(out, 1), 1) || !idOnly(hitLine(out, 2), 2, 2) || !whole(hitLine(out, 3), 3) {
		t.Errorf("after a compaction, only what it replayed is id only:\n%s", out)
	}
	recallAnswer = "[]"
	if out, isErr := call(t, cs, "memory_search", map[string]any{"queries": []string{"q"}, "scope": "all"}); isErr || out != "nothing in memory\n" {
		t.Errorf("empty search: %v %q", isErr, out)
	}
}

// While a subagent of the session runs, a call may be the subagent's (the
// server cannot tell whose it is): nothing comes back as id only. A
// subagent that stopped no longer counts, nor one started too long ago.
func TestSearchRepeatsWaitForTheSubagentsToStop(t *testing.T) {
	cs, home := creditSession(t)
	weightHits(4, nil)
	searchAsHook(t, cs, "S")
	fire(t, "subagent-start", map[string]any{"session_id": "S", "agent_id": "a1", "agent_type": "Explore", "cwd": filepath.Join(home, "proj")})
	if out := searchQ(t, cs); !whole(hitLine(out, 1), 1) || strings.Contains(out, searchShownBefore) {
		t.Errorf("a subagent runs: every hit whole:\n%s", out)
	}
	fire(t, "subagent-stop", map[string]any{"session_id": "S", "agent_id": "a1", "cwd": filepath.Join(home, "proj"), "last_assistant_message": "done"})
	if out := searchQ(t, cs); !idOnly(hitLine(out, 1), 1, 1) {
		t.Errorf("the subagent stopped: repeats are id only again:\n%s", out)
	}
	_ = sessions(home).Update("S", func(st *session.State) error {
		st.Subagents = map[string]time.Time{"stuck": time.Now().Add(-subagentRunningFor - time.Minute)}
		return nil
	})
	if out := searchQ(t, cs); !idOnly(hitLine(out, 1), 1, 1) {
		t.Errorf("a subagent that never stopped, started long ago, no longer counts:\n%s", out)
	}
}

// Paired by the session id only (after a /clear it names the previous
// session), the server cannot be sure whose context the call is: nothing
// comes back as id only.
func TestSearchRepeatsNeedTheCallsOwnSession(t *testing.T) {
	cs, home := creditSession(t)
	weightHits(4, nil)
	searchAsHook(t, cs, "S")
	if out := searchQ(t, cs); !idOnly(hitLine(out, 1), 1, 1) {
		t.Fatalf("own session: id only:\n%s", out)
	}
	mcpParentPID, mcpSessionID = 0, "S"
	t.Cleanup(func() { mcpSessionID = "" })
	_ = session.WriteLiveSession(pathsOf(home).SessionsDir(), session.Live{SessionID: "S", Cwd: filepath.Join(home, "proj"), Project: "proj", At: time.Now()})
	if out := searchQ(t, mcpConnect(t, home)); !whole(hitLine(out, 1), 1) || strings.Contains(out, searchShownBefore) {
		t.Errorf("paired by the session id: every hit whole:\n%s", out)
	}
}

// memory_credit's used and confirmed answers are the lines to print and
// nothing else; the hook journals them, and an older server's answers too.
func TestCreditAnswerIsTheLineToPrint(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	out, isErr := call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}})
	if isErr || out != "> 🧠 **From memory (proj · aaaaaaaa)** — <the facts you used, in your words>" {
		t.Errorf("used: %v %q", isErr, out)
	}
	out, isErr = call(t, cs, "memory_credit", map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": "confirmed"})
	if isErr || out != "> 🧠✔ **Confirmed by memory (proj · aaaaaaaa)** — <what this session confirmed, in your words>" {
		t.Errorf("confirmed: %v %q", isErr, out)
	}
	for _, c := range []struct{ verdict, text string }{
		{"used", "> 🧠 **From memory (proj · aaaaaaaa)** — x"},
		{"confirmed", "> 🧠✔ **Confirmed by memory (proj · aaaaaaaa)** — x"},
		{"used", "Credited. Print this line in your final message:\n> 🧠 **From memory (proj · aaaaaaaa)** — x"},
		{"confirmed", "Confirmed. Print this line in your final message:\n> 🧠✔ **Confirmed by memory (proj · aaaaaaaa)** — x"},
	} {
		before := len(eventsOf(journalEvents(t, home), "credit"))
		fire(t, "post-tool-use", map[string]any{"session_id": "S", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
			"tool_input": map[string]any{"ids": []string{"aaaaaaaa"}, "verdict": c.verdict}, "tool_response": []map[string]string{{"type": "text", "text": c.text}}})
		credits := eventsOf(journalEvents(t, home), "credit")
		if len(credits) != before+1 || credits[len(credits)-1].Verdict != c.verdict || strings.Join(credits[len(credits)-1].IDs, ",") != creditA {
			t.Errorf("%q not journaled as %s: %+v", c.text, c.verdict, credits)
		}
	}
	before := len(eventsOf(journalEvents(t, home), "credit"))
	fire(t, "post-tool-use", map[string]any{"session_id": "S", "tool_name": "mcp__plugin_backsight_memory__memory_credit",
		"tool_input": map[string]any{"ids": []string{"aaaaaaaa"}}, "tool_response": []map[string]string{{"type": "text", "text": "cccccccc: not shown in this session"}}})
	if n := len(eventsOf(journalEvents(t, home), "credit")); n != before {
		t.Errorf("a refusal was journaled as a credit")
	}
}
