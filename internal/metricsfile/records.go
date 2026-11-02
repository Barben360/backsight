package metricsfile

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Barben360/backsight/internal/session"
)

// Schema is the common `schema` field of every line.
const (
	Schema     = 1
	KeepMonths = 13
)

// The families, one file prefix each (<family>-YYYY-MM.jsonl).
const (
	FamilyServerDaily   = "server-daily"
	FamilyDoctorHistory = "doctor-history"
	FamilyMemoryDocs    = "memory-docs"
	FamilyScanHistory   = "scan-history"
)

// Families lists every family, in a fixed order.
var Families = []string{FamilyServerDaily, FamilyDoctorHistory, FamilyMemoryDocs, FamilyScanHistory}

// Record is one line's content. Records hold numbers, bools, times, closed
// enums and ids: never a text (memory, prompt, error, path, config string).
type Record interface {
	Family() string
	Kind() string
}

// Snapshot is a server-daily/snapshot line. A count the server did not give
// is nil (omitted), never 0.
type Snapshot struct {
	Day                  string           `json:"day"` // local date, YYYY-MM-DD
	OK                   bool             `json:"ok"`
	Error                string           `json:"error,omitempty"` // enum, see snapshotErrors
	World                *int64           `json:"world,omitempty"`
	Experience           *int64           `json:"experience,omitempty"`
	Observation          *int64           `json:"observation,omitempty"`
	Documents            *int64           `json:"documents,omitempty"`
	Links                *int64           `json:"links,omitempty"`
	Observations         *int64           `json:"observations,omitempty"`
	Ops                  map[string]int64 `json:"ops,omitempty"`
	ConsolidationPending *int64           `json:"consolidation_pending,omitempty"`
	ConsolidationFailed  *int64           `json:"consolidation_failed,omitempty"`
	LastConsolidatedAt   *time.Time       `json:"last_consolidated_at,omitempty"`
	Projects             map[string]int64 `json:"projects,omitempty"`
}

// ServerDay is a server-daily/day line: one past full UTC day.
type ServerDay struct {
	Bucket time.Time         `json:"bucket"`
	New    *KindCounts       `json:"new,omitempty"`
	LLM    map[string]LLMDay `json:"llm,omitempty"`
}

// KindCounts are memories by kind.
type KindCounts struct {
	World       int64 `json:"world"`
	Experience  int64 `json:"experience"`
	Observation int64 `json:"observation"`
}

// LLMDay is one operation's LLM use over a day.
type LLMDay struct {
	Input    int64            `json:"input"`
	Output   int64            `json:"output"`
	Cached   int64            `json:"cached"`
	Thoughts int64            `json:"thoughts"`
	Requests int64            `json:"requests"`
	Statuses map[string]int64 `json:"statuses,omitempty"`
}

// DoctorRun is a doctor-history/run line: check id to ok|info|warn|error.
type DoctorRun struct {
	Checks map[string]string `json:"checks,omitempty"`
}

// MemoryDoc is a memory-docs/memory line. Document and Session are set only
// when DocKind is session; any other document is only its DocKind.
type MemoryDoc struct {
	Memory     string `json:"memory"`
	Outcome    string `json:"outcome"` // found only: gone is derived at read time
	DocKind    string `json:"doc_kind"`
	Document   string `json:"document,omitempty"`
	Session    string `json:"session,omitempty"`
	CreatedDay string `json:"created_day"` // UTC date of the day list that returned the memory
	FactType   string `json:"fact_type"`
}

// Listed is a memory-docs/listed line: a day's list was read in full. N is
// the list's total for Day; Complete is written only when true (the server
// holds no memory created on or before Day).
type Listed struct {
	Day      string `json:"day"` // UTC date
	N        int64  `json:"n"`
	Complete bool   `json:"complete,omitempty"`
}

// DocOp is a memory-docs/op line: one retain operation of a session's
// document.
type DocOp struct {
	Operation string    `json:"operation"`
	Document  string    `json:"document"`
	Session   string    `json:"session"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Retries   int       `json:"retries"`
	Units     *int64    `json:"units,omitempty"`
}

// Scan is a scan-history/scan line (no id, no text).
type Scan struct {
	Count     int64 `json:"count"`
	Live      int64 `json:"live"`
	Forgotten int64 `json:"forgotten"`
	Pages     int64 `json:"pages"`
	MS        int64 `json:"ms"`
}

func (Snapshot) Family() string  { return FamilyServerDaily }
func (Snapshot) Kind() string    { return "snapshot" }
func (ServerDay) Family() string { return FamilyServerDaily }
func (ServerDay) Kind() string   { return "day" }
func (DoctorRun) Family() string { return FamilyDoctorHistory }
func (DoctorRun) Kind() string   { return "run" }
func (MemoryDoc) Family() string { return FamilyMemoryDocs }
func (MemoryDoc) Kind() string   { return "memory" }
func (Listed) Family() string    { return FamilyMemoryDocs }
func (Listed) Kind() string      { return "listed" }
func (DocOp) Family() string     { return FamilyMemoryDocs }
func (DocOp) Kind() string       { return "op" }
func (Scan) Family() string      { return FamilyScanHistory }
func (Scan) Kind() string        { return "scan" }

// Closed enums.
var (
	snapshotErrors = []string{"not_configured", "timeout", "unreachable", "unauthorized", "not_found", "bad_response", "server_error"}
	checkLevels    = []string{"ok", "info", "warn", "error"}
	outcomes       = []string{"found"}
	docKinds       = []string{"session", "note", "import", "other", "none"}
	factTypes      = []string{"world", "experience", "observation", "other"}
	opStatuses     = []string{"pending", "processing", "completed", "failed", "other"}
	dayRE          = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

const nameMax = 256 // as the index's name guard

// nameOK mirrors the index's name guard: non-empty, bounded, valid UTF-8, no
// control character.
func nameOK(s string) bool {
	return s != "" && len(s) <= nameMax && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func enum(field, v string, set []string, allowEmpty bool) error {
	if v == "" && allowEmpty {
		return nil
	}
	if !slices.Contains(set, v) {
		return fmt.Errorf("metricsfile: %s is not one of its closed values", field)
	}
	return nil
}

func names[V any](field string, m map[string]V) error {
	for k := range m {
		if !nameOK(k) {
			return fmt.Errorf("metricsfile: %s holds an invalid name", field)
		}
	}
	return nil
}

// sessionDoc checks that document is a session's document and that sid, when
// given, is that session.
func sessionDoc(document, sid string) error {
	got, ok := session.OfDocument(document)
	if !ok {
		return fmt.Errorf("metricsfile: document is not a session's")
	}
	if sid != "" && sid != got {
		return fmt.Errorf("metricsfile: session does not match its document")
	}
	return nil
}

// validate refuses a record that could carry text or a non-session document.
func validate(r Record) error {
	if rv := reflect.ValueOf(r); r == nil || rv.Kind() == reflect.Pointer && rv.IsNil() {
		return fmt.Errorf("metricsfile: nil record")
	}
	switch v := r.(type) {
	case *Snapshot:
		return validate(*v)
	case *ServerDay:
		return validate(*v)
	case *DoctorRun:
		return validate(*v)
	case *MemoryDoc:
		return validate(*v)
	case *DocOp:
		return validate(*v)
	case *Scan:
		return validate(*v)
	case *Listed:
		return validate(*v)
	case Listed:
		if !dayRE.MatchString(v.Day) {
			return fmt.Errorf("metricsfile: listed day is not a date")
		}
		if v.N < 0 {
			return fmt.Errorf("metricsfile: listed count is negative")
		}
	case Snapshot:
		if !dayRE.MatchString(v.Day) {
			return fmt.Errorf("metricsfile: snapshot day is not a date")
		}
		if err := enum("snapshot error", v.Error, snapshotErrors, true); err != nil {
			return err
		}
		if err := names("ops", v.Ops); err != nil {
			return err
		}
		return names("projects", v.Projects)
	case ServerDay:
		for op, d := range v.LLM {
			if !nameOK(op) {
				return fmt.Errorf("metricsfile: llm holds an invalid name")
			}
			if err := names("statuses", d.Statuses); err != nil {
				return err
			}
		}
	case DoctorRun:
		for id, level := range v.Checks {
			if !nameOK(id) {
				return fmt.Errorf("metricsfile: checks holds an invalid id")
			}
			if err := enum("check level", level, checkLevels, false); err != nil {
				return err
			}
		}
	case MemoryDoc:
		if !nameOK(v.Memory) {
			return fmt.Errorf("metricsfile: memory id is invalid")
		}
		for _, e := range []error{
			enum("outcome", v.Outcome, outcomes, false),
			enum("doc_kind", v.DocKind, docKinds, false),
			enum("fact_type", v.FactType, factTypes, false),
		} {
			if e != nil {
				return e
			}
		}
		if !dayRE.MatchString(v.CreatedDay) {
			return fmt.Errorf("metricsfile: created_day is not a date")
		}
		if v.Document != "" || v.Session != "" {
			if v.DocKind != "session" {
				return fmt.Errorf("metricsfile: a document or session is kept only for a session document")
			}
			if v.Document == "" {
				if !session.ValidID(v.Session) {
					return fmt.Errorf("metricsfile: session id is invalid")
				}
				return nil
			}
			return sessionDoc(v.Document, v.Session)
		}
	case DocOp:
		if !nameOK(v.Operation) {
			return fmt.Errorf("metricsfile: operation id is invalid")
		}
		if err := enum("status", v.Status, opStatuses, false); err != nil {
			return err
		}
		return sessionDoc(v.Document, v.Session)
	}
	return nil
}

// utc returns r with its times in UTC; records without a time are as is.
func utc(r Record) Record {
	t := func(p *time.Time) *time.Time {
		if p == nil {
			return nil
		}
		u := p.UTC()
		return &u
	}
	switch v := r.(type) {
	case *Snapshot:
		return utc(*v)
	case *ServerDay:
		return utc(*v)
	case *DocOp:
		return utc(*v)
	case Snapshot:
		v.LastConsolidatedAt = t(v.LastConsolidatedAt)
		return v
	case ServerDay:
		v.Bucket = v.Bucket.UTC()
		return v
	case DocOp:
		v.CreatedAt, v.UpdatedAt = v.CreatedAt.UTC(), v.UpdatedAt.UTC()
		return v
	}
	return r
}
