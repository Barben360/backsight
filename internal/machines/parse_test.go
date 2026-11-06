package machines

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var parseNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

const baseID = "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6"

// baseDoc is a valid document: at 2026-11-06 11:00 UTC, two days with a version row each.
func baseDoc() Doc {
	d := exampleDoc()
	d.At = time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	d.Days = []Day{
		{Day: "2026-11-04", Figures: exampleFigures, Flow: Pair{3, 3}, Versions: map[string]Figures{"v0.1.30-m7": exampleFigures}},
		{Day: "2026-11-05", Figures: exampleFigures, Flow: Pair{3, 3}, Versions: map[string]Figures{"v0.1.30-m7": exampleFigures}},
	}
	return d
}

func baseJSON(t testing.TB) string {
	t.Helper()
	s, err := baseDoc().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reasonOf(t *testing.T, err error) Reason {
	t.Helper()
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ParseError, got %T %v", err, err)
	}
	return pe.Reason
}

// edit applies f to the generic form of the base document.
func edit(f func(m map[string]any)) func(string) string {
	return func(s string) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			panic(err)
		}
		f(m)
		b, err := json.Marshal(m)
		if err != nil {
			panic(err)
		}
		return string(b)
	}
}

func day(m map[string]any) map[string]any { return m["days"].([]any)[1].(map[string]any) }
func ver(m map[string]any) map[string]any {
	return day(m)["versions"].(map[string]any)["v0.1.30-m7"].(map[string]any)
}

func replace(old, new string) func(string) string {
	return func(s string) string {
		if strings.Count(s, old) < 1 {
			panic("base lacks " + old)
		}
		return strings.Replace(s, old, new, 1)
	}
}

func TestParseRefusesHostileDocuments(t *testing.T) {
	if _, err := Parse(DocPrefix+baseID, baseJSON(t), parseNow); err != nil {
		t.Fatalf("the base must parse: %v", err)
	}
	upper := strings.ToUpper(baseID)
	nine := func(m map[string]any) {
		vs := map[string]any{}
		for i := 0; i < 9; i++ {
			vs[fmt.Sprintf("v%d", i)] = ver(m)
		}
		day(m)["versions"] = vs
	}
	rows := []struct {
		name string
		id   string // document id suffix; baseID when empty
		mut  func(string) string
		want Reason
	}{
		{"array", "", func(string) string { return `[]` }, NotValid},
		{"string", "", func(string) string { return `"x"` }, NotValid},
		{"null", "", func(string) string { return `null` }, NotValid},
		{"empty", "", func(string) string { return `` }, NotValid},
		{"missing v", "", edit(func(m map[string]any) { delete(m, "v") }), NotValid},
		{"machine differs from name", "", edit(func(m map[string]any) { m["machine"] = "00000000-0000-4000-8000-000000000000" }), NotValid},
		{"machine upper case", upper, edit(func(m map[string]any) { m["machine"] = upper }), NotValid},
		{"machine not a uuid", "x", edit(func(m map[string]any) { m["machine"] = "x" }), NotValid},
		{"at zero", "", edit(func(m map[string]any) { m["at"] = 0 }), NotValid},
		{"at before 2020", "", edit(func(m map[string]any) { m["at"] = 1577836799 }), NotValid},
		{"os beos", "", edit(func(m map[string]any) { m["os"] = "beos" }), NotValid},
		{"os upper", "", edit(func(m map[string]any) { m["os"] = "Linux" }), NotValid},
		{"edges other", "", edit(func(m map[string]any) {
			m["latency_edges_ms"] = []int{50, 100, 250, 500, 1000, 2000, 2500, 5001}
		}), NotValid},
		{"edges short", "", edit(func(m map[string]any) { m["latency_edges_ms"] = []int{50, 100} }), NotValid},
		{"first_seen not a date", "", edit(func(m map[string]any) { m["first_seen"] = "yesterday" }), NotValid},
		{"first_seen impossible date", "", edit(func(m map[string]any) { m["first_seen"] = "2026-02-30" }), NotValid},
		{"until not a date", "", edit(func(m map[string]any) { m["until"] = "2026-11-5" }), NotValid},
		{"binary_version free text", "", edit(func(m map[string]any) { m["binary_version"] = "v1 CANARY" }), NotValid},
		{"binary_version missing", "", edit(func(m map[string]any) { delete(m, "binary_version") }), NotValid},
		{"fraction", "", replace(`"prompts":41`, `"prompts":1.5`), NotValid},
		{"negative", "", replace(`"prompts":41`, `"prompts":-1`), NotValid},
		{"exponent beyond int64", "", replace(`"prompts":41`, `"prompts":1e300`), NotValid},
		{"over MaxCount", "", replace(`"prompts":41`, `"prompts":1000000001`), NotValid},
		{"pair over MaxCount", "", replace(`"hit":[12,30]`, `"hit":[1000000001,1000000001]`), NotValid},
		{"NaN", "", replace(`"prompts":41`, `"prompts":NaN`), NotValid},
		{"Infinity", "", replace(`"prompts":41`, `"prompts":Infinity`), NotValid},
		{"string number", "", replace(`"prompts":41`, `"prompts":"41"`), NotValid},
		{"num above den", "", edit(func(m map[string]any) { day(m)["hit"] = []int{31, 30} }), NotValid},
		{"coverage num above den", "", edit(func(m map[string]any) { day(m)["coverage"] = []int{12, 11} }), NotValid},
		{"flow num above den", "", edit(func(m map[string]any) { day(m)["flow"] = []int{4, 3} }), NotValid},
		{"latency of 8", "", edit(func(m map[string]any) { day(m)["latency"] = []int{0, 2, 10, 9, 5, 3, 1, 0} }), NotValid},
		{"latency of 10", "", edit(func(m map[string]any) { day(m)["latency"] = []int{0, 2, 10, 9, 5, 3, 1, 0, 0, 0} }), NotValid},
		{"latency null element", "", replace(`"latency":[0,2,10`, `"latency":[null,2,10`), NotValid},
		{"latency count over MaxCount", "", replace(`"latency":[0,2,10`, `"latency":[1000000001,2,10`), NotValid},
		{"hit of one", "", edit(func(m map[string]any) { day(m)["hit"] = []int{1} }), NotValid},
		{"hit of three", "", edit(func(m map[string]any) { day(m)["hit"] = []int{1, 2, 3} }), NotValid},
		{"hit null element", "", replace(`"hit":[12,30]`, `"hit":[null,30]`), NotValid},
		{"hit null", "", edit(func(m map[string]any) { day(m)["hit"] = nil }), NotValid},
		{"hit missing", "", edit(func(m map[string]any) { delete(day(m), "hit") }), NotValid},
		{"flow missing", "", edit(func(m map[string]any) { delete(day(m), "flow") }), NotValid},
		{"prompts missing", "", edit(func(m map[string]any) { delete(day(m), "prompts") }), NotValid},
		{"day null", "", edit(func(m map[string]any) { m["days"] = []any{nil} }), NotValid},
		{"day without a date", "", edit(func(m map[string]any) { delete(day(m), "day") }), NotValid},
		{"day not a date", "", edit(func(m map[string]any) { day(m)["day"] = "2026-13-01" }), NotValid},
		{"versions value null", "", edit(func(m map[string]any) { day(m)["versions"] = map[string]any{"v0.1.30-m7": nil} }), NotValid},
		{"versions not an object", "", edit(func(m map[string]any) { day(m)["versions"] = "x" }), NotValid},
		{"days repeated", "", edit(func(m map[string]any) { day(m)["day"] = "2026-11-04" }), NotValid},
		{"days descending", "", edit(func(m map[string]any) {
			d := m["days"].([]any)
			d[0], d[1] = d[1], d[0]
		}), NotValid},
		{"day after until", "", edit(func(m map[string]any) { day(m)["day"] = "2026-11-06" }), NotValid},
		{"nine versions", "", edit(nine), NotValid},
		{"version key script", "", edit(func(m map[string]any) {
			v := day(m)["versions"].(map[string]any)
			v["<script>"] = v["v0.1.30-m7"]
		}), NotValid},
		{"version prompts above the day", "", edit(func(m map[string]any) { ver(m)["prompts"] = 42 }), NotValid},
		{"version hit num above the day", "", edit(func(m map[string]any) { ver(m)["hit"] = []int{13, 30} }), NotValid},
		{"version hit den above the day", "", edit(func(m map[string]any) { ver(m)["hit"] = []int{12, 31} }), NotValid},
		{"version context above the day", "", edit(func(m map[string]any) { ver(m)["context"] = []int{72001, 4} }), NotValid},
		{"version latency above the day", "", edit(func(m map[string]any) { ver(m)["latency"] = []int{0, 3, 10, 9, 5, 3, 1, 0, 0} }), NotValid},
		{"version hit of one", "", edit(func(m map[string]any) { ver(m)["hit"] = []int{1} }), NotValid},
		{"days missing", "", edit(func(m map[string]any) { delete(m, "days") }), NotValid},
		{"days null", "", edit(func(m map[string]any) { m["days"] = nil }), NotValid},
		{"until missing", "", edit(func(m map[string]any) { delete(m, "until") }), NotValid},
		{"edges missing", "", edit(func(m map[string]any) { delete(m, "latency_edges_ms") }), NotValid},
		{"os missing", "", edit(func(m map[string]any) { delete(m, "os") }), NotValid},
		{"at missing", "", edit(func(m map[string]any) { delete(m, "at") }), NotValid},
		{"machine missing", "", edit(func(m map[string]any) { delete(m, "machine") }), NotValid},
		{"first_seen missing", "", edit(func(m map[string]any) { delete(m, "first_seen") }), NotValid},
		{"trailing value", "", func(s string) string { return s + s }, NotValid},
		{"trailing garbage", "", func(s string) string { return s + " x" }, NotValid},
		{"truncated", "", func(s string) string { return s[:len(s)-1] }, NotValid},
		{"name without the prefix", "", func(s string) string { return s }, NotValid}, // id overridden below
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			name := DocPrefix + baseID
			if r.id != "" {
				name = DocPrefix + r.id
			}
			if r.name == "name without the prefix" {
				name = "backsight:feedback:" + baseID
			}
			_, err := Parse(name, r.mut(baseJSON(t)), parseNow)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := reasonOf(t, err); got != r.want {
				t.Fatalf("reason %q want %q (%v)", got, r.want, errors.Unwrap(err))
			}
		})
	}
}

func TestParseAcceptsTheEdgesOfTheCaps(t *testing.T) {
	// MaxCount itself, the earliest allowed day (until - 399 d) and exactly 400 days.
	d := baseDoc()
	d.Days = nil
	until := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for i := MaxDays - 1; i >= 0; i-- {
		f := exampleFigures
		f.Prompts = MaxCount
		f.Hit = Pair{MaxCount, MaxCount}
		d.Days = append(d.Days, Day{Day: until.AddDate(0, 0, -i).Format("2006-01-02"), Figures: f, Flow: Pair{0, 0}})
	}
	s, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) > MaxBytes {
		t.Fatalf("test document too large: %d", len(s))
	}
	got, err := Parse(DocPrefix+baseID, s, parseNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Days) != 400 || got.Days[0].Day != "2025-10-02" {
		t.Fatalf("%d days from %s", len(got.Days), got.Days[0].Day)
	}
}

func TestParseRefusesOverTheCaps(t *testing.T) {
	_, err := Parse(DocPrefix+baseID, "{"+strings.Repeat(" ", MaxBytes), parseNow)
	if r := reasonOf(t, err); r != TooLarge {
		t.Fatalf("got %q", r)
	}
	// MaxBytes exactly is not too large: it is read (and fails as invalid JSON, not as too large).
	_, err = Parse(DocPrefix+baseID, "{"+strings.Repeat(" ", MaxBytes-1), parseNow)
	if r := reasonOf(t, err); r != NotValid {
		t.Fatalf("exactly MaxBytes got %q", r)
	}

	d := baseDoc()
	d.Days = nil
	until := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for i := MaxDays; i >= 0; i-- { // 401 days
		d.Days = append(d.Days, Day{Day: until.AddDate(0, 0, -i).Format("2006-01-02"), Figures: exampleFigures, Flow: Pair{3, 3}})
	}
	s, _ := d.Marshal()
	_, err = Parse(DocPrefix+baseID, s, parseNow)
	if r := reasonOf(t, err); r != NotValid {
		t.Fatalf("401 days got %q", r)
	}

	// A single day one before until - 399 d, the count being fine.
	d = baseDoc()
	d.Days = d.Days[:1]
	d.Days[0].Day = "2025-10-01"
	s, _ = d.Marshal()
	_, err = Parse(DocPrefix+baseID, s, parseNow)
	if r := reasonOf(t, err); r != NotValid {
		t.Fatalf("day before the window got %q", r)
	}
	d.Days[0].Day = "2025-10-02"
	s, _ = d.Marshal()
	if _, err = Parse(DocPrefix+baseID, s, parseNow); err != nil {
		t.Fatalf("until - 399 d must be accepted: %v", err)
	}
}

func TestParseNesting(t *testing.T) {
	nested := func(n int) string {
		b := baseJSON(t)
		return `{"x":` + strings.Repeat("[", n) + strings.Repeat("]", n) + "," + b[1:]
	}
	s := nested(9990)
	if len(s) > MaxBytes {
		t.Fatal("test document too large")
	}
	d, err := Parse(DocPrefix+baseID, s, parseNow)
	if err != nil {
		t.Fatalf("9990 deep: %v", err)
	}
	if len(d.Days) != 2 {
		t.Fatalf("the rest must be read: %+v", d)
	}
	s = nested(10000)
	_, err = Parse(DocPrefix+baseID, s, parseNow)
	if err == nil {
		t.Fatal("10000 deep accepted")
	}
	if r := reasonOf(t, err); r != NotValid {
		t.Fatalf("10000 deep: %q", r)
	}
}

func TestParseRefusesFutureDocuments(t *testing.T) {
	at := func(a time.Time, until string) string {
		d := baseDoc()
		d.At = a
		d.Until = until
		s, _ := d.Marshal()
		return s
	}
	if _, err := Parse(DocPrefix+baseID, at(parseNow.Add(23*time.Hour), "2026-11-06"), parseNow); err != nil {
		t.Fatalf("now+23h: %v", err)
	}
	_, err := Parse(DocPrefix+baseID, at(parseNow.Add(25*time.Hour), "2026-11-06"), parseNow)
	if r := reasonOf(t, err); r != NotValid {
		t.Fatalf("now+25h: %q", r)
	}
	// until after at's UTC date (at 2026-11-06 11:00 UTC; until 2026-11-06 is the same date: refused as it is not the day before at, but is it "after"? no).
	_, err = Parse(DocPrefix+baseID, at(baseDoc().At, "2026-11-07"), parseNow)
	if r := reasonOf(t, err); r != NotValid {
		t.Fatalf("until after at: %q", r)
	}
	// until on at's own date is not after it.
	if _, err := Parse(DocPrefix+baseID, at(baseDoc().At, "2026-11-06"), parseNow.Add(48*time.Hour)); err != nil {
		t.Fatalf("until = at's date: %v", err)
	}
}

func TestUnknownVersionIsIgnoredWithItsReason(t *testing.T) {
	for _, s := range []string{
		`{"v":2,"fields":{"this":"binary does not know"}}`,
		`{"v":2}`,
		`{"v":2,"machine":5,"days":"x"}`,
		`{"v":0}`,
		`{"v":99999999999}`,
	} {
		_, err := Parse(DocPrefix+baseID, s, parseNow)
		if r := reasonOf(t, err); r != UnknownVersion {
			t.Errorf("%s: %q", s, r)
		}
	}
	for _, s := range []string{`{"v":"1"}`, `{"v":1.5}`, `{"v":null}`, `{"v":2`, `{"v":2}{"v":2}`, `{}`} {
		_, err := Parse(DocPrefix+baseID, s, parseNow)
		if r := reasonOf(t, err); r != NotValid {
			t.Errorf("%s: %q", s, r)
		}
	}
	// v 1 with unknown fields (also nested, also in a day and a version row): ignored.
	got, err := Parse(DocPrefix+baseID, edit(func(m map[string]any) {
		m["new_field"] = map[string]any{"a": []any{1, "x"}}
		day(m)["extra"] = "text"
		ver(m)["flow"] = []int{9, 9}
	})(baseJSON(t)), parseNow)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := baseDoc().Marshal()
	if again, _ := got.Marshal(); again != want {
		t.Fatalf("unknown fields leaked:\n%s\n%s", again, want)
	}
}

func TestParseErrorNeverShowsItsCause(t *testing.T) {
	_, err := Parse(DocPrefix+baseID, `{"v":1,"machine":"SECRET-TEXT"`, parseNow)
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("error text must carry the reason only: %v", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("Err must be kept for tests and logs")
	}
}

func TestPairAndLatencyUnmarshal(t *testing.T) {
	var p Pair
	for _, s := range []string{`[1]`, `[1,2,3]`, `[]`, `null`, `[1,null]`, `[1.5,2]`, `"a"`, `{}`} {
		if err := json.Unmarshal([]byte(s), &p); err == nil {
			t.Errorf("Pair accepted %s", s)
		}
	}
	if err := json.Unmarshal([]byte(`[3,4]`), &p); err != nil || p != (Pair{3, 4}) {
		t.Fatalf("%v %v", err, p)
	}
	var l Latency
	for _, s := range []string{`[1,2,3,4,5,6,7,8]`, `[1,2,3,4,5,6,7,8,9,10]`, `[1,2,3,4,5,6,7,8,null]`, `null`} {
		if err := json.Unmarshal([]byte(s), &l); err == nil {
			t.Errorf("Latency accepted %s", s)
		}
	}
	if err := json.Unmarshal([]byte(`[1,2,3,4,5,6,7,8,9]`), &l); err != nil || l[8] != 9 {
		t.Fatalf("%v %v", err, l)
	}
}
