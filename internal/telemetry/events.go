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
}

// Outcome is what followed the previous prompt's injection.
type Outcome struct {
	N            int      `json:"n"`
	IDs          []string `json:"ids"`
	Searched     bool     `json:"searched"`
	Credited     *string  `json:"credited"` // exact | free | empty | null
	Contradicted bool     `json:"contradicted"`
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
func (Retain) EventName() string          { return "retain" }
func (HookFailure) EventName() string     { return "hook_failure" }
func (Job) EventName() string             { return "job" }
func (r Raw) EventName() string           { return r.Name }
