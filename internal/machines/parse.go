package machines

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Reason says why a document was ignored.
type Reason string

const (
	TooLarge       Reason = "too_large"
	UnknownVersion Reason = "unknown_version"
	NotValid       Reason = "not_valid"
)

// ParseError is the only error Parse returns. Err is for tests and logs: it
// is never part of Error() and must never be shown or stored.
type ParseError struct {
	Reason Reason
	Err    error
}

func (e *ParseError) Error() string { return "machines: document ignored: " + string(e.Reason) }
func (e *ParseError) Unwrap() error { return e.Err }

func bad(format string, a ...any) error {
	return &ParseError{Reason: NotValid, Err: fmt.Errorf(format, a...)}
}

// UnmarshalJSON requires exactly 2 non-null integers.
func (p *Pair) UnmarshalJSON(b []byte) error {
	v, err := ints(b, len(p))
	if err != nil {
		return err
	}
	copy(p[:], v)
	return nil
}

// UnmarshalJSON requires exactly 9 non-null integers.
func (l *Latency) UnmarshalJSON(b []byte) error {
	v, err := ints(b, len(l))
	if err != nil {
		return err
	}
	copy(l[:], v)
	return nil
}

func ints(b []byte, n int) ([]int64, error) {
	var raw []*int64
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if len(raw) != n {
		return nil, fmt.Errorf("want %d integers, got %d", n, len(raw))
	}
	out := make([]int64, n)
	for i, v := range raw {
		if v == nil {
			return nil, errors.New("null in an integer array")
		}
		out[i] = *v
	}
	return out, nil
}

type wireFigures struct {
	Prompts  *int64   `json:"prompts"`
	Hit      *Pair    `json:"hit"`
	Used     *Pair    `json:"used"`
	Coverage *Pair    `json:"coverage"`
	Wrong    *Pair    `json:"wrong"`
	Context  *Pair    `json:"context"`
	Latency  *Latency `json:"latency"`
}

type wireDay struct {
	Day *string `json:"day"`
	wireFigures
	Flow     *Pair                   `json:"flow"`
	Versions map[string]*wireFigures `json:"versions"`
}

type wireDoc struct {
	Machine       *string     `json:"machine"`
	At            *int64      `json:"at"`
	OS            *string     `json:"os"`
	FirstSeen     *string     `json:"first_seen"`
	BinaryVersion *string     `json:"binary_version"`
	Until         *string     `json:"until"`
	Edges         *[]int64    `json:"latency_edges_ms"`
	Days          *[]*wireDay `json:"days"`
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Short returns the first 8 characters of the machine id in name, or "" when
// name is not DocPrefix plus a lower-case UUID.
func Short(name string) string {
	id, ok := strings.CutPrefix(name, DocPrefix)
	if !ok || !uuidRe.MatchString(id) {
		return ""
	}
	return id[:8]
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil || t.Format("2006-01-02") != s {
		return time.Time{}, false
	}
	return t, true
}

var minAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Parse reads the document named name (DocPrefix plus the machine id) from
// raw, which is untrusted. Every error is a *ParseError. A document that
// fails is dropped whole; unknown fields are ignored.
func Parse(name, raw string, now time.Time) (Doc, error) {
	if len(raw) > MaxBytes {
		return Doc{}, &ParseError{Reason: TooLarge}
	}
	var probe struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return Doc{}, &ParseError{Reason: NotValid, Err: err}
	}
	if len(probe.V) == 0 || string(probe.V) == "null" {
		return Doc{}, bad("no v")
	}
	var v int64
	if err := json.Unmarshal(probe.V, &v); err != nil {
		return Doc{}, &ParseError{Reason: NotValid, Err: err}
	}
	if v != Version {
		return Doc{}, &ParseError{Reason: UnknownVersion, Err: fmt.Errorf("v %d", v)}
	}

	var w wireDoc
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&w); err != nil {
		return Doc{}, &ParseError{Reason: NotValid, Err: err}
	}
	if dec.More() {
		return Doc{}, bad("a second value after the object")
	}
	return build(name, &w, now)
}

func build(name string, w *wireDoc, now time.Time) (Doc, error) {
	if w.Machine == nil || w.At == nil || w.OS == nil || w.FirstSeen == nil || w.BinaryVersion == nil ||
		w.Until == nil || w.Edges == nil || w.Days == nil {
		return Doc{}, bad("a field is missing or null")
	}
	id, ok := strings.CutPrefix(name, DocPrefix)
	if !ok || !uuidRe.MatchString(id) || *w.Machine != id {
		return Doc{}, bad("machine does not match the document name")
	}
	at := time.Unix(*w.At, 0).UTC()
	if at.Before(minAt) || at.After(now.Add(MaxSkew)) {
		return Doc{}, bad("at out of range")
	}
	if *w.OS != OS(*w.OS) {
		return Doc{}, bad("os not in the enum")
	}
	if _, ok := parseDate(*w.FirstSeen); !ok {
		return Doc{}, bad("first_seen is not a date")
	}
	until, ok := parseDate(*w.Until)
	if !ok {
		return Doc{}, bad("until is not a date")
	}
	if until.After(time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)) {
		return Doc{}, bad("until after the document's date")
	}
	if !ValidVersion(*w.BinaryVersion) {
		return Doc{}, bad("binary_version not valid")
	}
	if len(*w.Edges) != len(Edges) {
		return Doc{}, bad("latency_edges_ms differ")
	}
	for i, e := range *w.Edges {
		if e != Edges[i] {
			return Doc{}, bad("latency_edges_ms differ")
		}
	}
	if len(*w.Days) > MaxDays {
		return Doc{}, bad("more than %d days", MaxDays)
	}

	d := Doc{Machine: *w.Machine, At: at, OS: *w.OS, FirstSeen: *w.FirstSeen, BinaryVersion: *w.BinaryVersion,
		Until: *w.Until, Days: make([]Day, 0, len(*w.Days))}
	first := until.AddDate(0, 0, -(MaxDays - 1))
	var prev time.Time
	for _, wd := range *w.Days {
		if wd == nil || wd.Day == nil || wd.Flow == nil {
			return Doc{}, bad("a day is missing or null, or lacks a field")
		}
		t, ok := parseDate(*wd.Day)
		if !ok {
			return Doc{}, bad("day is not a date")
		}
		if t.Before(first) || t.After(until) {
			return Doc{}, bad("day outside [until - %d d, until]", MaxDays-1)
		}
		if !prev.IsZero() && !t.After(prev) {
			return Doc{}, bad("days not ascending and unique")
		}
		prev = t
		f, err := figures(&wd.wireFigures)
		if err != nil {
			return Doc{}, err
		}
		if err := checkPair(*wd.Flow, true); err != nil {
			return Doc{}, err
		}
		day := Day{Day: *wd.Day, Figures: f, Flow: *wd.Flow}
		if len(wd.Versions) > MaxVersions {
			return Doc{}, bad("more than %d versions", MaxVersions)
		}
		if len(wd.Versions) > 0 {
			day.Versions = make(map[string]Figures, len(wd.Versions))
		}
		for k, wv := range wd.Versions {
			if !ValidVersion(k) {
				return Doc{}, bad("version key not valid")
			}
			vf, err := figures(wv)
			if err != nil {
				return Doc{}, err
			}
			if !within(vf, f) {
				return Doc{}, bad("a version figure above the day's")
			}
			day.Versions[k] = vf
		}
		d.Days = append(d.Days, day)
	}
	return d, nil
}

func checkCount(n int64) error {
	if n < 0 || n > MaxCount {
		return bad("count %d out of [0, %d]", n, MaxCount)
	}
	return nil
}

// checkPair ranges a pair; ordered also requires num <= den (not for context).
func checkPair(p Pair, ordered bool) error {
	for _, n := range p {
		if err := checkCount(n); err != nil {
			return err
		}
	}
	if ordered && p[0] > p[1] {
		return bad("numerator above denominator")
	}
	return nil
}

func figures(w *wireFigures) (Figures, error) {
	if w == nil || w.Prompts == nil || w.Hit == nil || w.Used == nil || w.Coverage == nil || w.Wrong == nil ||
		w.Context == nil || w.Latency == nil {
		return Figures{}, bad("a figure is missing or null")
	}
	f := Figures{Prompts: *w.Prompts, Hit: *w.Hit, Used: *w.Used, Coverage: *w.Coverage, Wrong: *w.Wrong,
		Context: *w.Context, Latency: *w.Latency}
	if err := checkCount(f.Prompts); err != nil {
		return Figures{}, err
	}
	for _, p := range []Pair{f.Hit, f.Used, f.Coverage, f.Wrong} {
		if err := checkPair(p, true); err != nil {
			return Figures{}, err
		}
	}
	if err := checkPair(f.Context, false); err != nil {
		return Figures{}, err
	}
	for _, n := range f.Latency {
		if err := checkCount(n); err != nil {
			return Figures{}, err
		}
	}
	return f, nil
}

// within reports whether every figure of v is at most the day's.
func within(v, day Figures) bool {
	if v.Prompts > day.Prompts {
		return false
	}
	for i, p := range []Pair{v.Hit, v.Used, v.Coverage, v.Wrong, v.Context} {
		d := []Pair{day.Hit, day.Used, day.Coverage, day.Wrong, day.Context}[i]
		if p[0] > d[0] || p[1] > d[1] {
			return false
		}
	}
	for i, n := range v.Latency {
		if n > day.Latency[i] {
			return false
		}
	}
	return true
}
