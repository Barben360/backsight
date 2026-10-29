package feedback

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// sharedNow is a whole second, as the shared document keeps times.
var sharedNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func daysBefore(d float64) time.Time {
	return sharedNow.Add(-time.Duration(d * float64(24*time.Hour)))
}

// The document is the verbatim format: v, machine, at in unix seconds and
// one array per memory of credits, credits_at, misses, misses_at and
// hidden_since, floats at 4 decimals, 0 for a zero time; any key order
// reads back, a zero time stays zero, an unknown v is ErrSharedVersion.
func TestSharedDocRoundTrip(t *testing.T) {
	doc := SharedDoc{V: SharedVersion, Machine: "pc-a", At: sharedNow, Entries: map[string]SharedEntry{
		"m1": {Credits: 1.23456789, CreditsAt: daysBefore(10), Misses: 0.5, MissesAt: daysBefore(3)},
		"o1": {HiddenSince: daysBefore(1)},
	}}
	s, err := doc.MarshalMeta()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	if len(raw) != 4 || string(raw["v"]) != "1" || string(raw["machine"]) != `"pc-a"` || string(raw["at"]) != fmt.Sprint(sharedNow.Unix()) {
		t.Errorf("document: %s", s)
	}
	var entries map[string]json.RawMessage
	_ = json.Unmarshal(raw["entries"], &entries)
	wantM1 := fmt.Sprintf("[1.2346,%d,0.5,%d,0]", daysBefore(10).Unix(), daysBefore(3).Unix())
	wantO1 := fmt.Sprintf("[0,0,0,0,%d]", daysBefore(1).Unix())
	if len(entries) != 2 || string(entries["m1"]) != wantM1 || string(entries["o1"]) != wantO1 {
		t.Errorf("entries: %s, want m1 %s and o1 %s", raw["entries"], wantM1, wantO1)
	}

	back, err := ParseSharedDoc(s)
	if err != nil {
		t.Fatal(err)
	}
	m1, o1 := back.Entries["m1"], back.Entries["o1"]
	if back.V != 1 || back.Machine != "pc-a" || !back.At.Equal(sharedNow) || len(back.Entries) != 2 ||
		m1.Credits != 1.2346 || !m1.CreditsAt.Equal(daysBefore(10)) || m1.Misses != 0.5 || !m1.MissesAt.Equal(daysBefore(3)) || !m1.HiddenSince.IsZero() ||
		o1.Credits != 0 || !o1.CreditsAt.IsZero() || !o1.MissesAt.IsZero() || !o1.HiddenSince.Equal(daysBefore(1)) {
		t.Errorf("parsed back: %+v", back)
	}

	reordered, err := ParseSharedDoc(`{"entries":{"m2":[2,1792000000,0,0,0]},"at":1793000000,"machine":"pc-b","v":1}`)
	if m2 := reordered.Entries["m2"]; err != nil || reordered.Machine != "pc-b" || reordered.At.Unix() != 1793000000 ||
		m2.Credits != 2 || m2.CreditsAt.Unix() != 1792000000 || !m2.MissesAt.IsZero() || !m2.HiddenSince.IsZero() {
		t.Errorf("reordered keys: %+v %v", reordered, err)
	}

	for _, s := range []string{
		`{"v":2,"machine":"pc-c","at":1793000000,"entries":{"m1":[1,2,3,4,5,6]}}`,
		`{"machine":"pc-c","at":1793000000,"entries":{}}`,
	} {
		if _, err := ParseSharedDoc(s); !errors.Is(err, ErrSharedVersion) {
			t.Errorf("%s: %v, want ErrSharedVersion", s, err)
		}
	}
	for _, s := range []string{`not json`, `{"v":1,"machine":"pc-c","at":1,"entries":{"m1":[1,2,3,4]}}`} {
		if _, err := ParseSharedDoc(s); err == nil || errors.Is(err, ErrSharedVersion) {
			t.Errorf("%s: %v, want a parse error", s, err)
		}
	}
}

// Export keeps the 5000 most recently shown memories, a hidden one always
// and first, leaves out a memory whose counts both decayed below 0.01,
// and carries each sum at its own time: the same ledger exports the same
// entries an hour later.
func TestExportCapsAndPrunes(t *testing.T) {
	l := newLedger()
	for i := range 5001 {
		l.Memories[fmt.Sprintf("m%04d", i)] = Entry{Shown: 1, Credits: 1, CreditsAt: daysBefore(1), LastShown: sharedNow.Add(time.Duration(i-5001) * time.Minute)}
	}
	doc := l.Export("pc-a", sharedNow, testParams)
	if _, ok := doc.Entries["m0000"]; len(doc.Entries) != 5000 || ok || doc.Entries["m5000"].Credits != 1 {
		t.Errorf("5001 memories: %d exported, least recently shown kept: %v", len(doc.Entries), ok)
	}
	if doc.V != SharedVersion || doc.Machine != "pc-a" || !doc.At.Equal(sharedNow) {
		t.Errorf("header: %d %q %v", doc.V, doc.Machine, doc.At)
	}
	l.Memories["hidden-old"] = Entry{Hidden: &Hidden{Since: daysBefore(400)}}
	doc = l.Export("pc-a", sharedNow, testParams)
	_, kept0 := doc.Entries["m0000"]
	_, kept1 := doc.Entries["m0001"]
	if len(doc.Entries) != 5000 || kept0 || kept1 || !doc.Entries["hidden-old"].HiddenSince.Equal(daysBefore(400)) {
		t.Errorf("with a hidden memory: %d exported, m0000 %v, m0001 %v, hidden %+v", len(doc.Entries), kept0, kept1, doc.Entries["hidden-old"])
	}

	// 2^-7 ≈ 0.0078 is below 0.01, 2^-6 ≈ 0.0156 is not.
	small := Ledger{Memories: map[string]Entry{
		"faded":        {Shown: 9, Credits: 1, CreditsAt: daysBefore(7 * 365), Misses: 1, MissesAt: daysBefore(7 * 90), LastShown: sharedNow},
		"credits-left": {Shown: 9, Credits: 1, CreditsAt: daysBefore(6 * 365), Misses: 1, MissesAt: daysBefore(7 * 90)},
		"misses-left":  {Shown: 9, Credits: 1, CreditsAt: daysBefore(7 * 365), Misses: 1, MissesAt: daysBefore(6 * 90)},
		"shown-only":   {Shown: 3, LastShown: sharedNow},
		"hidden-faded": {Shown: 9, Credits: 1, CreditsAt: daysBefore(7 * 365), Hidden: &Hidden{Since: daysBefore(2), Verdict: "wrong"}},
	}}
	doc = small.Export("pc-a", sharedNow, testParams)
	_, faded := doc.Entries["faded"]
	_, shownOnly := doc.Entries["shown-only"]
	_, credits := doc.Entries["credits-left"]
	_, misses := doc.Entries["misses-left"]
	if faded || shownOnly || !credits || !misses || len(doc.Entries) != 3 {
		t.Errorf("pruning: %+v", doc.Entries)
	}
	h := doc.Entries["hidden-faded"]
	if h.Credits != 1 || !h.CreditsAt.Equal(daysBefore(7*365)) || !h.HiddenSince.Equal(daysBefore(2)) {
		t.Errorf("hidden and faded: %+v", h)
	}
	later := small.Export("pc-a", sharedNow.Add(time.Hour), testParams)
	for id, e := range doc.Entries {
		if later.Entries[id] != e {
			t.Errorf("%s an hour later: %+v, was %+v", id, later.Entries[id], e)
		}
	}
}

var sharedParams = Params{Weight: 0.1, PriorCredits: 3, PriorMisses: 9, CreditHalfLife: 365 * 24 * time.Hour, MissHalfLife: 90 * 24 * time.Hour}

// viaMeta is doc as another machine reads it back from the server.
func viaMeta(t *testing.T, doc SharedDoc) SharedDoc {
	t.Helper()
	s, err := doc.MarshalMeta()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSharedDoc(s)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// Two machines credit one memory at different times: its boost here uses
// this machine's and both others' counts, each decayed from its own time;
// this machine's own document is never counted, a memory only the others
// know gets an entry, and the ledger itself stays as it was. Every input
// has at most 4 decimals and whole seconds, so the document's rounding
// changes nothing and the boosts agree within 1e-12.
func TestCombinedSumsMachines(t *testing.T) {
	local := Ledger{Schema: LedgerSchema, Watermark: daysBefore(1), Memories: map[string]Entry{
		"m1": {Shown: 4, Credits: 1, CreditsAt: daysBefore(10), LastShown: daysBefore(10), LastCredited: daysBefore(10), Wrong: 1, Imported: Imported{Shown: 2, Credits: 1}},
	}}
	docs := []SharedDoc{
		viaMeta(t, SharedDoc{V: 1, Machine: "pc-b", At: daysBefore(2), Entries: map[string]SharedEntry{
			"m1": {Credits: 2, CreditsAt: daysBefore(100)},
		}}),
		viaMeta(t, SharedDoc{V: 1, Machine: "pc-c", At: daysBefore(5), Entries: map[string]SharedEntry{
			"m1": {Credits: 1.5, CreditsAt: daysBefore(200), Misses: 1, MissesAt: daysBefore(50)},
			"r1": {Misses: 2.25, MissesAt: daysBefore(90)},
		}}),
		viaMeta(t, SharedDoc{V: 1, Machine: "pc-a", At: daysBefore(1), Entries: map[string]SharedEntry{
			"m1": {Credits: 50, CreditsAt: daysBefore(1)},
		}}),
	}
	got := local.WithRemote(CombineRemote(docs, "pc-a", sharedNow, sharedParams), sharedParams, sharedNow)

	c := 1*math.Exp2(-10.0/365) + 2*math.Exp2(-100.0/365) + 1.5*math.Exp2(-200.0/365)
	m := math.Exp2(-50.0 / 90)
	want := 0.1 * ((c+3)/(c+m+12) - 0.25)
	if b := got.Boost("m1", sharedParams, sharedNow); math.Abs(b-want) > 1e-12 {
		t.Errorf("m1 boost %v, want %v (credits %v, misses %v)", b, want, c, m)
	}
	wantR1 := 0.1 * (3/(1.125+12) - 0.25)
	if b := got.Boost("r1", sharedParams, sharedNow); math.Abs(b-wantR1) > 1e-12 || got.Memories["r1"].Shown != 0 {
		t.Errorf("r1 boost %v, want %v; entry %+v", b, wantR1, got.Memories["r1"])
	}
	e := got.Memories["m1"]
	if e.Shown != 4 || !e.LastShown.Equal(daysBefore(10)) || !e.LastCredited.Equal(daysBefore(10)) || e.Wrong != 1 || e.Imported != (Imported{Shown: 2, Credits: 1}) {
		t.Errorf("m1's own counts changed: %+v", e)
	}
	if !got.Watermark.Equal(local.Watermark) || got.Schema != LedgerSchema {
		t.Errorf("ledger header changed: %+v", got)
	}
	if lm := local.Memories["m1"]; len(local.Memories) != 1 || lm.Credits != 1 || !lm.CreditsAt.Equal(daysBefore(10)) {
		t.Errorf("WithRemote changed the ledger: %+v", local.Memories)
	}
}

// A document whose at is more than 30 days old is a machine no longer in
// use: it counts nowhere.
func TestOldDocumentsAreIgnored(t *testing.T) {
	docs := []SharedDoc{
		{V: 1, Machine: "pc-old", At: daysBefore(31), Entries: map[string]SharedEntry{
			"m1": {Credits: 1, CreditsAt: daysBefore(31)},
			"o1": {HiddenSince: daysBefore(31)},
		}},
		{V: 1, Machine: "pc-recent", At: daysBefore(29), Entries: map[string]SharedEntry{
			"m2": {Credits: 1, CreditsAt: daysBefore(29)},
		}},
	}
	r := CombineRemote(docs, "pc-a", sharedNow, sharedParams)
	if _, ok := r["m1"]; ok {
		t.Errorf("31 days old counted: %+v", r)
	}
	if _, ok := r["o1"]; ok {
		t.Errorf("31 days old hiding counted: %+v", r)
	}
	if want := math.Exp2(-29.0 / 365); math.Abs(r["m2"].Credits-want) > 1e-12 {
		t.Errorf("29 days old: %+v, want credits %v", r["m2"], want)
	}
}

// A memory hidden on another machine is hidden here, unless this machine
// restored it after that hiding; the latest hiding of all the machines
// is the one compared, and a hiding of this machine stays as it is.
func TestLocalRestoreBeatsARemoteHiding(t *testing.T) {
	ten, eleven, twelve := sharedNow.Add(-2*time.Hour), sharedNow.Add(-time.Hour), sharedNow
	local := Ledger{Memories: map[string]Entry{
		"o1":    {Shown: 1, UnhiddenAt: eleven},
		"local": {Shown: 1, Hidden: &Hidden{Since: ten, Verdict: "wrong", Note: "n1"}},
	}}
	hiding := func(machine string, since time.Time, ids ...string) SharedDoc {
		d := SharedDoc{V: 1, Machine: machine, At: sharedNow, Entries: map[string]SharedEntry{}}
		for _, id := range ids {
			d.Entries[id] = SharedEntry{HiddenSince: since}
		}
		return d
	}
	view := func(docs ...SharedDoc) Ledger {
		return local.WithRemote(CombineRemote(docs, "pc-a", sharedNow, sharedParams), sharedParams, sharedNow)
	}

	got := view(hiding("pc-b", ten, "o1", "remote-only", "local"))
	if h := got.Memories["o1"].Hidden; h != nil {
		t.Errorf("hidden at 10:00 elsewhere, restored here at 11:00: hidden %+v", h)
	}
	if h := got.Memories["remote-only"].Hidden; h == nil || !h.Since.Equal(ten) {
		t.Errorf("hidden elsewhere, never seen here: %+v", got.Memories["remote-only"])
	}
	if h := got.Memories["local"].Hidden; h == nil || !h.Since.Equal(ten) || h.Verdict != "wrong" || h.Note != "n1" {
		t.Errorf("this machine's hiding: %+v", h)
	}

	got = view(hiding("pc-b", twelve, "o1"))
	if h := got.Memories["o1"].Hidden; h == nil || !h.Since.Equal(twelve) {
		t.Errorf("hidden at 12:00 elsewhere, restored here at 11:00: %+v", got.Memories["o1"])
	}
	got = view(hiding("pc-b", ten, "o1"), hiding("pc-c", twelve, "o1"))
	if h := got.Memories["o1"].Hidden; h == nil || !h.Since.Equal(twelve) {
		t.Errorf("hidden at 10:00 and 12:00 elsewhere: %+v", got.Memories["o1"])
	}
	if local.Memories["o1"].Hidden != nil || len(local.Memories) != 2 {
		t.Errorf("WithRemote hid the ledger's own entry: %+v", local.Memories["o1"])
	}
}
