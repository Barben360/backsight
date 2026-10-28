package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/instructions"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/recall"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// Failure pitfall recall (SPEC §14, hook verified in E15): when a tool call
// fails, the memories about that failure are handed to Claude, once per
// error signature. Off until the user sets the [recall] failure switch.
const (
	// failureMinScore is the server (reranker) score a memory needs to be
	// injected after a failure: higher than a prompt's, since Claude did
	// not ask and the failure text is noisy.
	failureMinScore = 0.8
	failureHits     = 2
	failureBudget   = 6 * time.Second // one recall, inside the hook's 8 s deadline
	// failureMinError: shorter error text (past "Exit code N") is not a
	// failure worth a lookup; it also keeps `backsight verify` off the server.
	failureMinError = 15
	failureSigsKept = 200
	failureCmdChars = 100
)

var (
	exitCodeRE = regexp.MustCompile(`^\s*Exit code \d+\s*`)
	sigPathRE  = regexp.MustCompile(`(?:[A-Za-z]:)?[\\/][^\s:"'()\[\]]+|\b[\w.-]+/[\w./-]+`)
	sigHexRE   = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b|\b[0-9a-f]{7,}\b`)
	sigNumRE   = regexp.MustCompile(`\d+`)
	truncRE    = regexp.MustCompile(`\.\.\. \[\d+ characters truncated\] \.\.\.`)
)

func init() {
	handlers["post-tool-use-failure"] = func(ctx context.Context, _ string, input []byte) ([]byte, error) {
		h, ok := newHookEnv(ctx, input, readSide)
		if !ok || !h.cfg.Recall.FailurePitfalls {
			return nil, nil
		}
		return h.toolFailure(), nil
	}
}

// failureSignature identifies a failure across its repetitions: the tool
// and the error's first lines without paths, hex, numbers or case.
func failureSignature(tool, errText string) string {
	e := exitCodeRE.ReplaceAllString(truncRE.ReplaceAllString(errText, " "), "")
	if lines := strings.Split(e, "\n"); len(lines) > 3 {
		e = strings.Join(lines[:3], "\n")
	}
	e = sigPathRE.ReplaceAllString(e, "<p>")
	e = sigHexRE.ReplaceAllString(e, "<h>")
	e = sigNumRE.ReplaceAllString(e, "<n>")
	e = strings.ToLower(strings.Join(strings.Fields(e), " "))
	sum := sha256.Sum256([]byte(tool + "\x00" + head(e, 300)))
	return hex.EncodeToString(sum[:8])
}

// failureCommand is what the failed call was doing: a command, a path, a
// URL, or a query.
func failureCommand(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "url", "path", "query"} {
		if s, ok := in[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// toolFailure answers one failed tool call. It never waits on a lock and
// skips whenever recall would be unwelcome: paused, excluded, maintenance,
// breaker open, budget spent.
func (h *hookEnv) toolFailure() []byte {
	start := time.Now()
	tool, errText := h.in.ToolName, strings.TrimSpace(h.in.Error)
	body := strings.TrimSpace(exitCodeRE.ReplaceAllString(errText, ""))
	ev := telemetry.Raw{Name: "failure_recall", Fields: map[string]any{"tool": tool}}
	skip := func(reason string) []byte {
		ev.Fields["decision"], ev.Fields["reason"] = "skip", reason
		ev.Fields["ms"] = int(time.Since(start).Milliseconds())
		h.tw.Emit(ev)
		return nil
	}
	switch {
	case h.in.IsInterrupt:
		return skip("interrupt")
	case len([]rune(body)) < failureMinError:
		return skip("short_error")
	case strings.HasPrefix(tool, "mcp__plugin_"+plugin.Name+"_"):
		return skip("own_tool") // backsight's own tools failing is not a pitfall to look up
	}
	if paused, _ := session.Paused(h.dirs.SessionsDir(), h.in.SessionID); paused {
		return skip("paused")
	}
	if out, _ := h.resolver.OptedOut(h.in.Cwd, h.cfg.Privacy.ExcludePaths); out {
		return skip("excluded")
	}
	if h.maintenance() {
		return skip("maintenance")
	}
	if h.breakerOpen() {
		return skip("breaker")
	}
	st, _ := h.store.Load(h.in.SessionID)
	sig := failureSignature(tool, errText)
	rc := h.cfg.Recall
	switch {
	case slices.Contains(st.FailureSigs, sig):
		return skip("seen")
	case st.CharsInjected >= rc.SessionBudgetChars:
		return skip("budget")
	}

	// The query: what failed, then why. recallQuery strips code fences,
	// cuts to recall.query_chars and redacts secrets.
	q := h.recallQuery(tool + " failed: " + clip(failureCommand(h.in.ToolInput), failureCmdChars) + "\n" + body)
	var related []string
	if idx, err := recall.LoadIndex(h.dirs.EntitiesFile()); err == nil {
		related = idx.Projects(q, h.project, h.cfg.Projects.Transverse)
	}
	eng := &recall.Engine{Client: h.client, Bank: h.cfg.Server.Bank, Transverse: h.cfg.Projects.Transverse, Timeout: failureBudget, Now: now, Boost: rc.ProjectBoost}
	res := eng.Search(h.ctx, recall.Query{Queries: []string{q}, Current: h.project, Linked: h.linked, Related: related,
		MinReranker: failureMinScore, GlobalMinReranker: max(failureMinScore, rc.GlobalMinScore), Limit: 20})
	failed := len(res.Scopes) > 0
	for _, m := range res.Scopes {
		failed = failed && !m.OK
	}
	h.breakerRecord(failed)

	// At most failureHits unseen hits at the floor, within what the session
	// budget has left.
	pick := rc
	pick.PromptHits, pick.PromptChars = failureHits, min(rc.PromptChars, rc.SessionBudgetChars-st.CharsInjected)
	block, chosen := selectHits(res.Hits, pick, func(hit recall.Hit) bool {
		return hit.Reranker < failureMinScore || slices.Contains(st.Seen, hit.ID)
	})
	text := ""
	if len(chosen) > 0 {
		text = instructions.FailureHeader + strings.TrimPrefix(block, instructions.RecallHeader)
	}
	at := now()
	h.store.Retry = time.Second // parallel tool calls fail together
	h.updateSession(func(s *session.State) error {
		if failed {
			return nil // the server was unreachable: a later repeat may try again
		}
		s.FailureSigs = append(s.FailureSigs, sig)
		if len(s.FailureSigs) > failureSigsKept {
			s.FailureSigs = s.FailureSigs[len(s.FailureSigs)-failureSigsKept:]
		}
		for _, hit := range chosen {
			if !slices.Contains(s.Seen, hit.ID) {
				s.Seen = append(s.Seen, hit.ID)
			}
			s.Injected = append(s.Injected, session.Injection{ID: hit.ID, Text: hit.Text, Project: hit.Project, Score: hit.Score, Scope: hit.Scope, Prompt: s.Prompts, Rule: "failure", At: at})
		}
		s.KeepShownTexts(hitTexts(chosen))
		s.CharsInjected += len([]rune(text))
		return nil
	})
	ids := make([]string, 0, len(chosen))
	for _, hit := range chosen {
		ids = append(ids, hit.ID)
	}
	ev.Fields["decision"], ev.Fields["above"], ev.Fields["injected"], ev.Fields["ids"], ev.Fields["chars"] = "recall", len(res.Hits), len(ids), ids, len([]rune(text))
	ev.Fields["ms"] = int(time.Since(start).Milliseconds())
	h.tw.Emit(ev)
	if failed {
		return nil
	}
	h.journalShows("failure", itemsFromHits(chosen))
	return contextOutput("PostToolUseFailure", text, "")
}
