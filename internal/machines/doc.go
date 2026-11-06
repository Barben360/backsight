// Package machines defines the daily figures each PC publishes, their caps,
// the strict parser for documents that come from other PCs (untrusted), the
// latency buckets, the cache of fetched documents and the publish record.
//
// It imports only the standard library and internal/fsutil.
package machines

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

const (
	Version       = 1
	DocPrefix     = "backsight:metrics:"
	MetaKey       = "metrics"
	MaxBytes      = 1 << 20   // a reader refuses a document over this
	WriteMax      = 256 << 10 // this binary writes at most this
	MaxCacheBytes = 96 << 20
	MaxDays       = 400
	MaxVersions   = 8
	MaxDocs       = 64
	MaxCount      = 1_000_000_000
	StaleAfter    = 48 * time.Hour
	DropAfter     = 30 * 24 * time.Hour
	MaxSkew       = 24 * time.Hour
)

// Edges are the fixed upper-inclusive latency bucket edges, in milliseconds.
var Edges = [8]int64{50, 100, 250, 500, 1000, 2000, 2500, 5000}

// Pair is [numerator, denominator] (for context: [characters, sessions]).
type Pair [2]int64

// Latency holds the counts of the nine latency buckets.
type Latency [9]int64

// Figures are the figures of one day, in total or for one binary version.
type Figures struct {
	Prompts                             int64
	Hit, Used, Coverage, Wrong, Context Pair
	Latency                             Latency
}

// Day is one UTC day. Flow is day level only; Versions is omitted when empty.
type Day struct {
	Day string
	Figures
	Flow     Pair
	Versions map[string]Figures
}

// Doc is the document a PC publishes. Its V is always Version.
type Doc struct {
	Machine                             string
	At                                  time.Time
	OS, FirstSeen, BinaryVersion, Until string
	Days                                []Day
}

var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)

// ValidVersion reports whether s is a binary version safe to publish and show.
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

// OS maps runtime.GOOS to the published enum.
func OS(goos string) string {
	switch goos {
	case "linux", "darwin", "windows":
		return goos
	}
	return "other"
}

type outFigures struct {
	Prompts int64   `json:"prompts"`
	Hit     Pair    `json:"hit"`
	Used    Pair    `json:"used"`
	Cov     Pair    `json:"coverage"`
	Wrong   Pair    `json:"wrong"`
	Context Pair    `json:"context"`
	Latency Latency `json:"latency"`
}

type outDay struct {
	Day      string                `json:"day"`
	Prompts  int64                 `json:"prompts"`
	Hit      Pair                  `json:"hit"`
	Used     Pair                  `json:"used"`
	Cov      Pair                  `json:"coverage"`
	Wrong    Pair                  `json:"wrong"`
	Context  Pair                  `json:"context"`
	Flow     Pair                  `json:"flow"`
	Latency  Latency               `json:"latency"`
	Versions map[string]outFigures `json:"versions,omitempty"`
}

type outDoc struct {
	V             int      `json:"v"`
	Machine       string   `json:"machine"`
	At            int64    `json:"at"`
	OS            string   `json:"os"`
	FirstSeen     string   `json:"first_seen"`
	BinaryVersion string   `json:"binary_version"`
	Until         string   `json:"until"`
	Edges         [8]int64 `json:"latency_edges_ms"`
	Days          []outDay `json:"days"`
}

func outF(f Figures) outFigures {
	return outFigures{f.Prompts, f.Hit, f.Used, f.Coverage, f.Wrong, f.Context, f.Latency}
}

// Marshal returns the document's compact JSON, the fields in the published order.
func (d Doc) Marshal() (string, error) {
	o := outDoc{V: Version, Machine: d.Machine, At: d.At.Unix(), OS: d.OS, FirstSeen: d.FirstSeen,
		BinaryVersion: d.BinaryVersion, Until: d.Until, Edges: Edges, Days: make([]outDay, 0, len(d.Days))}
	for _, day := range d.Days {
		f := day.Figures
		od := outDay{Day: day.Day, Prompts: f.Prompts, Hit: f.Hit, Used: f.Used, Cov: f.Coverage, Wrong: f.Wrong,
			Context: f.Context, Flow: day.Flow, Latency: f.Latency}
		if len(day.Versions) > 0 {
			od.Versions = make(map[string]outFigures, len(day.Versions))
			for k, v := range day.Versions {
				od.Versions[k] = outF(v)
			}
		}
		o.Days = append(o.Days, od)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Dropped counts what Fit left out.
type Dropped struct{ VersionDays, Days int }

// Fit returns d trimmed to a marshaled size of at most limit bytes: it drops
// the versions of the oldest days first, one day at a time, then the oldest
// days. The newest day and the head are always kept; the error says that even
// they do not fit. d itself is not changed.
func Fit(d Doc, limit int) (Doc, Dropped, error) {
	var dr Dropped
	out := d
	out.Days = append([]Day(nil), d.Days...)
	fits := func() (bool, error) {
		s, err := out.Marshal()
		return len(s) <= limit, err
	}
	ok, err := fits()
	for err == nil && !ok {
		switch i := oldestWithVersions(out.Days); {
		case i >= 0:
			out.Days[i].Versions = nil
			dr.VersionDays++
		case len(out.Days) > 1:
			out.Days = out.Days[1:]
			dr.Days++
		default:
			return d, Dropped{}, fmt.Errorf("machines: the document does not fit %d bytes even with one day", limit)
		}
		ok, err = fits()
	}
	if err != nil {
		return d, Dropped{}, err
	}
	return out, dr, nil
}

func oldestWithVersions(days []Day) int {
	for i, d := range days {
		if len(d.Versions) > 0 {
			return i
		}
	}
	return -1
}
