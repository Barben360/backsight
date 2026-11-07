package indexgen

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/machines"
)

// WriteMachines writes metricsDir/machines.json, the cache of fetched
// documents of s.OtherPCs other PCs, as the fetch step leaves it: one
// document each, dated 3 hours before Now (the last one 3 days before, so
// stale), covering the days from the first of s's months to the day before
// its date (at most machines.MaxDays), with a version row or two a day;
// plus one ignored entry. The same Spec gives the same cache.
func WriteMachines(t testing.TB, metricsDir string, s Spec) {
	t.Helper()
	rng := rand.New(rand.NewPCG(s.Seed, 0x6d616368)) // apart from the lines' generator
	first := monthSpan(s, End())[0][0]
	c := machines.Cache{FetchedAt: Now.Add(-time.Hour)}
	for i := 0; i < s.OtherPCs; i++ {
		id := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", rng.Uint32(), rng.Uint32N(1<<16), rng.Uint32N(1<<12), rng.Uint32N(1<<12), rng.Uint64N(1<<48))
		at := Now.Add(-3 * time.Hour)
		if s.OtherPCs > 1 && i == s.OtherPCs-1 {
			at = Now.Add(-3 * 24 * time.Hour)
		}
		until := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
		start := first
		if lim := until.AddDate(0, 0, -(machines.MaxDays - 1)); lim.After(start) {
			start = lim
		}
		d := machines.Doc{Machine: id, At: at, OS: []string{"linux", "darwin", "windows"}[i%3],
			FirstSeen: start.Format("2006-01-02"), BinaryVersion: "v0.1.30-m7", Until: until.Format("2006-01-02")}
		for day := start; !day.After(until); day = day.AddDate(0, 0, 1) {
			f := figures(rng)
			dd := machines.Day{Day: day.Format("2006-01-02"), Figures: f, Flow: ordered(rng, 20)}
			// A version row is the day's figures or half of them, so that
			// each stays within the day's.
			dd.Versions = map[string]machines.Figures{"v0.1.30-m7": f}
			if rng.IntN(2) == 0 {
				dd.Versions["v0.1.29"] = halve(f)
			}
			d.Days = append(d.Days, dd)
		}
		raw, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		c.Docs = append(c.Docs, machines.CachedDoc{Name: machines.DocPrefix + id, Raw: raw})
	}
	c.Ignored = []machines.Ignored{{Machine: fmt.Sprintf("%08x", rng.Uint32()), Reason: machines.NotValid}}
	if err := os.MkdirAll(metricsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := machines.SaveCache(metricsDir, c); err != nil {
		t.Fatal(err)
	}
}

// ordered is a pair [n, d] with n ≤ d ≤ max.
func ordered(rng *rand.Rand, max int) machines.Pair {
	d := int64(1 + rng.IntN(max))
	return machines.Pair{rng.Int64N(d + 1), d}
}

func figures(rng *rand.Rand) machines.Figures {
	f := machines.Figures{Prompts: int64(5 + rng.IntN(60)), Hit: ordered(rng, 60), Used: ordered(rng, 20), Coverage: ordered(rng, 20),
		Wrong: ordered(rng, 12), Context: machines.Pair{int64(rng.IntN(200_000)), int64(1 + rng.IntN(8))}}
	for i := range f.Latency {
		f.Latency[i] = int64(rng.IntN(15))
	}
	return f
}

// halve is f with every count halved (each pair stays ordered).
func halve(f machines.Figures) machines.Figures {
	h := func(p machines.Pair) machines.Pair { return machines.Pair{p[0] / 2, p[1] / 2} }
	g := machines.Figures{Prompts: f.Prompts / 2, Hit: h(f.Hit), Used: h(f.Used), Coverage: h(f.Coverage), Wrong: h(f.Wrong), Context: h(f.Context)}
	for i, v := range f.Latency {
		g.Latency[i] = v / 2
	}
	return g
}

// The canary strings of HostileCache, one per way a fetched document can
// carry text that must never reach a table, a view or a page.
const (
	CanaryUnknown = "CANARY-UNKNOWN-FIELD-31f"  // in a field the reader does not know
	CanaryScript  = "CANARY-SCRIPT-VERSION-8c2" // in a version key, inside <script>
	CanaryBig     = "CANARY-OVERSIZE-DOC-5d9"   // in a document over the size cap
	CanaryName    = "CANARY-DOC-NAME-a07"       // in a document name that holds no machine id
	CanaryIgnored = "CANARY-IGNORED-ENTRY-e44"  // in the cache's own list of ignored documents
)

// HostileBigMachine is the machine id of the oversize document of
// HostileCache.
const HostileBigMachine = "c0c0c0c0-1111-4222-8333-444455556666"

// HostileCanaries are every canary of HostileCache, and the tag that holds
// one.
var HostileCanaries = []string{CanaryUnknown, CanaryScript, CanaryBig, CanaryName, CanaryIgnored, "<script>"}

// HostileCache is a cache of hostile documents: good with an unknown field
// that holds a canary (a reader ignores the field, so the document stays a
// good one), scripty with a <script> canary for its "v0.1.29" version key
// (it must hold that version), a document over the size cap, a document
// whose name holds no machine id, and an ignored entry that names a canary.
// Only good is counted by a reader; the other four are set aside whole.
func HostileCache(t testing.TB, good, scripty machines.Doc, fetchedAt time.Time) machines.Cache {
	t.Helper()
	raw, err := good.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := strings.Replace(raw, `{"v":1,`, `{"v":1,"`+CanaryUnknown+`":{"deep":"`+CanaryUnknown+`"},`, 1)
	if withUnknown == raw {
		t.Fatal("the unknown field was not added")
	}
	sraw, err := scripty.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	scriptRaw := strings.Replace(sraw, `"v0.1.29":{`, `"<script>`+CanaryScript+`</script>":{`, 1)
	if scriptRaw == sraw {
		t.Fatal("the script version was not added")
	}
	big := `{"v":1,"x":"` + strings.Repeat("a", machines.MaxBytes) + CanaryBig + `"}` // over the 1 MiB cap
	return machines.Cache{FetchedAt: fetchedAt,
		Docs: []machines.CachedDoc{
			{Name: machines.DocPrefix + good.Machine, Raw: withUnknown},
			{Name: machines.DocPrefix + scripty.Machine, Raw: scriptRaw},
			{Name: machines.DocPrefix + HostileBigMachine, Raw: big},
			{Name: machines.DocPrefix + CanaryName, Raw: raw},
		},
		Ignored: []machines.Ignored{{Machine: CanaryIgnored, Reason: machines.Reason(CanaryIgnored)}}}
}
