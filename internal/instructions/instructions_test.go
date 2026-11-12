package instructions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/harnessid"
	"github.com/Barben360/backsight/internal/metrics"
)

// Size limits from SPEC §6.2 and §8: hook outputs must stay far below
// Claude Code's 10 000-char cap and inside the session budget.
func TestSizes(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		max  int
	}{
		{"guide (≤ 1 200, E6; 0083 C7: what the tools do not say)", Guide, 400},
		{"short guide", ShortGuide, 700},
		{"reminder", Reminder, 400},
		{"nudge with terms", Nudge([]string{"PostgreSQL", "Grafana", "backfill", "observation scopes"}), 300},
		{"gate reason", GateReason, 400},
		{"subagent guide", SubagentGuide, 280},
		{"search description (#271: when to name another project)", SearchDescription, 1100},
		{"projects line, twelve names", ProjectsLine("backsight", []string{"hindsight-stack", "hindsight-claude-spec"}, []string{"my-homelab", "infirlib", "ski-2027", "resume", "jobouchon", "hindsight-stack-old", "photo-sorter", "garage-door", "billing-api", "web-shop", "dotfiles", "notes-app"}, 3), 520},
		{"project named line", ProjectNamed([]string{"my-homelab", "infirlib"}), 200},
		{"store description (0083 C7: ≈ 40 % under v15's 1 496)", StoreDescription, 910},
		{"credit description (0083 C7: ≈ 40 % under v14's 1 630)", CreditDescription, 980},
		{"read description", ReadDescription, 300},
		{"pointer line", Pointer([]string{"https://github.com/o/r/issues/1", "https://github.com/o/r/pull/2", "o/r#3"}), 300},
	} {
		if n := utf8.RuneCountInString(c.text); n > c.max || n == 0 {
			t.Errorf("%s: %d chars (max %d)", c.name, n, c.max)
		}
	}
}

// v7: the agent declares the memories it used with memory_credit, whose
// answer carries the line to print; no text asks for a citation format.
func TestCreditAskIsQuotedVerbatimWhereClaudeIsAskedToDeclare(t *testing.T) {
	want := "When memories were shown to you this turn, call `memory_credit` once you know what you used, before your final message: " +
		"the ids that informed it (it returns the line to print), or `verdict: none`."
	if CreditAsk != want {
		t.Errorf("credit ask changed: %q", CreditAsk)
	}
	for name, s := range map[string]string{"short guide": ShortGuide, "reminder": Reminder} {
		if !strings.Contains(s, CreditAsk) || strings.Contains(s, Credit) {
			t.Errorf("%s: want the credit ask and no citation format: %s", name, s)
		}
	}
	// 0083 C7: the MCP instructions carry it for every session; the guide,
	// beside them in the same context, does not repeat it.
	if strings.Contains(Guide, "memory_credit") || strings.Contains(Guide, Credit) {
		t.Errorf("guide repeats the credit ask: %s", Guide)
	}
	if Credit != "> 🧠 **From memory (<project>)** — <facts>" {
		t.Errorf("credit changed: %q", Credit)
	}
}

// v8: memory_credit takes the verdicts wrong and outdated, with their
// rubric and an example each.
func TestGuideV8(t *testing.T) {
	if Version != "v21" {
		t.Errorf("version %s", Version)
	}
	for _, want := range []string{
		"Use verdict wrong when this session shows the memory is false, outdated when it was true but no longer is; give the right fact as correction and what in this session shows it as evidence (the user's words, a file, a command output).",
		`"verdict": "wrong"`, `"correction":`, `"evidence":`,
	} {
		if !strings.Contains(CreditDescription, want) {
			t.Errorf("credit description lacks %q: %s", want, CreditDescription)
		}
	}
	for _, c := range []struct {
		name string
		text string
		max  int
	}{{"short guide", ShortGuide, 1000}} {
		if n := utf8.RuneCountInString(c.text); n > c.max {
			t.Errorf("%s: %d chars (max %d)", c.name, n, c.max)
		}
		for _, want := range []string{"memory_credit", "verdict: none"} {
			if !strings.Contains(c.text, want) {
				t.Errorf("%s lacks %q", c.name, want)
			}
		}
		if strings.Contains(c.text, "cite it exactly") || strings.Contains(c.text, "cite what you use") {
			t.Errorf("%s still asks for a citation: %s", c.name, c.text)
		}
	}
}

// feedback.enabled off: memory_credit is not offered, so no text names it;
// the agent cites in the earlier format instead. On: the credit ask.
func TestTextsFollowFeedback(t *testing.T) {
	on, off := For(true), For(false)
	if on.Guide != off.Guide || strings.Contains(on.Guide, "memory_credit") || strings.Contains(on.Guide, Credit) {
		t.Errorf("the guide names neither the credit nor the citation (the short guide does): %q / %q", on.Guide, off.Guide)
	}
	for name, pair := range map[string][2]string{
		"short guide": {on.ShortGuide, off.ShortGuide}, "reminder": {on.Reminder, off.Reminder},
		"recall end": {on.RecallEnd, off.RecallEnd}, "search end": {on.SearchEnd, off.SearchEnd},
	} {
		if !strings.Contains(pair[0], "memory_credit") || strings.Contains(pair[0], Credit) {
			t.Errorf("%s, feedback on: %s", name, pair[0])
		}
		if strings.Contains(pair[1], "memory_credit") || !strings.Contains(pair[1], Credit) {
			t.Errorf("%s, feedback off: %s", name, pair[1])
		}
	}
	if on.Guide != Guide || on.ShortGuide != ShortGuide || on.Reminder != Reminder || on.RecallEnd != CreditAsk {
		t.Errorf("feedback on is not the v7 set: %+v", on)
	}
	for name, c := range map[string]struct {
		text string
		max  int
	}{"guide": {off.Guide, 1200}, "short guide": {off.ShortGuide, 1000}, "reminder": {off.Reminder, 400}} {
		if n := utf8.RuneCountInString(c.text); n > c.max {
			t.Errorf("%s, feedback off: %d chars (max %d)", name, n, c.max)
		}
	}
}

// SPEC §8's guide content, now held once across the texts every session
// reads (0083 C7): the search triggers and recipes in the search tool's
// description, the credit, the empty-search line, what to store and the
// override in the MCP instructions, the conversation's rules in the guide.
// exceptionRule is the precedence's exception (v21, issue 325 point 10):
// a project's own rule wins over a conflicting user-wide one there.
const exceptionRule = "except a project's own standing convention, decision or correction, which wins there; a past event never does"

func TestGuideCoversTheSpecPoints(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"guide", Guide, []string{"issue, PR or link", "user's language", "Never ask the user for a key"}},
		{"short guide", ShortGuide, []string{CreditAsk, NothingLine, "Store decisions, corrections, conventions, pitfalls and lessons with `memory_store`",
			"User-wide rules override conflicting project facts", exceptionRule, "each tool's description says when and how to use it"}},
		{"short guide, feedback off", ShortGuideCite, []string{Credit, NothingLine, "memory_store", "override", exceptionRule}},
		{"search description", SearchDescription, []string{"BEFORE asking the user a factual question", "recommending", "starting a task or topic",
			"refers to the past", "writing a test or changing something shared", "Skip it for trivial edits", "verify"}},
	} {
		for _, want := range c.want {
			if !strings.Contains(c.text, want) {
				t.Errorf("%s lacks %q: %s", c.name, want, c.text)
			}
		}
	}
}

// shingles are the 6-word runs of s, lower-cased, punctuation dropped.
func shingles(s string) map[string]bool {
	word := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '\''
	}
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !word(r) })
	out := map[string]bool{}
	for i := 0; i+6 <= len(words); i++ {
		out[strings.Join(words[i:i+6], " ")] = true
	}
	return out
}

// 0083 C7: the guide, the MCP instructions and the tool descriptions sit
// in one context; none repeats another (v14's guide and short guide shared
// about 740 characters).
func TestStaticTextsDoNotRepeatEachOther(t *testing.T) {
	for _, feedback := range []bool{true, false} {
		x := For(feedback)
		texts := map[string]string{"guide": x.Guide, "short guide": x.ShortGuide, "search": SearchDescription, "store": StoreDescription, "read": ReadDescription}
		if feedback {
			texts["credit"] = CreditDescription
		}
		for a, ta := range texts {
			for b, tb := range texts {
				if a >= b {
					continue
				}
				sb := shingles(tb)
				for sh := range shingles(ta) {
					if sb[sh] {
						t.Errorf("feedback %v: %s and %s both say %q", feedback, a, b, sh)
					}
				}
			}
		}
	}
}

// 0083 C7: 13 % of searches looked the project's process conventions up
// again every session; the briefing page states them under "In force",
// in the same length.
func TestBriefingQueryAsksForTheProcessRules(t *testing.T) {
	_, inForce, ok := strings.Cut(BriefingQuery, "'In force'")
	inForce, _, _ = strings.Cut(inForce, ")")
	if !ok {
		t.Fatalf("no In force section: %s", BriefingQuery)
	}
	for _, want := range []string{"standing process rules", "releases", "pull requests", "worktree"} {
		if !strings.Contains(inForce, want) {
			t.Errorf("In force lacks %q: %s", want, inForce)
		}
	}
	if !strings.Contains(BriefingQuery, "In at most 1 200 characters") {
		t.Errorf("the length rule changed: %s", BriefingQuery)
	}
}

// The search recipes: topic first, intent word second; the triggers; when
// not to search; the real parameter name for related repositories.
func TestSearchDescriptionCarriesTheRecipes(t *testing.T) {
	for _, want := range []string{"topic + intent word", "never a kind word alone", "one per angle", "date or version", "pitfall / correction",
		"topic + decision", "convention / correction", "Skip it for trivial edits", "already found this session", "Hits show their kinds"} {
		if !strings.Contains(SearchDescription, want) {
			t.Errorf("search description lacks %q", want)
		}
	}
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide} {
		if strings.Contains(s, "topic + intent word") {
			t.Errorf("%s repeats the query recipe", name)
		}
	}
}

func TestStoreDescriptionCarriesTheRubric(t *testing.T) {
	// The kinds, the text's rubric (1-3 sentences, why, absolute dates) and
	// the project are the parameters' (internal/cli's static-context test).
	for _, want := range []string{"one durable fact per call", "exact identifiers", "no code dumps, no secrets", "Example:", "the memory it shows"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
}

// Close the loop: a recalled open item that is settled and a contradicted
// memory become one dated fact naming the old claim; a contradicted
// similar memory is updated. Shipped work alone is no reason to store
// (v15, decision 0083 C6). Said once, in memory_store's description (C7).
func TestStoreDescriptionClosesTheLoop(t *testing.T) {
	for _, want := range []string{"Close the loop", "when a recalled open item is settled (shipped, abandoned, root cause found)",
		"contradicted by the code or the user", "ONE dated fact", "names the old claim in its own words", "replaces open item", "kind correction",
		"updates the memory it shows (no parallel fact)"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
	if strings.Contains(StoreDescription, "when work ships") {
		t.Errorf("store description still closes the loop on any shipped work: %s", StoreDescription)
	}
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide, "guide (cite)": GuideCite, "short guide (cite)": ShortGuideCite} {
		if strings.Contains(s, "old claim") || strings.Contains(s, "when work ships") {
			t.Errorf("%s repeats the close-the-loop line: %s", name, s)
		}
	}
}

// v15 (decision 0083 C6): merges, releases and CI status are left to the
// conversation, which records them; a delivered state is stored only with
// what the conversation would not make obvious. v16 (C7): said once, in
// memory_store's description; the short guide lists what to store, without
// delivered states.
func TestStoreTextsLeaveProcessStatusToTheConversation(t *testing.T) {
	for _, want := range []string{"no merges, releases or CI status (the conversation has them)",
		"a delivered state only with a reason or location the conversation lacks"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
	for name, s := range map[string]string{"guide": Guide, "short guide": ShortGuide, "guide (cite)": GuideCite, "short guide (cite)": ShortGuideCite} {
		if strings.Contains(s, "delivered states") || strings.Contains(s, "CI status") {
			t.Errorf("%s asks to store every delivered state, or repeats the process-status rule: %s", name, s)
		}
	}
	if !strings.Contains(ShortGuide, "decisions, corrections, conventions, pitfalls and lessons") {
		t.Errorf("short guide does not list what to store: %s", ShortGuide)
	}
}

func TestStoreDescriptionForbidsMemoryAboutMemory(t *testing.T) {
	for _, want := range []string{"state the fact itself", "never \"a memory about X was stored\"", "split a fact that covers two topics"} {
		if !strings.Contains(StoreDescription, want) {
			t.Errorf("store description lacks %q", want)
		}
	}
}

// What a subagent's final report should look like: it is kept in memory.
func TestSubagentGuideShapesTheReport(t *testing.T) {
	for _, want := range []string{"memory_search", "before asking or guessing", "Your final message is stored", "findings with their cause", "decisions with their reason",
		"what was delivered where", "no progress chatter", "never say you retrieved one", "Leave storing to the main session"} {
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
	for _, cmd := range []string{"/s doctor", "/s doctor --json", "/s verify --live", "/s verify --no-round-trip", "/s stats overview --json", "/s why",
		"/s version", "/s update check", "/s report", "/s guide", `/s guide search "comment restaurer ?"`, "/s guide install#requirements",
		"/s guide troubleshooting#slow-recall-and-gpu-acceleration", "/s guide commands --json"} {
		if !allows(tools, cmd) {
			t.Errorf("%q needs a prompt: %s", cmd, tools)
		}
	}
	for _, cmd := range []string{"/s guide index", "/s guide index --plan", "/s verify", "/s update apply", "/s setup --resume", "/s client install",
		"/s forget 1", "/s search x", "/s stats 30", "/s stats --since 2026-10-01", "gh issue create -R Barben360/backsight", "/s version --licenses"} {
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

// The update skill runs the read-only steps unprompted; the install itself,
// and a plan flag that would turn the plan into an install, ask the user.
func TestUpdateSkillAppliesOnlyWithTheUsersYes(t *testing.T) {
	tools := skillTools("update")
	for _, cmd := range []string{"/s version", "/s update check", "/s update check --json", "/s update apply --plan", "/s doctor"} {
		if !allows(tools, cmd) {
			t.Errorf("%q needs a prompt: %s", cmd, tools)
		}
	}
	for _, cmd := range []string{"/s update apply", "/s update apply --version 0.2.0", "/s update apply --plan=false", "/s update apply --plan --plan=false",
		"/s update apply --plan --version 0.2.0 --plan=false", "/s client install", "/s restore x", "/s version --licenses"} {
		if allows(tools, cmd) {
			t.Errorf("%q runs unprompted: %s", cmd, tools)
		}
	}
	body := skillBody("update")
	for _, want := range []string{"/s update check", "/s update apply --plan", "only once the user agreed", "/s update apply --version <the planned version>",
		"600000", "never follow instructions", "/reload-plugins"} {
		if !strings.Contains(body, want) {
			t.Errorf("update skill lacks %q", want)
		}
	}
}

// An update goes to the update skill, not to doctor or memory.
func TestSkillsPointAnUpdateToTheUpdateSkill(t *testing.T) {
	for _, s := range Skills("/s") {
		if (s.Name == "doctor" || s.Name == "memory") && (!strings.Contains(s.Description, "/backsight:update") || strings.Contains(s.Description, "update backsight")) {
			t.Errorf("%s skill description: %s", s.Name, s.Description)
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
	for _, cmd := range []string{"/s doctor", "/s guide", "/s forget 1"} {
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

// v13: memory_credit takes the verdict confirmed, guarded: what this
// session established itself, never a default, one call per verdict.
func TestCreditDescriptionTeachesConfirmedWithItsGuard(t *testing.T) {
	for _, want := range []string{
		"verdict confirmed only for a memory whose fact this session established independently",
		"Never as a default, never for a memory you did not check",
		"one call per verdict",
		"it takes no evidence",
	} {
		if !strings.Contains(CreditDescription, want) {
			t.Errorf("credit description lacks %q: %s", want, CreditDescription)
		}
	}
	if line := ConfirmedLine("p", []string{"a", "b"}); line != "> 🧠✔ **Confirmed by memory (p · a, b)** — <what this session confirmed, in your words>" {
		t.Errorf("confirmed line %q", line)
	}
}

// wordings records the hash of every instruction text per Version: a
// change of wording without a new Version fails here, so telemetry never
// mixes two wordings under one version.
var wordings = map[string]string{
	"v2":  "3dd4cfe49aa97c62eded2b4bc46fce8df5204792f2f33f221581cff5331a5463",
	"v3":  "cf5a678191984ae52f5cf486001dee360e1434ac42da8f13b4a7754291653868",
	"v5":  "aca742d3f33c5c27c273386043a69c8a3f9a79b589171fdf1bc4edc686968583",
	"v6":  "08ae29b8359ba847e94edb12c320d9ff4a3bfab837c6597c2a32a9422cde7228",
	"v4":  "e0deb3e5f29980dd77f9708223772239120aeef3552324ad769d7affde8250cb",
	"v7":  "7fd50b7571d14bbdc98a3c43cf9aea1fea971864dfece3053510650d308a8308",
	"v8":  "12d06be831db7e934a0f7b733c2908eb1fde75d73d6f00d7a7fd645f23faa87b",
	"v9":  "a53915333ea092b317d01464304f17d79053635c57bdffca975896af0542f32c",
	"v10": "951a7ad31110632dda7081ea584084a93ecb0ede29141181d32ff0fb8f0a1295",
	"v11": "d13ec35e73d4639aaa418dd50cba48b3a683ca929001dec22ae584d3d4928f4b",
	"v12": "8e8aec126cbede943fcee5c75d31a2a9e781059316444c66255602ae4c94c9a0",
	"v13": "daf579e2e998c49a849a2c7d5f27ba20b181e5261ea1ce3aa80f026b980110bb",
	"v14": "38d36fbc00507212af4d086fc7499522dcd948cba1894be8c6c9bc9d5f6ca8ff",
	"v15": "1c8774365de706f728d5ffd77598677ea22f235b0b8654c6ded8b6c5540f69ec",
	"v16": "7d9fc362643e3a5a571090cec52a9ea4d4f2e79853d878dea8f9cd1fa2735a05",
	"v17": "ae3cdc1cacf8c2ab0ace8301af83ff728b9b6f3ff6e4c6c496fc49df2f4e0f50",
	"v18": "e3d71eb6c1dbb23e1ea4c64b3c8e60d322798803ff0f86d21c4fb762fce7b7c6",
	"v19": "c63d54bcb944357eabf2debf721fd2a25976244deecfa69066d2227ce25e77f3",
	"v20": "7289e3614a9bd8c5a2e28e387df92833e36f7b0a9538d1a06ac872cdb29c68b9",
	"v21": "87aeeade9b320deb89f6666fd28f8bdc1d632d8eb3fb0f20b7d7488eec2b9f97",
}

func wordingHash() string {
	h := sha256.New()
	texts := []string{Credit, Guide, ShortGuide, Reminder, Nudge(nil), Nudge([]string{"a", "b"}), DetectorRule, DetectorApproval, GateReason,
		RecallHeader, HintsLine, BriefingHeader("p"), BriefingModelHeader("p", "1d"), BriefingModelEnd, BriefingQuery, BriefingEmpty, BriefingRaw,
		FirstSessionNotice("s", "p"), SubagentGuide, SearchDescription, StoreDescription, ReadDescription, FailureHeader,
		CreditAsk, CreditDescription, CreditLine("p", []string{"a", "b"}), ConfirmedLine("p", []string{"a", "b"}), StopCheckHeader, StopCheckAsk,
		SavedLine("p", "k"), UpdatedLine("p", "k", "i"), StoreReplacesIt, StoreReplacesOne, RetiredLine("wrong", "p", "i"), RetiredLine("outdated", "p", "i"),
		HiddenLine("wrong", "p", "i", true), HiddenLine("outdated", "p", "i", false), NotRetiredLine("p", "i", "r", true), NotRetiredLine("p", "i", "r", false),
		NothingLine, RetroLine, Pointer([]string{"#1", "https://example.com/2"}),
		ProjectsLine("p", []string{"l"}, []string{"a", "b"}, 1), ProjectNamed([]string{"a"}), ProjectNamed([]string{"a", "b"}), UnknownProjects([]string{"x"}, []string{"a"})}
	for _, feedback := range []bool{true, false} {
		x := For(feedback)
		texts = append(texts, x.Guide, x.ShortGuide, x.Reminder, x.RecallEnd, x.SearchEnd)
	}
	for _, s := range Skills("/s") {
		// AllowedTools lists one entry per docs topic: a new docs page is
		// not a change of wording.
		texts = append(texts, s.Name, s.Description, s.ArgumentHint, s.Body)
	}
	// Codex's words, from v18 on (its skills carry no allowed tools).
	texts = append(texts, FirstSessionNoticeFor(harnessid.Codex, "s", "p"))
	for _, s := range SkillsFor(harnessid.Codex, "/s") {
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

// The stats views are read only; the index's rebuild and migrate are not,
// and no pattern may start `stats` with a flag and a wildcard: cobra strips
// the flags before it finds the subcommand.
func TestSkillsAllowOnlyReadOnlyStats(t *testing.T) {
	for _, skill := range []string{"memory", "doctor"} {
		tools := skillTools(skill)
		for _, cmd := range []string{"/s stats overview --json", "/s stats sessions --has used", "/s stats", "/s stats --json", "/s stats index --json", "/s stats index",
			"/s stats defs recall.hit_rate"} {
			if !allows(tools, cmd) {
				t.Errorf("%s: %q needs a prompt: %s", skill, cmd, tools)
			}
		}
		if got := allows(tools, "/s stats memories --text"); got != (skill == "memory") {
			t.Errorf("%s: stats memories allowed = %v (only the memory skill may run it unprompted)", skill, got)
		}
		for _, cmd := range []string{"/s stats index rebuild", "/s stats index rebuild --yes", "/s stats --days 3 index rebuild --yes", "/s stats --json index rebuild",
			"/s stats index migrate", "/s stats index --yes rebuild"} {
			if allows(tools, cmd) {
				t.Errorf("%s: %q runs unprompted: %s", skill, cmd, tools)
			}
		}
	}
}

func TestStatsViewsAreTheMetricsViews(t *testing.T) {
	if !slices.Equal(statsViews, metrics.Views) {
		t.Errorf("statsViews %v differ from the views %v: the skills would miss one", statsViews, metrics.Views)
	}
}

func TestDoctorSkillPointsToTheOverview(t *testing.T) {
	for _, s := range Skills("/s") {
		if s.Name != "doctor" {
			continue
		}
		for _, want := range []string{"stats overview --json", "verdict", "attention"} {
			if !strings.Contains(s.Body, want) {
				t.Errorf("doctor skill lacks %q", want)
			}
		}
		return
	}
	t.Fatal("no doctor skill")
}

func TestMemorySkillListsTheOverview(t *testing.T) {
	for _, s := range Skills("/s") {
		if s.Name == "memory" && !strings.Contains(s.Body, "/s stats overview") {
			t.Errorf("memory skill lacks stats overview: %s", s.Body)
		}
	}
}

// A prompt that only points at an issue or a link: read it, then search
// its content. The reminder and the guide say so; the pointer line names
// at most three references.
func TestPointerWording(t *testing.T) {
	for _, feedback := range []bool{true, false} {
		x := For(feedback)
		for name, text := range map[string]string{"guide": x.Guide, "reminder": x.Reminder} {
			if !strings.Contains(text, "issue, PR or link") || !strings.Contains(text, "read it first, then search its content") {
				t.Errorf("%s (feedback %v) does not say to read a pointed-at issue first: %s", name, feedback, text)
			}
		}
	}
	p := Pointer([]string{"#1", "#2", "#3", "https://example.com/4"})
	if !strings.Contains(p, "#1, #2, #3.") || strings.Contains(p, "example.com") || !strings.Contains(p, "Read it first") {
		t.Errorf("pointer line: %s", p)
	}
	if n := utf8.RuneCountInString(Pointer([]string{"https://github.com/Barben360/backsight/issues/144"})); n > 300 {
		t.Errorf("pointer line: %d chars (max 300)", n)
	}
}
