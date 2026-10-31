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
}

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
}

// ServerState is whether the server answered; LastOK is null when unknown.
type ServerState struct {
	OK     bool       `json:"ok"`
	Error  string     `json:"error,omitempty"`
	LastOK *time.Time `json:"last_ok"`
}

// Metric is one number with its judgement.
type Metric struct {
	Value       *float64       `json:"value"`
	Num         *int64         `json:"num"`
	Den         *int64         `json:"den"`
	Unit        string         `json:"unit"`
	Aggregation string         `json:"aggregation"`
	Status      Status         `json:"status"`
	Reason      string         `json:"reason,omitempty"`
	Borderline  bool           `json:"borderline"`
	Tags        []string       `json:"tags"` // unreliable, not_filtered, judged_28d, partial
	CI          *[2]float64    `json:"ci"`
	MDC         *float64       `json:"mdc"`
	Previous    *float64       `json:"previous"`
	Delta       *float64       `json:"delta"`
	Drill       string         `json:"drill"`
	Series      []Point        `json:"series,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"` // keys declared in the catalog
}

// Point is one day, hour or week of a series.
type Point struct {
	Day   string           `json:"day,omitempty"`
	Hour  string           `json:"hour,omitempty"`
	Week  string           `json:"week,omitempty"`
	Value *float64         `json:"value"`
	Num   *int64           `json:"num,omitempty"`
	Den   *int64           `json:"den,omitempty"`
	N     *int64           `json:"n,omitempty"`
	P50   *float64         `json:"p50,omitempty"`
	P95   *float64         `json:"p95,omitempty"`
	Parts map[string]int64 `json:"parts,omitempty"`
	// Partial: the window covers only part of this day or hour (its first
	// or last), so its counts are not a dip.
	Partial bool `json:"partial,omitempty"`
}

// Item is one entry of the attention list.
type Item struct {
	Metric   string   `json:"metric"`
	Severity Severity `json:"severity"`
	Axis     string   `json:"axis"`
	Text     string   `json:"text"`
	Why      string   `json:"why"`
	Command  string   `json:"command"`
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
