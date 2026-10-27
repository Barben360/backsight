package instructions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// Size limits from SPEC §6.2 and §8: hook outputs must stay far below
// Claude Code's 10 000-char cap and inside the session budget.
func TestSizes(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		max  int
	}{
		{"guide (E6: near 700)", Guide, 1000},
		{"short guide", ShortGuide, 1000},
		{"reminder", Reminder, 300},
		{"nudge with terms", Nudge([]string{"PostgreSQL", "Grafana", "backfill", "observation scopes"}), 300},
		{"gate reason", GateReason, 400},
		{"subagent guide", SubagentGuide, 300},
		{"search description", SearchDescription, 1000},
		{"store description", StoreDescription, 1400},
		{"read description", ReadDescription, 300},
	} {
		if n := utf8.RuneCountInString(c.text); n > c.max || n == 0 {
			t.Errorf("%s: %d chars (max %d)", c.name, n, c.max)
		}
	}
}

func TestCreditIsQuotedVerbatimWhereClaudeIsAskedToCite(t *testing.T) {
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide, "reminder": Reminder} {
		if !strings.Contains(s, Credit) {
			t.Errorf("%s lacks the exact credit format", name)
		}
	}
	if Credit != "> 🧠 **From memory (<project>)** — <facts>" {
		t.Errorf("credit changed: %q", Credit)
	}
}

func TestGuideCoversTheSpecPoints(t *testing.T) {
	for _, want := range []string{"memory_search", "1-4 short keyword queries", "verify", "nothing in memory", "memory_store",
		"transverse", "override", "user's language", "test"} {
		if !strings.Contains(Guide, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}

// The search recipes: topic first, intent word second; the triggers; when
// not to search; the real parameter name for related repositories.
func TestSearchDescriptionCarriesTheRecipes(t *testing.T) {
	for _, want := range []string{"topic + intent word", "never a kind word alone", "one per angle", "date or version", "pitfall / correction",
		"topic + decision", "convention / correction", "`projects`", "Skip it for trivial edits", "already found this session", "Hits show their kinds"} {
		if !strings.Contains(SearchDescription, want) {
			t.Errorf("search description lacks %q", want)
		}
	}
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide} {
		if !strings.Contains(s, "topic + intent word") {
			t.Errorf("%s lacks the query recipe", name)
		}
	}
}

func TestStoreDescriptionCarriesTheRubric(t *testing.T) {
	for _, want := range []string{"decision, correction, convention, pitfall, state, lesson", "one fact per call", "why", "absolute dates",
		"no secrets", "transverse", "Examples:", "Non-examples:", "3 most similar"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
}

// Close the loop: shipped work and contradicted memories become one dated
// fact naming the old claim; a contradicted similar memory is updated.
func TestStoreDescriptionClosesTheLoop(t *testing.T) {
	for _, want := range []string{"Close the loop", "PR merged, issue closed or abandoned, root cause found", "contradicted by the code or the user",
		"ONE dated fact", "names the old claim in its own words", "replaces open item", "kind correction", "updates it rather than adding a parallel fact"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide} {
		if !strings.Contains(s, "naming the old claim") {
			t.Errorf("%s lacks the close-the-loop line", name)
		}
	}
}

func TestStoreDescriptionForbidsMemoryAboutMemory(t *testing.T) {
	for _, want := range []string{"State the fact itself", "never \"a memory about X was stored\"", "split a fact that covers two topics"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
}

// What a subagent's final report should look like: it is kept in memory.
func TestSubagentGuideShapesTheReport(t *testing.T) {
	for _, want := range []string{"memory_search", "Your final message is kept", "findings with their cause", "decisions with their reason",
		"what was delivered where", "no progress chatter", "never say you retrieved a memory", "The main session stores durable decisions"} {
		if !strings.Contains(SubagentGuide, want) {
			t.Errorf("subagent guide lacks %q", want)
		}
	}
}

func TestNudgeNamesTermsOnlyWhenKnown(t *testing.T) {
	if strings.Contains(Nudge(nil), "knows about") || !strings.Contains(Nudge([]string{"Grafana"}), "knows about: Grafana") {
		t.Errorf("%q / %q", Nudge(nil), Nudge([]string{"Grafana"}))
	}
}

// allows says whether a Claude Code allowed-tools list lets cmd run
// unprompted. Each `*` matches any text, a superset of Claude Code's own
// matching, so what this refuses Claude Code refuses too.
func allows(tools, cmd string) bool {
	for _, t := range strings.Split(tools, ", ") {
		pat, ok := strings.CutPrefix(t, "Bash(")
		if !ok {
			continue
		}
		re := "^" + strings.ReplaceAll(regexp.QuoteMeta(strings.TrimSuffix(pat, ")")), `\*`, ".*") + "$"
		if regexp.MustCompile(re).MatchString(cmd) {
			return true
		}
	}
	return false
}

// The memory skill reads the docs unprompted (the topics, a topic, a
// section, a search) but never runs guide index, which writes the docs
// bank (decision 0067, block 4 review).
func skillTools(name string) string {
	for _, s := range Skills("/s") {
		if s.Name == name {
			return s.AllowedTools
		}
	}
	return ""
}

// The doctor skill diagnoses and reads the docs unprompted; anything that
// changes the install, a verify that calls the LLM, and gh ask the user.
func TestDoctorSkillRunsTheDiagnosticsOnly(t *testing.T) {
	tools := skillTools("doctor")
	for _, cmd := range []string{"/s doctor", "/s doctor --json", "/s verify --live", "/s verify --no-round-trip", "/s stats 30", "/s why",
		"/s version", "/s update check", "/s report", "/s guide", `/s guide search "comment restaurer ?"`, "/s guide install#requirements",
		"/s guide troubleshooting#slow-recall-and-gpu-acceleration", "/s guide commands --json"} {
		if !allows(tools, cmd) {
			t.Errorf("%q needs a prompt: %s", cmd, tools)
		}
	}
	for _, cmd := range []string{"/s guide index", "/s guide index --plan", "/s verify", "/s update apply", "/s setup --resume", "/s client install",
		"/s forget 1", "/s search x", "gh issue create -R Barben360/backsight", "/s version --licenses"} {
		if allows(tools, cmd) {
			t.Errorf("%q runs unprompted: %s", cmd, tools)
		}
	}
	var body string
	for _, s := range Skills("/s") {
		if s.Name == "doctor" {
			body = s.Body
		}
	}
	for _, want := range []string{"/s report", "gh issue list -R Barben360/backsight", "gh issue create -R Barben360/backsight", "only once the user confirmed"} {
		if !strings.Contains(body, want) {
			t.Errorf("doctor skill lacks %q", want)
		}
	}
}

// The memory skill is about the memory only: backsight's own commands
// moved to the doctor skill.
func TestMemorySkillRunsTheMemoryCommandsOnly(t *testing.T) {
	tools := skillTools("memory")
	for _, cmd := range []string{`/s search "deploy" "backup"`, "/s read 0b4c2a52", "/s why --last 3"} {
		if !allows(tools, cmd) {
			t.Errorf("%q needs a prompt: %s", cmd, tools)
		}
	}
	for _, cmd := range []string{"/s doctor", "/s guide", "/s stats", "/s forget 1"} {
		if allows(tools, cmd) {
			t.Errorf("%q runs unprompted: %s", cmd, tools)
		}
	}
}

func skillBody(name string) string {
	for _, s := range Skills("/s") {
		if s.Name == name {
			return s.Body
		}
	}
	return ""
}

// The retro skill used to preload a search for the four kind words, so the
// "already stored" list was arbitrary. It now preloads the project's recent
// history with read-only commands, searches by topic, and asks for the
// rejected alternatives, the open questions, the feedback on wrong
// memories, and a report-only promote step.
func TestRetroSkillSearchesByTopicFromTheRecentHistory(t *testing.T) {
	body, tools := skillBody("memory-retro"), skillTools("memory-retro")
	if strings.Contains(body, "/s search") || strings.Contains(body, `"decision" "pitfall"`) {
		t.Errorf("retro still preloads a bare-kind search:\n%s", body)
	}
	for _, cmd := range []string{"git log --oneline -15", "git branch --show-current", "true", "/s why", "/s why --last 3"} {
		if !allows(tools, cmd) {
			t.Errorf("%q needs a prompt: %s", cmd, tools)
		}
	}
	for _, cmd := range []string{"git push", "git commit -am x", "git log -p", "git reset --hard", "/s forget 1", "/s search x", "/s doctor", "rm -rf x"} {
		if allows(tools, cmd) {
			t.Errorf("%q runs unprompted: %s", cmd, tools)
		}
	}
	for _, want := range []string{"!`git log --oneline -15 || true`", "!`git branch --show-current || true`", "2-4 topics", "one `memory_search`",
		"topic + intent word", "alternatives rejected", "questions left open", "proved wrong", "/s why", "CLAUDE.md", "`.claude/rules`", "a hook",
		"never edit those files without that approval", "at most 10"} {
		if !strings.Contains(body, want) {
			t.Errorf("retro skill lacks %q", want)
		}
	}
	// The shim is one shell word even with a space in its path.
	for _, s := range Skills("/Application Support/current") {
		if s.Name == "memory-retro" && !strings.Contains(s.AllowedTools, "Bash('/Application Support/current' why*)") {
			t.Errorf("shim not quoted: %s", s.AllowedTools)
		}
	}
}

// The docs promise that Claude is told never to ask for a key in the
// conversation: the session guide, which every session gets, says so and
// names the command that takes a secret instead.
func TestGuideTellsClaudeNeverToAskForASecret(t *testing.T) {
	for _, want := range []string{"Never ask the user for a key, token or password", "`backsight secret set`", "terminal"} {
		if !strings.Contains(Guide, want) {
			t.Errorf("guide lacks %q: %s", want, Guide)
		}
	}
}

// wordings records the hash of every instruction text per Version: a
// change of wording without a new Version fails here, so telemetry never
// mixes two wordings under one version.
var wordings = map[string]string{
	"v2": "3dd4cfe49aa97c62eded2b4bc46fce8df5204792f2f33f221581cff5331a5463",
	"v3": "cf5a678191984ae52f5cf486001dee360e1434ac42da8f13b4a7754291653868",
	"v5": "aca742d3f33c5c27c273386043a69c8a3f9a79b589171fdf1bc4edc686968583",
	"v6": "4acc218d97e8f4d2e12ae5766f4e0848a452d64a518552cbd63e4584eed3da6f",
	"v4": "e0deb3e5f29980dd77f9708223772239120aeef3552324ad769d7affde8250cb",
}

func wordingHash() string {
	h := sha256.New()
	texts := []string{Credit, Guide, ShortGuide, Reminder, Nudge(nil), Nudge([]string{"a", "b"}), DetectorRule, DetectorApproval, GateReason,
		RecallHeader, HintsLine, BriefingHeader("p"), BriefingModelHeader("p", "1d"), BriefingModelEnd, BriefingQuery, BriefingEmpty, BriefingRaw,
		FirstSessionNotice("s", "p"), SubagentGuide, SearchDescription, StoreDescription, ReadDescription}
	for _, s := range Skills("/s") {
		// AllowedTools lists one entry per docs topic: a new docs page is
		// not a change of wording.
		texts = append(texts, s.Name, s.Description, s.ArgumentHint, s.Body)
	}
	for _, s := range texts {
		fmt.Fprintf(h, "%d:%s\n", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestVersionMatchesTheWording(t *testing.T) {
	want, ok := wordings[Version]
	if got := wordingHash(); !ok || got != want {
		t.Errorf("instruction texts hash to %s, recorded for %s: %q (ok %v); a wording change needs a new Version and its hash in wordings", got, Version, want, ok)
	}
}
