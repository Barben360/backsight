package telemetry

// ScopeResult is one recall scope inside a prompt event.
type ScopeResult struct {
	OK  bool `json:"ok"`
	MS  int  `json:"ms"`
	Raw int  `json:"raw"`
}

// Prompt is the UserPromptSubmit decision and what it injected.
type Prompt struct {
	N        int                    `json:"n"`
	Decision string                 `json:"decision"` // recall | skip
	Reason   string                 `json:"reason,omitempty"`
	Plen     int                    `json:"plen"`
	Lang     string                 `json:"lang,omitempty"` // fr | en (decision 0058)
	Scopes   map[string]ScopeResult `json:"scopes,omitempty"`
	Above    int                    `json:"above"`
	Injected int                    `json:"injected"`
	IDs      []string               `json:"ids"`
	Top      float64                `json:"top"`
	MS       int                    `json:"ms"`
	Nudged   bool                   `json:"nudged"`
	Hinted   bool                   `json:"hinted"`
	Detector *string                `json:"detector"` // rule | approval | null
	Chars    int                    `json:"chars"`
	// Empty: why a recall decision injected nothing (no_candidates |
	// below_floor | all_seen | capped); unset when it injected something.
	Empty string `json:"empty,omitempty"`
}

// Store is one backsight store or memory_store call: saved, updated (the
// note replaces memories) or refused. Kind is the note kind, empty when
// the call was refused before it was parsed.
type Store struct {
	Result string `json:"result"` // saved | updated | refused
	Kind   string `json:"kind"`
}

func (Store) EventName() string { return "store" }

// Outcome is what followed the previous prompt's injection.
type Outcome struct {
	N            int      `json:"n"`
	IDs          []string `json:"ids"`
	Searched     bool     `json:"searched"`
	Credited     *string  `json:"credited"` // exact | free | empty | null
	Contradicted bool     `json:"contradicted"`
	// CreditSource: tool (a memory_credit call of the main agent in the
	// turn), else citation (a credit found in the reply), else empty.
	CreditSource string `json:"credit_source,omitempty"`
}

// Reply is how a finished reply used memory (Stop): the per-turn search
// and credit rates of C12.
type Reply struct {
	N        int    `json:"n"`
	Credit   string `json:"credit"` // exact | free | empty | "" (silent)
	Searched bool   `json:"searched"`
}

type Briefing struct {
	IDs    []string `json:"ids"`
	Chars  int      `json:"chars"`
	Source string   `json:"source"` // observations | raw | empty
}

type BriefingOutcome struct {
	UsedIDs []string `json:"used_ids"`
}

type Tool struct {
	Name string `json:"name"`
	MS   int    `json:"ms"`
	Hits int    `json:"hits"`
	OK   bool   `json:"ok"`
	Live string `json:"live,omitempty"` // MCP: record | stale | none (how the server found its session)
}

// Credit is one memory_credit call: its verdict, how many ids it named
// and whether it was accepted.
type Credit struct {
	Verdict string `json:"verdict"` // used | none | wrong | outdated | invalid
	IDs     int    `json:"ids_n"`
	OK      bool   `json:"ok"`
}

// Retire is what a wrong or outdated verdict did: retired (raw facts
// invalidated), partly (some, before the server refused one), hidden
// (observations hidden on this machine) or sources (observations whose
// facts were listed first), with how many memories. A verdict that both
// invalidated and hid emits one event of each. Never an id or a text.
type Retire struct {
	Action string `json:"action"` // retired | partly | hidden | sources
	IDs    int    `json:"ids_n"`
}

// CreditCheck is the end-of-turn check of one agent's turn that showed
// memories: declared, blocked, undeclared or check_skipped, with the
// honesty flags of its declaration. AfterBlock: a declared or undeclared
// turn that an earlier check of the turn blocked.
type CreditCheck struct {
	Outcome    string   `json:"outcome"`
	Shown      int      `json:"shown"`
	Flags      []string `json:"flags,omitempty"`
	AfterBlock bool     `json:"after_block,omitempty"`
}

// FeedbackSettle is one settle of the feedback journal: the shows it
// decided by outcome, and the used credits that came too late to attach.
type FeedbackSettle struct {
	Shows   int `json:"shows"`
	Credits int `json:"credits"`
	Misses  int `json:"misses"`
	Unknown int `json:"unknown"`
	Late    int `json:"late"`
}

type Gate struct {
	N        int    `json:"n"`
	Decision string `json:"decision"` // deny | allow
	Reason   string `json:"reason"`
}

type Retain struct {
	Document string `json:"document"`
	Items    int    `json:"items"`
	Status   string `json:"status"` // ok | queued | quarantined | failed
	MS       int    `json:"ms"`
}

// Attribution is one turn's project: the one it was filed under and the
// one the path rule gives (decision 0072). On main turns the path rule
// only runs in shadow; on subagent reports it is applied, and Base is the
// project the session's directory gave.
type Attribution struct {
	Turn     string `json:"turn"`
	Subagent bool   `json:"subagent"`
	Base     string `json:"base"`
	Applied  string `json:"applied"`
	PathRule string `json:"path_rule"`
	Source   string `json:"source"`
}

type HookFailure struct {
	Hook string `json:"hook"`
	Kind string `json:"kind"` // panic | fatal | timeout | invalid_output
	MS   int    `json:"ms"`
}

type Job struct {
	Name    string         `json:"name"`
	Status  string         `json:"status"`
	MS      int            `json:"ms"`
	Details map[string]any `json:"details,omitempty"`
}

// Raw is an ad-hoc event for experiments and tools outside the A.4 list.
// The writer owns ts, v, event, session, project and binary_version: a
// field of Fields with one of those names is overwritten (set the session
// in the Writer's Common).
type Raw struct {
	Name   string
	Fields map[string]any
}

func (Prompt) EventName() string          { return "prompt" }
func (Outcome) EventName() string         { return "outcome" }
func (Reply) EventName() string           { return "reply" }
func (Briefing) EventName() string        { return "briefing" }
func (BriefingOutcome) EventName() string { return "briefing_outcome" }
func (Tool) EventName() string            { return "tool" }
func (Gate) EventName() string            { return "gate" }
func (CreditCheck) EventName() string     { return "credit_check" }
func (Credit) EventName() string          { return "credit" }
func (Retire) EventName() string          { return "retire" }
func (FeedbackSettle) EventName() string  { return "feedback_settle" }
func (Retain) EventName() string          { return "retain" }
func (HookFailure) EventName() string     { return "hook_failure" }
func (Attribution) EventName() string     { return "attribution" }
func (Job) EventName() string             { return "job" }
func (r Raw) EventName() string           { return r.Name }
