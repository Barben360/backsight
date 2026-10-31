package metrics

import "time"

// Tables holds the rows a view lists beside its metrics; each view fills
// only its own. A table the view does not fill (nil) is left out; one it
// fills with no rows marshals as [] (omitzero, not omitempty).
type Tables struct {
	Page             *Page              `json:"page,omitzero"`
	Activity         *Activity          `json:"activity,omitzero"`
	Lately           []SessionRow       `json:"lately,omitzero"`
	OutcomeMix       []OutcomeDay       `json:"outcome_mix,omitzero"`
	Reported         []ReportedRow      `json:"reported,omitzero"`
	Extraction       []ExtractionRow    `json:"extraction,omitzero"`
	ByProject        []ProjectCount     `json:"by_project,omitzero"`
	Review           *Review            `json:"review,omitzero"`
	FailedOperations []OperationRow     `json:"failed_operations,omitzero"`
	Jobs             []JobRow           `json:"jobs,omitzero"`
	Doctor           []DoctorRow        `json:"doctor,omitzero"`
	System           *SystemInfo        `json:"system,omitzero"`
	Projects         []ProjectRow       `json:"projects,omitzero"`
	Pooled           *PooledRow         `json:"pooled,omitzero"`
	Project          *ProjectPage       `json:"project,omitzero"`
	Sessions         []SessionRow       `json:"sessions,omitzero"`
	Session          *SessionPage       `json:"session,omitzero"`
	Memories         []MemoryRow        `json:"memories,omitzero"`
	Memory           *MemoryPage        `json:"memory,omitzero"`
	Days             []DayRow           `json:"days,omitzero"`
	Day              *DayPage           `json:"day,omitzero"`
	Releases         []ReleaseRow       `json:"releases,omitzero"`
	Settings         map[string]float64 `json:"settings,omitzero"`
	Defs             []DefRow           `json:"defs,omitzero"`
	Note             string             `json:"note,omitzero"`
}

// Page is a browse view's paging.
type Page struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// Activity counts what happened in the window (the overview's agreement counts).
type Activity struct {
	Prompts  int64            `json:"prompts"`
	Machine  int64            `json:"machine"`
	Recalls  int64            `json:"recalls"`
	WithHits int64            `json:"with_hits"`
	Credits  map[string]int64 `json:"credits"`
	Retains  map[string]int64 `json:"retains"`
}

// SessionRow is one session of the window.
type SessionRow struct {
	ID                string    `json:"id"`
	Project           string    `json:"project"`
	Start             time.Time `json:"start"`
	Last              time.Time `json:"last"`
	Prompts           int64     `json:"prompts"`
	PromptsWithMemory int64     `json:"prompts_with_memory"`
	Shown             int64     `json:"shown"`
	Used              int64     `json:"used"`
	Wrong             int64     `json:"wrong"`
	ContextChars      int64     `json:"context_chars"`
	Sent              string    `json:"sent"`
}

// OutcomeDay is one day of the show outcomes.
type OutcomeDay struct {
	Day         string                `json:"day"`
	Used        int64                 `json:"used"`
	Ignored     int64                 `json:"ignored"`
	NotDeclared int64                 `json:"not_declared"`
	ShownOnly   int64                 `json:"shown_only"`
	Pending     int64                 `json:"pending"`
	Partial     bool                  `json:"partial,omitempty"` // a day the window covers only in part
	ByPath      map[string]OutcomeDay `json:"by_path,omitempty"`
}

// ReportedRow is one wrong or outdated credit.
type ReportedRow struct {
	MemoryID string    `json:"memory_id"`
	Verdict  string    `json:"verdict"`
	Session  string    `json:"session"`
	Turn     string    `json:"turn"`
	Project  string    `json:"project"`
	At       time.Time `json:"at"`
}

// ExtractionRow is one day and operation of the server's extraction usage.
type ExtractionRow struct {
	Day       string   `json:"day"`
	Operation string   `json:"operation"`
	Input     int64    `json:"input"`
	Cached    int64    `json:"cached"`
	Output    int64    `json:"output"`
	Cost      *float64 `json:"cost"`
}

// ProjectCount is a project's memory count on the server.
type ProjectCount struct {
	Project  string `json:"project"`
	Memories int64  `json:"memories"`
}

// Review lists the memories worth a look.
type Review struct {
	NeverUsed []ReviewRow `json:"never_used"`
	Hidden    []ReviewRow `json:"hidden"`
	Reported  []ReviewRow `json:"reported"`
}

// ReviewRow is one memory of the review lists.
type ReviewRow struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Shown     int64     `json:"shown"`
	LastShown time.Time `json:"last_shown"`
	Command   string    `json:"command"`
}

// OperationRow is one failed server operation (never its error message).
type OperationRow struct {
	Type    string `json:"type"`
	AgeMS   int64  `json:"age_ms"`
	Retries int64  `json:"retries"`
}

// JobRow is one scheduled job's last run.
type JobRow struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	LastAt time.Time `json:"last_at"`
	MS     int64     `json:"ms"`
	AgeMS  int64     `json:"age_ms"`
}

// DoctorRow is one doctor check.
type DoctorRow struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Fix    string `json:"fix"`
}

// SystemInfo is the system page.
type SystemInfo struct {
	ServerVersion string     `json:"server_version"`
	Runtime       string     `json:"runtime"`
	Mode          string     `json:"mode"`
	Reach         ReachInfo  `json:"reach"`
	Backup        BackupInfo `json:"backup"`
	Update        UpdateInfo `json:"update"`
	Index         IndexInfo  `json:"index"`
}

// ReachInfo is what this PC recorded of its server's answers.
type ReachInfo struct {
	LastOK           time.Time `json:"last_ok,omitzero"`
	FailingSince     time.Time `json:"failing_since,omitzero"`
	BreakerOpenUntil time.Time `json:"breaker_open_until,omitzero"`
	BreakerFailures  int       `json:"breaker_failures"`
}

// BackupInfo is the last backup and its verification.
type BackupInfo struct {
	CreatedAt  time.Time `json:"created_at,omitzero"`
	VerifiedAt time.Time `json:"verified_at,omitzero"`
	OK         bool      `json:"ok"`
}

// UpdateInfo is the last release check.
type UpdateInfo struct {
	CheckedAt   time.Time `json:"checked_at,omitzero"`
	Newer       int       `json:"newer"`
	CheckFailed bool      `json:"check_failed"`
}

// IndexInfo is the metrics index's state.
type IndexInfo struct {
	Schema     int       `json:"schema"`
	IngestedAt time.Time `json:"ingested_at,omitzero"`
	LagSeconds int64     `json:"lag_seconds"`
	Partial    bool      `json:"partial"`
}

// ProjectRow is one project of the projects view.
type ProjectRow struct {
	Project           string    `json:"project"`
	Sessions          int64     `json:"sessions"`
	Prompts           int64     `json:"prompts"`
	PromptsWithMemory int64     `json:"prompts_with_memory"`
	UsedTurns         int64     `json:"used_turns"`
	ReportedWrong     int64     `json:"reported_wrong"`
	MemoriesOnServer  *int64    `json:"memories_on_server"`
	LastActivity      time.Time `json:"last_activity"`
	Series            []Point   `json:"series"`
}

// PooledRow totals all projects, with the per-column extremes.
type PooledRow struct {
	Label  string             `json:"label"` // "all projects pooled"
	Totals ProjectRow         `json:"totals"`
	Min    map[string]float64 `json:"min"`
	Max    map[string]float64 `json:"max"`
}

// ProjectPage is one project's page.
type ProjectPage struct {
	Project                  string       `json:"project"`
	TopUsed                  []ReviewRow  `json:"top_used"`
	NeverUsed                []ReviewRow  `json:"never_used"`
	Sessions                 []SessionRow `json:"sessions"`
	AttributionDisagreements int64        `json:"attribution_disagreements"`
	NoMemories               bool         `json:"no_memories"`
	Command                  string       `json:"command"`
}

// SessionPage is one session's page.
type SessionPage struct {
	Totals SessionRow `json:"totals"`
	Turns  []TurnRow  `json:"turns"`
}

// TurnRow is one main-agent turn of a session.
type TurnRow struct {
	N        *int64     `json:"n"`
	Turn     string     `json:"turn"`
	At       time.Time  `json:"at"`
	Decision string     `json:"decision"`
	Reason   string     `json:"reason"`
	Empty    string     `json:"empty"`
	Shows    []ShowChip `json:"shows"`
	Searches int64      `json:"searches"`
	Declared []string   `json:"declared"`
	Check    string     `json:"check"`
}

// ShowChip is one memory shown in a turn, with its outcome.
type ShowChip struct {
	MemoryID string `json:"memory_id"`
	Path     string `json:"path"`
	Outcome  string `json:"outcome"`
	Rank     *int64 `json:"rank"`
}

// MemoryRow is one memory of the feedback ledger.
type MemoryRow struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Shown     int64     `json:"shown"`
	Used      float64   `json:"used"`
	Ignored   float64   `json:"ignored"`
	Rate      *float64  `json:"rate"`
	Boost     float64   `json:"boost"`
	LastShown time.Time `json:"last_shown"`
	LastUsed  time.Time `json:"last_used"`
	State     string    `json:"state"` // hidden | wrong | never_used | ""
}

// MemoryPage is one memory's page.
type MemoryPage struct {
	Row           MemoryRow        `json:"row"`
	Kind          string           `json:"kind"`
	ServerState   string           `json:"server_state"`
	Created       time.Time        `json:"created"`
	Shows         []ShowMark       `json:"shows"`
	Retirements   []RetireMark     `json:"retirements"`
	RankHistogram map[string]int64 `json:"rank_histogram"`
	Commands      []string         `json:"commands"`
}

// ShowMark is one showing of a memory.
type ShowMark struct {
	At               time.Time `json:"at"`
	Path             string    `json:"path"`
	Outcome          string    `json:"outcome"`
	Turn             string    `json:"turn"`
	Rank             *int64    `json:"rank"`
	RankWithoutBoost *int64    `json:"rank_without_boost"`
	Score            *float64  `json:"score"`
}

// RetireMark is one retirement (hide or restore) of a memory.
type RetireMark struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
}

// DayRow is one local day of the days view.
type DayRow struct {
	Day               string `json:"day"`
	Prompts           int64  `json:"prompts"`
	Recalls           int64  `json:"recalls"`
	PromptsWithMemory int64  `json:"prompts_with_memory"`
	UsedTurns         int64  `json:"used_turns"`
	Wrong             int64  `json:"wrong"`
	ContextChars      int64  `json:"context_chars"`
	Sessions          int64  `json:"sessions"`
	SentOK            int64  `json:"sent_ok"`
	SentFailed        int64  `json:"sent_failed"`
}

// DayPage is one day's page; its tiles are Result.Metrics over that day.
type DayPage struct {
	Day      string       `json:"day"`
	Sessions []SessionRow `json:"sessions"`
}

// ReleaseRow is one binary version of the releases view.
type ReleaseRow struct {
	Version       string             `json:"version"`
	First         time.Time          `json:"first"`
	Last          time.Time          `json:"last"`
	N             int64              `json:"n"`
	Flagship      map[string]Metric  `json:"flagship"`
	ProjectShares map[string]float64 `json:"project_shares"`
	PromptsPerDay float64            `json:"prompts_per_day"`
	BankSize      *int64             `json:"bank_size"`
}

// DefRow is a catalog entry with the window's n and the smallest change
// (MDC) the data can show; the definitions table of a view.
type DefRow struct {
	Def
	N   int64    `json:"n"`
	MDC *float64 `json:"mdc"`
}
