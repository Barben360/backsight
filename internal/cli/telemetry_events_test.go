package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/telemetry"
)

// eventsNamed is every telemetry line of the event name, in order.
func eventsNamed(t *testing.T, home, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(telemetryText(t, home), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

// lastPrompt is the newest prompt event of the session.
func lastPrompt(t *testing.T, home string) map[string]any {
	t.Helper()
	evs := eventsNamed(t, home, "prompt")
	if len(evs) == 0 {
		t.Fatal("no prompt event")
	}
	return evs[len(evs)-1]
}

func TestPromptEmptyReasons(t *testing.T) {
	cases := map[string]struct {
		answer string
		extra  string
		seen   []string
		want   string
	}{
		"no_candidates": {answer: "[]", want: "no_candidates"},
		"below_floor":   {answer: hits(0.1), want: "below_floor"},
		"all_seen":      {answer: hits(0.9), seen: []string{"m1"}, want: "all_seen"},
		"capped":        {answer: hits(0.9), extra: "[recall]\nprompt_chars = 20", want: "capped"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t)
			if c.extra != "" {
				appendConfig(t, home, c.extra)
			}
			if c.seen != nil {
				_ = sessions(home).Update("s1", func(st *session.State) error { st.Seen = c.seen; return nil })
			}
			recallAnswer = c.answer
			prompt(t, home, "s1", longPrompt)
			ev := lastPrompt(t, home)
			if ev["decision"] != "recall" || ev["injected"] != float64(0) || ev["empty"] != c.want {
				t.Errorf("event %v, want empty %q", ev, c.want)
			}
		})
	}

	t.Run("an injection and a skip carry no empty key", func(t *testing.T) {
		home := hookHome(t)
		recallAnswer = hits(0.9)
		prompt(t, home, "s1", longPrompt)
		ev := lastPrompt(t, home)
		if ev["injected"] != float64(1) {
			t.Fatalf("event %v", ev)
		}
		if _, has := ev["empty"]; has {
			t.Errorf("an injecting prompt has empty: %v", ev)
		}
		prompt(t, home, "s1", "ok") // too short: skip
		ev = lastPrompt(t, home)
		if ev["decision"] != "skip" {
			t.Fatalf("event %v", ev)
		}
		if _, has := ev["empty"]; has {
			t.Errorf("a skipped prompt has empty: %v", ev)
		}
	})
}

func TestStoreEvents(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	if code, _, _ := run(t, "store", "--kind", "decision", "--project", "billing", "x"); code != output.OK {
		t.Fatalf("exit %d", code)
	}
	if code, _, _ := run(t, "store", "--kind", "nope", "--project", "billing", "x"); code != output.Usage {
		t.Fatalf("a bad kind exits %d, want %d", code, output.Usage)
	}
	evs := eventsNamed(t, home, "store")
	if len(evs) != 2 || evs[0]["result"] != "saved" || evs[0]["kind"] != "decision" || evs[1]["result"] != "refused" || evs[1]["kind"] != "" {
		t.Fatalf("store events %v", evs)
	}

	cs := mcpConnect(t, home)
	recallAnswer = "[]"
	if _, isErr := call(t, cs, "memory_store", map[string]any{"kind": "lesson", "text": "Retry with backoff."}); isErr {
		t.Fatal("memory_store failed")
	}
	if _, isErr := call(t, cs, "memory_store", map[string]any{"kind": "idea", "text": "A fine idea."}); !isErr {
		t.Fatal("an unknown kind was stored")
	}
	evs = eventsNamed(t, home, "store")
	if len(evs) != 4 || evs[2]["result"] != "saved" || evs[2]["kind"] != "lesson" || evs[3]["result"] != "refused" || evs[3]["kind"] != "" {
		t.Fatalf("memory_store events %v", evs)
	}
}

func TestCorrectionThatReplacesIsAnUpdatedStore(t *testing.T) {
	cs, home := creditSession(t)
	showIn(t, home, "S", creditA)
	rawFact(creditA)
	out, isErr := call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa"))
	if isErr || !retiredRE.MatchString(out) {
		t.Fatalf("%v %s", isErr, out)
	}
	evs := eventsNamed(t, home, "store")
	if len(evs) != 1 || evs[0]["result"] != "updated" || evs[0]["kind"] != "correction" {
		t.Errorf("store events %v", evs)
	}
}

// breakFeedbackDir puts a file where the feedback directory goes: every
// write under it fails.
func breakFeedbackDir(t *testing.T, home string) {
	t.Helper()
	dir := pathsOf(home).FeedbackDir()
	_ = os.RemoveAll(dir)
	_ = os.MkdirAll(filepath.Dir(dir), 0o700)
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reasonsOf is the reasons of the feedback_error lines; it fails when a
// line carries an error text or anything but a reason.
func reasonsOf(t *testing.T, home string) []string {
	t.Helper()
	var reasons []string
	for _, e := range eventsNamed(t, home, "feedback_error") {
		if _, has := e["error"]; has {
			t.Errorf("a feedback_error carries an error text: %v", e)
		}
		r, _ := e["reason"].(string)
		reasons = append(reasons, r)
	}
	return reasons
}

func TestFeedbackErrorsCarryAReasonOnly(t *testing.T) {
	t.Run("hook: turn pointer and journal", func(t *testing.T) {
		home := hookHome(t)
		breakFeedbackDir(t, home)
		recallAnswer = hits(0.9)
		prompt(t, home, "s1", longPrompt)
		got := reasonsOf(t, home)
		if !slices.Contains(got, telemetry.FeedbackReasonTurnPointer) || !slices.Contains(got, telemetry.FeedbackReasonJournalWrite) {
			t.Errorf("reasons %v", got)
		}
	})
	t.Run("mcp search journal", func(t *testing.T) {
		cs, home := creditSession(t)
		breakFeedbackDir(t, home)
		recallAnswer = hits(0.9)
		call(t, cs, "memory_search", map[string]any{"queries": []string{"billing database"}})
		if got := reasonsOf(t, home); !slices.Equal(got, []string{telemetry.FeedbackReasonJournalWrite}) {
			t.Errorf("reasons %v", got)
		}
	})
	t.Run("mcp verdict journal", func(t *testing.T) {
		if os.Geteuid() == 0 || runtime.GOOS == "windows" {
			t.Skip("read-only permissions do not stop root or Windows from appending")
		}
		cs, home := creditSession(t)
		showIn(t, home, "S", creditA)
		rawFact(creditA)
		dir := pathsOf(home).FeedbackDir()
		// the journal stays readable for the shows check, but cannot be appended to
		entries, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		for _, f := range entries {
			_ = os.Chmod(f, 0o400)
		}
		_ = os.Chmod(dir, 0o500)
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		call(t, cs, "memory_credit", verdict("wrong", "aaaaaaaa"))
		if got := reasonsOf(t, home); len(got) == 0 || slices.ContainsFunc(got, func(r string) bool { return r != telemetry.FeedbackReasonJournalWrite }) {
			t.Errorf("reasons %v", got)
		}
	})
	t.Run("settle", func(t *testing.T) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		breakFeedbackDir(t, home)
		run(t, "jobs", "tick")
		if got := reasonsOf(t, home); !slices.Contains(got, telemetry.FeedbackReasonSettle) {
			t.Errorf("reasons %v", got)
		}
	})
	t.Run("shared combine", func(t *testing.T) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		dir := pathsOf(home).FeedbackDir()
		_ = os.MkdirAll(dir, 0o700)
		if err := os.WriteFile(feedback.SharedCacheFile(dir), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, "jobs", "tick")
		if got := reasonsOf(t, home); !slices.Equal(got, []string{telemetry.FeedbackReasonSharedCombine}) {
			t.Errorf("reasons %v", got)
		}
	})
}

// A note the server quarantined is a refused store, not a saved one.
func TestStoreEventOfAQuarantinedNoteIsRefused(t *testing.T) {
	got := storeEvent("decision", storeResult{Status: "rejected", Document: "note:x"}, false)
	if got.Result != "refused" || got.Kind != "decision" {
		t.Errorf("%+v", got)
	}
	if got := storeEvent("decision", storeResult{Status: "stored", Document: "note:x"}, false); got.Result != "saved" {
		t.Errorf("%+v", got)
	}
}
