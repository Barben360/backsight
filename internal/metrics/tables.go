package metrics

import "time"

// Tables holds the rows a view lists beside its metrics; each view fills
// only its own. A table the view does not fill (nil) is left out; one it
// fills with no rows marshals as [] (omitzero, not omitempty).
type Tables struct {
	Page             *Page               `json:"page,omitzero"`
	Activity         *Activity           `json:"activity,omitzero"`
	Lately           []SessionRow        `json:"lately,omitzero"`
	OutcomeMix       []OutcomeDay        `json:"outcome_mix,omitzero"`
	Reported         []ReportedRow       `json:"reported,omitzero"`
	Extraction       []ExtractionRow     `json:"extraction,omitzero"`
	ByProject        []ProjectCount      `json:"by_project,omitzero"`
	Review           *Review             `json:"review,omitzero"`
	TopShown         []TopShownRow       `json:"top_shown,omitzero"`
	FailedOperations []OperationRow      `json:"failed_operations,omitzero"`
	Jobs             []JobRow            `json:"jobs,omitzero"`
	NeverExtracted   []NeverExtractedRow `json:"never_extracted,omitzero"`
	Doctor           []DoctorRow         `json:"doctor,omitzero"`
	DoctorHistory    []DoctorHistoryRow  `json:"doctor_history,omitzero"`
	System           *SystemInfo         `json:"system,omitzero"`
	Projects         []ProjectRow        `json:"projects,omitzero"`
	Pooled           *PooledRow          `json:"pooled,omitzero"`
	Project          *ProjectPage        `json:"project,omitzero"`
	Sessions         []SessionRow        `json:"sessions,omitzero"`
	Session          *SessionPage        `json:"session,omitzero"`
	Memories         []MemoryRow         `json:"memories,omitzero"`
	Memory           *MemoryPage         `json:"memory,omitzero"`
	Days             []DayRow            `json:"days,omitzero"`
	Calendar         []CalendarDay       `json:"calendar,omitzero"`
	Helped           []HelpedRow         `json:"helped,omitzero"`
	Day              *DayPage            `json:"day,omitzero"`
	Releases         []ReleaseRow        `json:"releases,omitzero"`
	Markers          []Marker            `json:"markers,omitzero"`
	Settings         map[string]float64  `json:"settings,omitzero"`
	Compare          *Comparison         `json:"compare,omitzero"`
	Defs             []DefRow            `json:"defs,omitzero"`
	Note             string              `json:"note,omitzero"`
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

// TopShownRow is one of the memories shown most in the window: its project
// (that of its latest show), how often it was shown (every kind of show,
// answers from sources included, as the concentration counts them), its
// share of all the shows, and the credits of verdict used it got in the window.
type TopShownRow struct {
	Memory  string  `json:"memory"`
	Project string  `json:"project"`
	Shown   int64   `json:"shown"`
	Share   float64 `json:"share"`
	Used    int64   `json:"used"`
}

// Review lists the memories worth a look.
type Review struct {
	NeverUsed []ReviewRow `json:"never_used"`
	Hidden    []ReviewRow `json:"hidden"`
	Reported  []ReviewRow `json:"reported"`
}

// ReviewRow is one memory of the review lists.
type ReviewRow struct {
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Shown     int64      `json:"shown"`
	LastShown *time.Time `json:"last_shown"` // null when never shown
	Command   string     `json:"command"`
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

// DoctorHistoryRow is one doctor check over the window: how many days it
// read ok, watch and bad, how it reads now (its latest day with a run), its
// last change and each day (ok, watch, bad, or none for a day without a run).
type DoctorHistoryRow struct {
	Check      string        `json:"check"`
	Now        string        `json:"now"`
	OK         int64         `json:"ok"`
	Watch      int64         `json:"watch"`
	Bad        int64         `json:"bad"`
	LastChange *DoctorChange `json:"last_change"`
	Days       []DoctorDay   `json:"days"`
}

// DoctorDay is one check's status on one local day.
type DoctorDay struct {
	Day    string `json:"day"`
	Status string `json:"status"`
}

// DoctorChange is the latest day a check's status differed from the one
// before it (days without a run left out).
type DoctorChange struct {
	Day  string `json:"day"`
	From string `json:"from"`
	To   string `json:"to"`
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
	Project           string     `json:"project"`
	Sessions          int64      `json:"sessions"`
	Prompts           int64      `json:"prompts"`
	PromptsWithMemory int64      `json:"prompts_with_memory"`
	UsedTurns         int64      `json:"used_turns"`
	ReportedWrong     int64      `json:"reported_wrong"`
	MemoriesOnServer  *int64     `json:"memories_on_server"`
	LastActivity      *time.Time `json:"last_activity"` // null for the pooled row of no project
	Series            []Point    `json:"series"`
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
	ID        string     `json:"id"`
	Project   string     `json:"project"`
	Shown     int64      `json:"shown"`
	Used      float64    `json:"used"`
	Ignored   float64    `json:"ignored"`
	Rate      *float64   `json:"rate"`
	Boost     float64    `json:"boost"`
	LastShown *time.Time `json:"last_shown"` // null when never shown
	LastUsed  *time.Time `json:"last_used"`  // null when never used
	State     string     `json:"state"`      // hidden | wrong | never_used | ""
}

// MemoryPage is one memory's page.
type MemoryPage struct {
	Row           MemoryRow        `json:"row"`
	Kind          string           `json:"kind"`
	ServerState   string           `json:"server_state"`
	Created       *time.Time       `json:"created"` // null when the server did not answer
	Shows         []ShowMark       `json:"shows"`
	Retirements   []RetireMark     `json:"retirements"`
	RankHistogram map[string]int64 `json:"rank_histogram"`
	Origin        *Origin          `json:"origin"`
	Commands      []string         `json:"commands"`
}

// Origin says where a memory came from, from the record of the day lists
// (never a document id): the kind of its document and, for a session's, the
// session. The times are those of the session on this PC (null when not
// reached); Created is the day another PC's memory was made.
type Origin struct {
	State      string     `json:"state"` // this_pc | other_pc | no_session | unknown
	DocKind    string     `json:"doc_kind"`
	Session    string     `json:"session"`
	Project    string     `json:"project"`
	Ended      *time.Time `json:"ended"`
	Sent       *time.Time `json:"sent"`
	Extracted  *time.Time `json:"extracted"`
	FirstShown *time.Time `json:"first_shown"` // null when never shown
	Created    *string    `json:"created"`     // another PC's: the UTC date of the day list; else null
}

// The states of an Origin.
const (
	OriginThisPC    = "this_pc"
	OriginOtherPC   = "other_pc"
	OriginNoSession = "no_session"
	OriginUnknown   = "unknown"
)

// OriginStates are the states of an Origin, the words the views name.
var OriginStates = []string{OriginThisPC, OriginOtherPC, OriginNoSession, OriginUnknown}

// HelpedRow is one of the memories that helped most in the window: its
// project (that of its latest show), its kind (from the day lists, empty
// when not known), the credits of verdict used it got and how often it was
// shown, both in the window.
type HelpedRow struct {
	Memory   string `json:"memory"`
	Project  string `json:"project"`
	FactType string `json:"fact_type"`
	Used     int64  `json:"used"`
	Shown    int64  `json:"shown"`
}

// CalendarDay is one local day of the days view's calendar, whatever the
// window: the typed prompts, those that got memory, the turns where Claude
// used memory, the sessions that prompted, and whether anything was
// recorded that day.
type CalendarDay struct {
	Day               string `json:"day"`
	Prompts           int64  `json:"prompts"`
	PromptsWithMemory int64  `json:"prompts_with_memory"`
	UsedTurns         int64  `json:"used_turns"`
	Sessions          int64  `json:"sessions"`
	Recorded          bool   `json:"recorded"`
}

// ShowMark is one showing of a memory.
type ShowMark struct {
	At                time.Time `json:"at"`
	Path              string    `json:"path"`
	Outcome           string    `json:"outcome"`
	Turn              string    `json:"turn"`
	Session           string    `json:"session"`
	Prompt            *int64    `json:"prompt"` // the turn's prompt number in its session, null when unknown
	Rank              *int64    `json:"rank"`
	RankWithoutBoost  *int64    `json:"rank_without_boost"`
	ScoreWithoutBoost *float64  `json:"score_without_boost"`
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
	Markers  []Marker     `json:"markers"`
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

// Marker is a point on a time chart where something changed: a version seen
// for the first time after another one (Kind "release", with Version and
// Previous), or a recorded change of the settings (Kind "config", with
// Changes). At is when it was recorded.
type Marker struct {
	At       time.Time       `json:"at"`
	Kind     string          `json:"kind"`
	Version  string          `json:"version,omitzero"`
	Previous string          `json:"previous,omitzero"`
	Changes  []SettingChange `json:"changes,omitzero"`
}

// SettingChange is one setting that differs from the previous record: From
// is null for a setting that is new, To for one that is gone.
type SettingChange struct {
	Key  string   `json:"key"`
	From *float64 `json:"from"`
	To   *float64 `json:"to"`
}

// Rate is a count over a count.
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Comparison is the releases view's answer to --compare A,B: one measure
// of two versions, compared stratum by stratum (project and recall path)
// and pooled over the strata that have enough days and units on both
// sides, with an interval that treats the units of a session as one
// observation. Reading is differs, no_clear_difference or too_few; never
// better or worse. Diff (B minus A), CI, PooledA and PooledB are null when
// the reading is too_few.
type Comparison struct {
	A               string        `json:"a"`
	B               string        `json:"b"`
	Measure         string        `json:"measure"`
	Reading         string        `json:"reading"`
	Diff            *float64      `json:"diff"`
	CI              *[2]float64   `json:"ci"`
	PooledA         *float64      `json:"pooled_a"`
	PooledB         *float64      `json:"pooled_b"`
	RawA            Rate          `json:"raw_a"`
	RawB            Rate          `json:"raw_b"`
	RawDisagrees    bool          `json:"raw_disagrees"`
	Strata          []StratumRow  `json:"strata"`
	SessionsA       int           `json:"sessions_a"`
	SessionsB       int           `json:"sessions_b"`
	TooFew          *TooFew       `json:"too_few"`
	SettingsDiffer  []SettingDiff `json:"settings_differ"`
	SettingsUnknown bool          `json:"settings_unknown"`
}

// StratumRow is one (project, path) stratum of a comparison. Weight, Diff,
// CI and Reading's figures are null for a stratum that is not eligible;
// Why then names the first gate it missed (days, units or sessions).
type StratumRow struct {
	Project  string      `json:"project"`
	Path     string      `json:"path"`
	A        StratumSide `json:"a"`
	B        StratumSide `json:"b"`
	Weight   *float64    `json:"weight"`
	Diff     *float64    `json:"diff"`
	CI       *[2]float64 `json:"ci"`
	Reading  string      `json:"reading"`
	Eligible bool        `json:"eligible"`
	Why      string      `json:"why,omitzero"`
}

// StratumSide is one version's units in a stratum: the successes, the
// units and the distinct active days.
type StratumSide struct {
	Num  int64 `json:"num"`
	Den  int64 `json:"den"`
	Days int   `json:"days"`
}

// TooFew says what a comparison lacks: the version that misses the first
// gate (days, units, then sessions), what it has in the best stratum, and
// when the days gate could be met at the earliest.
type TooFew struct {
	Version    string     `json:"version"`
	Days       int        `json:"days"`
	Units      int        `json:"units"`
	Why        string     `json:"why"`
	RetryAfter *time.Time `json:"retry_after,omitzero"`
}

// SettingDiff is a numeric setting whose value in effect differs between
// the two versions; ChangedAt is the record that changed it.
type SettingDiff struct {
	Key       string     `json:"key"`
	A         *float64   `json:"a"`
	B         *float64   `json:"b"`
	ChangedAt *time.Time `json:"changed_at"`
}
