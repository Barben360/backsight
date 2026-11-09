package metrics

import (
	"time"

	"github.com/Barben360/backsight/internal/index"
)

// ViewSchema versions the JSON of every view; blocks 4 and 5 read it.
const ViewSchema = 1

// Status is how a metric reads: no_data too_few ok watch bad info.
type Status string

// Severity is how loud an attention item or verdict reason is.
type Severity string

// AxisState is the state of one verdict axis.
type AxisState string

// The statuses, severities and axis states.
const (
	StatusNoData  Status   = "no_data"
	StatusTooFew  Status   = "too_few"
	StatusOK      Status   = "ok"
	StatusWatch   Status   = "watch"
	StatusBad     Status   = "bad"
	StatusInfo    Status   = "info"
	SeverityInfo  Severity = "info"
	SeverityWatch Severity = "watch"
	SeverityBad   Severity = "bad"

	AxisOK        AxisState = "ok"
	AxisAttention AxisState = "attention"
	AxisTooEarly  AxisState = "too_early"
	AxisCantJudge AxisState = "cant_judge"
	AxisLearning  AxisState = "learning"
)

// Result is what every view returns and `stats <view> --json` prints.
type Result struct {
	View          string            `json:"view"`
	ViewSchema    int               `json:"view_schema"`
	GeneratedAt   time.Time         `json:"generated_at"`
	Window        Window            `json:"window"`
	Previous      Span              `json:"previous"`
	Scope         Scope             `json:"scope"`
	Index         index.Freshness   `json:"index"`
	Server        *ServerState      `json:"server,omitempty"`
	Verdict       *Verdict          `json:"verdict,omitempty"`
	Summary       *Summary          `json:"summary,omitempty"`
	Metrics       map[string]Metric `json:"metrics"`
	Tables        Tables            `json:"tables"`
	Attention     []Item            `json:"attention"`
	AttentionMore int               `json:"attention_more"`
	Info          []InfoItem        `json:"info"`
	Text          *MemoryText       `json:"text,omitempty"` // memories <id> --text only

	// own is this PC's answer to the same query, set only by a run that merged (Scope.Machines "all") and
	// was asked for it (Query.WithOwn). Unexported, so no encoder emits it: the view's JSON is unchanged.
	own *Result
	// verdictFigures: the overview's figures of the verdict's own window,
	// which its axis lines quote (VerdictFigures). Unexported: the JSON is
	// unchanged.
	verdictFigures map[string]Metric
}

// VerdictFigures are the figures the verdict's axis lines quote: the
// verdict's last 7 days whatever the view's window. A result that does not
// carry them (not an overview, or one read back from JSON) gives its own
// figures.
func (r Result) VerdictFigures() map[string]Metric {
	if r.verdictFigures != nil {
		return r.verdictFigures
	}
	return r.Metrics
}

// ThisPC is this PC's own answer to the query a merged result answers: what `--machines this` returns for the
// same parameters. Nil when the result did not merge, or was not asked to carry it.
func (r Result) ThisPC() *Result { return r.own }

// Scope says what the numbers cover.
type Scope struct {
	Machines string `json:"machines"`
	Server   string `json:"server"`
	ServerOK bool   `json:"server_ok"`
	// ServerOKAt: for a view that reads nothing from the server, server_ok
	// is what this PC last recorded, and this is when it last recorded an
	// answer (null when it never did); left out when the view read the
	// server, whose answer is as of generated_at.
	ServerOKAt *time.Time `json:"server_ok_at,omitempty"`
	Project    string     `json:"project,omitempty"`
	// PCs says which PCs the figures count (every view, both modes).
	PCs *PCs `json:"pcs,omitempty"`
	// OthersUnfiltered: machines "all" under a project filter with at least
	// one other PC, whose whole figures are added (they send no project).
	OthersUnfiltered bool `json:"others_unfiltered,omitempty"`
}

// PCs is the scope of the PCs: who is counted, who went quiet, what was
// ignored. Count is N of "all PCs on this server (N)": this PC when it
// shares and is not a server-only host, plus every counted other PC.
type PCs struct {
	Sharing   bool       `json:"sharing"`
	Count     int        `json:"count"`
	Active    int        `json:"active"`
	Stale     int        `json:"stale"`
	Ignored   int        `json:"ignored"`
	FetchedAt *time.Time `json:"fetched_at"`
	Until     *string    `json:"until"`
	List      []PCRow    `json:"list"`
}

// PCRow is one counted PC; Machine is its short id (8 characters, 13 when
// two counted PCs share their first 8).
type PCRow struct {
	Machine   string  `json:"machine"`
	This      bool    `json:"this"`
	OS        string  `json:"os"`
	FirstSeen string  `json:"first_seen"`
	Until     *string `json:"until"`
	// SentAt is when the PC's document was sent (UTC); null for this PC,
	// whose figures are live.
	SentAt *time.Time `json:"sent_at"`
	Stale  bool       `json:"stale"`
	// Active: the PC has a prompt in the window.
	Active bool `json:"active"`
}

// ServerState is whether the server answered; LastOK is null when unknown.
type ServerState struct {
	OK     bool       `json:"ok"`
	Error  string     `json:"error,omitempty"`
	LastOK *time.Time `json:"last_ok"`
}

// Metric is one number with its judgement. Its tags: unreliable,
// not_filtered, judged_28d, partial, old_scan, and on all PCs (--machines
// all): others_not_filtered (a project filter: the other PCs' whole figures
// are added), series_this_pc (a window drawn by the hour: the totals add the
// other PCs' days, the series stay this PC's) and status_this_pc
// (pipeline.flow and context.per_session: the status is this PC's rule),
// average (context.per_session) and approximate (recall.latency: a range
// from the merged buckets).
type Metric struct {
	Value       *float64       `json:"value"`
	Num         *int64         `json:"num"`
	Den         *int64         `json:"den"`
	Unit        string         `json:"unit"`
	Aggregation string         `json:"aggregation"`
	Status      Status         `json:"status"`
	Reason      string         `json:"reason,omitempty"`
	Borderline  bool           `json:"borderline"`
	Tags        []string       `json:"tags"` // see Metric
	CI          *[2]float64    `json:"ci"`
	MDC         *float64       `json:"mdc"`
	Previous    *float64       `json:"previous"`
	Delta       *float64       `json:"delta"`
	Drill       string         `json:"drill"`
	Series      []Point        `json:"series,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"` // keys declared in the catalog

	// chars is this PC's characters added in the window (context.per_session)
	// and buckets its samples by latency bucket (a latency metric): what a
	// merge over several PCs adds to. Never in the JSON.
	chars   int64
	buckets [9]int64
}

// Point is one day, hour or week of a series.
type Point struct {
	Day   string   `json:"day,omitempty"`
	Hour  string   `json:"hour,omitempty"`
	Week  string   `json:"week,omitempty"`
	Value *float64 `json:"value"`
	Num   *int64   `json:"num,omitempty"`
	Den   *int64   `json:"den,omitempty"`
	N     *int64   `json:"n,omitempty"`
	P50   *float64 `json:"p50,omitempty"`
	P95   *float64 `json:"p95,omitempty"`
	// Lo and Hi: the day's p95 bucket of the PCs together, (Lo, Hi] in
	// milliseconds (Hi left out for the open bucket), on all PCs; ThisP95:
	// this PC's exact p95 of the day, then.
	Lo      *float64         `json:"lo,omitempty"`
	Hi      *float64         `json:"hi,omitempty"`
	ThisP95 *float64         `json:"this_p95,omitempty"`
	Parts   map[string]int64 `json:"parts,omitempty"`
	// Source: where a day's or week's server figures came from: "server"
	// (its live series), "record" (the days this PC recorded) or "none".
	Source string `json:"source,omitempty"`
	// Complete: a weekly cohort whose 28 days after its last day have
	// passed. Tokens and TokensPerUsed: the server's extraction tokens of
	// the week and per useful memory (yield).
	Complete      *bool    `json:"complete,omitempty"`
	Tokens        *int64   `json:"tokens,omitempty"`
	TokensPerUsed *float64 `json:"tokens_per_used,omitempty"`
	// CI: the 95 % Wilson interval of a weekly share, set with its value.
	CI *[2]float64 `json:"ci,omitempty"`
	// Partial: the window covers only part of this day or hour (its first
	// or last), so its counts are not a dip.
	Partial bool `json:"partial,omitempty"`

	// buckets: this PC's samples of the point by latency bucket, never in
	// the JSON.
	buckets [9]int64
}

// Item is one entry of the attention list.
type Item struct {
	Metric   string   `json:"metric"`
	Severity Severity `json:"severity"`
	Axis     string   `json:"axis"`
	Text     string   `json:"text"`
	Why      string   `json:"why"`
	Command  string   `json:"command"`

	// reason, when set, is the banner's wording of the item (its Text is the
	// list's).
	reason string
}

// bannerText is the item's wording on a verdict axis's banner.
func (it Item) bannerText() string {
	if it.reason != "" {
		return it.reason
	}
	return it.Text
}

// InfoItem is one counted, non-alarming line.
type InfoItem struct {
	Metric  string `json:"metric"`
	Count   int64  `json:"count"`
	Command string `json:"command"`
}

// MemoryText is a memory's text, only for `memories <id> --text`.
type MemoryText struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Reason is why the text could not be read (a no_data reason: the
	// server did not answer, has no such memory, or none is set up); ""
	// when Text holds it.
	Reason string `json:"reason,omitempty"`
}

// Verdict is the four-axis reading: found, right, cost, flowing.
type Verdict struct {
	WindowDays int             `json:"window_days"`
	Learning   bool            `json:"learning"`
	Axes       map[string]Axis `json:"axes"`
}

// Axis is one axis of the verdict.
type Axis struct {
	State      AxisState `json:"state"`
	WindowDays int       `json:"window_days"`
	Reasons    []Reason  `json:"reasons"`
}

// Reason says why an axis reads as it does.
type Reason struct {
	Metric   string   `json:"metric"`
	Severity Severity `json:"severity"`
	Text     string   `json:"text"`
	Command  string   `json:"command,omitempty"`
}

// Summary is the overview's four-line summary.
type Summary struct {
	PromptsWithMemory   int64    `json:"prompts_with_memory"`
	TurnsWithMemoryUsed int64    `json:"turns_with_memory_used"`
	ContextChars        int64    `json:"context_chars"`
	TokensApprox        int64    `json:"tokens_approx"`
	ExtractionCost      *float64 `json:"extraction_cost"`
	Currency            string   `json:"currency"`
	PriceSet            bool     `json:"price_set"`
	ExtractionScope     string   `json:"extraction_scope"` // "server"
}
