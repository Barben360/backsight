// Package stats reads the local telemetry (Appendix A.4) back into the
// numbers `stats` shows and `doctor` checks (SPEC §15). Nothing here calls
// the server.
package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// LongPrompt is the length from which a prompt counts in the search rate
// (C12's REMINDER_MIN_CHARS).
const LongPrompt = 40

// Event is one telemetry line, with the fields any reader here needs.
type Event struct {
	TS      time.Time
	Event   string
	Session string

	N        int
	Decision string
	Reason   string
	Plen     int
	Scopes   map[string]struct {
		OK bool `json:"ok"`
	}
	Injected     int
	MS           int
	Detector     *string
	Chars        int
	IDs          []string // prompt, outcome, briefing
	Searched     bool
	Credited     *string
	Credit       string
	Contradicted bool
	Source       string
	Name         string
	OK           bool
	Live         string
	MemoryIDs    []string
	Subagent     bool
	Status       string
	Kind         string
	Types        map[string]int
	Details      map[string]any
}

// line decodes "ids", an array in most events and a count in tool_use.
type line struct {
	TS  string          `json:"ts"`
	IDs json.RawMessage `json:"ids"`
	Event
	MemoryIDs []string `json:"memory_ids"`
}

// Load reads the events in [from, to) from <state>/telemetry, skipping
// lines that do not parse (a write cut short by a full disk).
func Load(dir string, from, to time.Time) ([]Event, error) {
	var out []Event
	for m := time.Date(from.UTC().Year(), from.UTC().Month(), 1, 0, 0, 0, 0, time.UTC); m.Before(to); m = m.AddDate(0, 1, 0) {
		f, err := os.Open(filepath.Join(dir, "events-"+m.Format("2006-01")+".jsonl"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var l line
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, l.TS)
			if err != nil || ts.Before(from) || !ts.Before(to) {
				continue
			}
			e := l.Event
			e.TS, e.MemoryIDs = ts, l.MemoryIDs
			_ = json.Unmarshal(l.IDs, &e.IDs) // a count (tool_use) leaves it empty
			out = append(out, e)
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// Report is what `stats [days]` shows.
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Prompts struct {
		Human         int            `json:"human"`
		Machine       int            `json:"machine"`
		Recalls       int            `json:"recalls"`
		WithHits      int            `json:"with_hits"` // recalls that injected something
		Skips         map[string]int `json:"skips"`
		P50MS         int            `json:"p50_ms"` // prompt hook, recalls only
		P95MS         int            `json:"p95_ms"`
		ScopeCalls    int            `json:"scope_calls"`
		ScopeFailures int            `json:"scope_failures"`
	} `json:"prompts"`
	// Turns: human prompts of at least LongPrompt characters, and whether a
	// memory tool ran before the next one (C12's search rate).
	Turns struct {
		Long     int `json:"long"`
		Searched int `json:"searched"`
		// Credit of the replies that followed a search: exact, free,
		// empty or silent.
		Credit map[string]int `json:"credit"`
	} `json:"turns"`
	Injections struct {
		N            int `json:"n"`
		Searched     int `json:"searched"`
		Credited     int `json:"credited"` // exact or free
		Contradicted int `json:"contradicted"`
	} `json:"injections"`
	Briefings struct {
		N     int `json:"n"`
		Empty int `json:"empty"`
		IDs   int `json:"ids"`
		Used  int `json:"used"` // briefed memories a memory tool returned later in the session
	} `json:"briefings"`
	Detector struct {
		Fired int `json:"fired"`
		Saved int `json:"saved"` // a memory_store in the same turn or the next
	} `json:"detector"`
	Budget struct {
		Sessions int `json:"sessions"`
		P95Chars int `json:"p95_chars"` // recall text injected per session
	} `json:"budget"`
	Gate         map[string]int `json:"gate"`
	Tools        map[string]int `json:"tools"`
	Live         map[string]int `json:"live"` // how the MCP server found its session
	Retains      map[string]int `json:"retains"`
	HookFailures map[string]int `json:"hook_failures"`
	Unknown      map[string]int `json:"unknown_records"`
}

type turn struct {
	session string
	n       int
}

func isMemoryTool(name string) bool {
	return strings.HasSuffix(name, "memory_search") || strings.HasSuffix(name, "memory_read") || strings.HasSuffix(name, "memory_store")
}

// Compute builds the report of events (already limited to its window).
func Compute(events []Event, from, to time.Time) Report {
	var r Report
	r.From, r.To = from, to
	r.Prompts.Skips, r.Turns.Credit = map[string]int{}, map[string]int{}
	r.Gate, r.Tools, r.Live, r.Retains, r.HookFailures, r.Unknown = map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	searched, stored := map[turn]bool{}, map[turn]bool{}
	returned := map[string]map[string]time.Time{} // session → memory id → first time a tool returned it
	for _, e := range events {
		if e.Event != "tool_use" || e.Subagent || !isMemoryTool(e.Name) {
			continue
		}
		t := turn{e.Session, e.N}
		if strings.HasSuffix(e.Name, "memory_store") {
			stored[t] = true
		} else {
			searched[t] = true
		}
		if returned[e.Session] == nil {
			returned[e.Session] = map[string]time.Time{}
		}
		for _, id := range e.MemoryIDs {
			if _, ok := returned[e.Session][id]; !ok {
				returned[e.Session][id] = e.TS
			}
		}
	}
	var ms []int
	chars := map[string]int{}
	for _, e := range events {
		switch e.Event {
		case "prompt":
			if e.Reason == "machine" {
				r.Prompts.Machine++
				continue
			}
			r.Prompts.Human++
			if e.Session != "" {
				chars[e.Session] += e.Chars
			}
			if e.Plen >= LongPrompt {
				r.Turns.Long++
				if searched[turn{e.Session, e.N}] {
					r.Turns.Searched++
				}
			}
			if e.Detector != nil {
				r.Detector.Fired++
				if stored[turn{e.Session, e.N}] || stored[turn{e.Session, e.N + 1}] {
					r.Detector.Saved++
				}
			}
			if e.Decision != "recall" {
				r.Prompts.Skips[e.Reason]++
				continue
			}
			r.Prompts.Recalls++
			ms = append(ms, e.MS)
			if e.Injected > 0 {
				r.Prompts.WithHits++
			}
			for _, sc := range e.Scopes {
				r.Prompts.ScopeCalls++
				if !sc.OK {
					r.Prompts.ScopeFailures++
				}
			}
		case "reply":
			if e.Searched {
				c := e.Credit
				if c == "" {
					c = "silent"
				}
				r.Turns.Credit[c]++
			}
		case "outcome":
			r.Injections.N++
			if e.Searched {
				r.Injections.Searched++
			}
			if e.Credited != nil && (*e.Credited == "exact" || *e.Credited == "free") {
				r.Injections.Credited++
			}
			if e.Contradicted {
				r.Injections.Contradicted++
			}
		case "briefing":
			r.Briefings.N++
			if e.Source == "empty" || len(e.IDs) == 0 {
				r.Briefings.Empty++
			}
			r.Briefings.IDs += len(e.IDs)
			for _, id := range e.IDs {
				if at, ok := returned[e.Session][id]; ok && at.After(e.TS) {
					r.Briefings.Used++
				}
			}
		case "gate":
			r.Gate[e.Decision]++
		case "tool":
			r.Tools[e.Name]++
			if e.Live != "" {
				r.Live[e.Live]++
			}
		case "retain":
			r.Retains[e.Status]++
		case "hook_failure":
			r.HookFailures[e.Kind]++
		case "transcript_unknown":
			for t, n := range e.Types {
				r.Unknown[t] += n
			}
		}
	}
	r.Prompts.P50MS, r.Prompts.P95MS = Percentile(ms, 50), Percentile(ms, 95)
	var per []int
	for _, c := range chars {
		per = append(per, c)
	}
	r.Budget.Sessions, r.Budget.P95Chars = len(per), Percentile(per, 95)
	return r
}

// Percentile is the nearest-rank percentile of xs; 0 for none.
func Percentile(xs []int, p int) int {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := (p*len(s)+99)/100 - 1
	return s[max(0, min(i, len(s)-1))]
}
