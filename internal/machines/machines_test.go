package machines

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const exampleJSON = `{"v":1,"machine":"3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6","at":1762394700,"os":"linux",` +
	`"first_seen":"2026-09-02","binary_version":"v0.1.30-m7","until":"2026-11-05",` +
	`"latency_edges_ms":[50,100,250,500,1000,2000,2500,5000],` +
	`"days":[{"day":"2026-11-05","prompts":41,"hit":[12,30],"used":[5,9],"coverage":[9,11],"wrong":[0,6],` +
	`"context":[72000,4],"flow":[3,3],"latency":[0,2,10,9,5,3,1,0,0],` +
	`"versions":{"v0.1.30-m7":{"prompts":41,"hit":[12,30],"used":[5,9],"coverage":[9,11],"wrong":[0,6],"context":[72000,4],"latency":[0,2,10,9,5,3,1,0,0]}}}]}`

const exampleName = DocPrefix + "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6"

var exampleFigures = Figures{
	Prompts: 41, Hit: Pair{12, 30}, Used: Pair{5, 9}, Coverage: Pair{9, 11}, Wrong: Pair{0, 6},
	Context: Pair{72000, 4}, Latency: Latency{0, 2, 10, 9, 5, 3, 1, 0, 0},
}

func exampleDoc() Doc {
	return Doc{
		Machine: "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6", At: time.Unix(1762394700, 0).UTC(), OS: "linux",
		FirstSeen: "2026-09-02", BinaryVersion: "v0.1.30-m7", Until: "2026-11-05",
		Days: []Day{{Day: "2026-11-05", Figures: exampleFigures, Flow: Pair{3, 3},
			Versions: map[string]Figures{"v0.1.30-m7": exampleFigures}}},
	}
}

func TestDocRoundTrip(t *testing.T) {
	got, err := exampleDoc().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if got != exampleJSON {
		t.Fatalf("marshal differs:\n got %s\nwant %s", got, exampleJSON)
	}
	// D's example has an `at` (2025-11-06) earlier than its `until` (2026-11-05), which Parse
	// refuses, so the parse-back runs on the same document published a year later.
	later := exampleDoc()
	later.At = time.Date(2026, 11, 6, 9, 5, 0, 0, time.UTC)
	s, _ := later.Marshal()
	back, err := Parse(exampleName, s, later.At.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, later) {
		t.Fatalf("round trip differs:\n got %+v\nwant %+v", back, later)
	}
	if _, err := Parse(exampleName, got, later.At); err == nil {
		t.Fatal("until after at's date must be refused")
	}
}

func TestDocWithoutVersionsOmitsThem(t *testing.T) {
	d := exampleDoc()
	d.Days[0].Versions = nil
	s, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "versions") {
		t.Fatalf("versions not omitted: %s", s)
	}
	d.Days = nil
	s, _ = d.Marshal()
	if !strings.Contains(s, `"days":[]`) {
		t.Fatalf("no days must be [] not null: %s", s)
	}
}

// freeText returns the fields of t (recursively) that could carry free text.
func freeText(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	named := map[string]bool{"Machine": true, "OS": true, "FirstSeen": true, "BinaryVersion": true, "Until": true, "Day": true}
	var bad []string
	if seen[t] {
		return nil
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		p := path + "." + f.Name
		ft := f.Type
		switch {
		case ft.Kind() == reflect.Int64 && ft.PkgPath() == "" && ft.Name() == "int64":
		case ft == reflect.TypeOf(time.Time{}), ft == reflect.TypeOf(Pair{}), ft == reflect.TypeOf(Latency{}):
		case ft.Kind() == reflect.String && ft.Name() == "string" && named[f.Name]:
		case ft == reflect.TypeOf(Figures{}):
			bad = append(bad, freeText(ft, p, seen)...)
		case ft == reflect.TypeOf([]Day{}):
			bad = append(bad, freeText(ft.Elem(), p, seen)...)
		case ft == reflect.TypeOf(map[string]Figures{}) && f.Name == "Versions":
			bad = append(bad, freeText(ft.Elem(), p, seen)...)
		default:
			bad = append(bad, p+" ("+ft.String()+")")
		}
	}
	return bad
}

func TestDocHoldsNoFreeText(t *testing.T) {
	if bad := freeText(reflect.TypeOf(Doc{}), "Doc", map[reflect.Type]bool{}); len(bad) != 0 {
		t.Fatalf("fields that could hold free text: %v", bad)
	}
}

func TestFreeTextGuardBites(t *testing.T) {
	for name, v := range map[string]any{
		"[]string":        struct{ Tags []string }{},
		"map[string]str":  struct{ M map[string]string }{},
		"any":             struct{ X any }{},
		"RawMessage":      struct{ R json.RawMessage }{},
		"unnamed string":  struct{ Note string }{},
		"int":             struct{ N int }{},
		"unknown version": struct{ Versions map[string]string }{},
	} {
		if bad := freeText(reflect.TypeOf(v), "T", map[reflect.Type]bool{}); len(bad) == 0 {
			t.Errorf("%s not caught", name)
		}
	}
}

func TestBucketEdgesAreUpperInclusive(t *testing.T) {
	for ms, want := range map[int64]int{0: 0, 50: 0, 51: 1, 100: 1, 250: 2, 500: 3, 1000: 4, 2000: 5, 2001: 6, 2500: 6, 2501: 7, 5000: 7, 5001: 8} {
		if got := Bucket(ms); got != want {
			t.Errorf("Bucket(%d) = %d, want %d", ms, got, want)
		}
	}
}

func TestRangeEdges(t *testing.T) {
	lo, hi := Range(0)
	if lo != 0 || hi == nil || *hi != 50 {
		t.Fatalf("bucket 0: %d %v", lo, hi)
	}
	lo, hi = Range(4)
	if lo != 500 || hi == nil || *hi != 1000 {
		t.Fatalf("bucket 4: %d %v", lo, hi)
	}
	lo, hi = Range(8)
	if lo != 5000 || hi != nil {
		t.Fatalf("bucket 8: %d %v", lo, hi)
	}
	*hi2(4) = 7 // a caller changing the returned edge must not change Edges
	if Edges[4] != 1000 {
		t.Fatal("Range leaked a pointer into Edges")
	}
}

func hi2(b int) *int64 { _, h := Range(b); return h }

func TestRankIsNearestRank(t *testing.T) {
	cases := []struct {
		b    [9]int64
		p    int
		want int
	}{
		{[9]int64{0, 0, 0, 0, 95, 5, 0, 0, 0}, 95, 4},
		{[9]int64{0, 0, 0, 0, 94, 6, 0, 0, 0}, 95, 5},
		{[9]int64{0, 0, 0, 0, 94, 5, 1, 0, 0}, 95, 5},
		{[9]int64{0, 0, 0, 0, 90, 10, 0, 0, 0}, 95, 5},
		{[9]int64{0, 0, 0, 0, 50, 50, 0, 0, 0}, 50, 4},
		{[9]int64{0, 0, 0, 0, 49, 51, 0, 0, 0}, 50, 5},
		{[9]int64{0, 0, 0, 0, 0, 0, 0, 0, 1}, 50, 8},
		{[9]int64{0, 0, 0, 0, 0, 0, 0, 0, 1}, 95, 8},
	}
	for _, c := range cases {
		got, ok := Rank(c.b, c.p)
		if !ok || got != c.want {
			t.Errorf("Rank(%v,%d) = %d,%v want %d", c.b, c.p, got, ok, c.want)
		}
	}
	if _, ok := Rank([9]int64{}, 95); ok {
		t.Error("all zero must be ok=false")
	}
	if _, hi := Range(8); hi != nil {
		t.Error("open bucket has no upper edge")
	}
}

func TestOS(t *testing.T) {
	for in, want := range map[string]string{"linux": "linux", "darwin": "darwin", "windows": "windows", "freebsd": "other", "": "other", "Linux": "other"} {
		if got := OS(in); got != want {
			t.Errorf("OS(%q) = %q want %q", in, got, want)
		}
	}
}

func TestValidVersionGuard(t *testing.T) {
	for _, s := range []string{"v0.1.30-m7", "1.2.3+build", "other", "a", strings.Repeat("a", 64),
		"0.1.27-m7", "v0.0.2-m0-93-gfd77ae6", "dev"} { // what a release build, make build and go build write
		if !ValidVersion(s) {
			t.Errorf("%q must be valid", s)
		}
	}
	for _, s := range []string{"(devel)", "v1 CANARY", "<script>", strings.Repeat("a", 65), "", ".1", "-1", "v1\n", "v1\x00"} {
		if ValidVersion(s) {
			t.Errorf("%q must be invalid", s)
		}
	}
}

func TestShortNeedsAValidName(t *testing.T) {
	cases := map[string]string{
		DocPrefix + "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6": "3f9c2a17",
		DocPrefix + "x": "",
		"backsight:feedback:3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6": "",
		DocPrefix + "3F9C2A17-1B2C-4D5E-8F90-A1B2C3D4E5F6":        "",
		DocPrefix + "3f9c2a17-1b2c-4d5e-8f90-a1b2c3d4e5f6x":       "",
		"": "",
	}
	for name, want := range cases {
		if got := Short(name); got != want {
			t.Errorf("Short(%q) = %q want %q", name, got, want)
		}
	}
}

func fitDoc() Doc {
	d := exampleDoc()
	d.Days = nil
	for _, day := range []string{"2026-11-03", "2026-11-04", "2026-11-05"} {
		d.Days = append(d.Days, Day{Day: day, Figures: exampleFigures, Flow: Pair{3, 3},
			Versions: map[string]Figures{"v0.1.30-m7": exampleFigures, "v0.1.29": exampleFigures}})
	}
	return d
}

func size(t *testing.T, d Doc) int {
	t.Helper()
	s, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return len(s)
}

func TestFitDropsVersionsThenOldestDays(t *testing.T) {
	d := fitDoc()
	full := size(t, d)

	got, dr, err := Fit(d, full)
	if err != nil || dr != (Dropped{}) || !reflect.DeepEqual(got, d) {
		t.Fatalf("fits as is: %v %+v", err, dr)
	}

	got, dr, err = Fit(d, full-1)
	if err != nil || dr != (Dropped{1, 0}) {
		t.Fatalf("limit-1: %v %+v", err, dr)
	}
	if got.Days[0].Versions != nil || got.Days[1].Versions == nil || got.Days[2].Versions == nil {
		t.Fatalf("only the oldest day loses versions: %+v", got.Days)
	}
	if d.Days[0].Versions == nil {
		t.Fatal("Fit mutated its input")
	}

	noVersions := d
	noVersions.Days = make([]Day, 3)
	for i, day := range d.Days {
		day.Versions = nil
		noVersions.Days[i] = day
	}
	nv := size(t, noVersions)
	got, dr, err = Fit(d, nv)
	if err != nil || dr != (Dropped{3, 0}) || len(got.Days) != 3 {
		t.Fatalf("limit=no versions: %v %+v", err, dr)
	}

	got, dr, err = Fit(d, nv-1)
	if err != nil || dr != (Dropped{3, 1}) || len(got.Days) != 2 || got.Days[0].Day != "2026-11-04" {
		t.Fatalf("one day less: %v %+v %v", err, dr, got.Days)
	}
	if got.Until != d.Until || got.Days[1].Day != "2026-11-05" {
		t.Fatal("until and the newest day must stay")
	}

	head := noVersions
	head.Days = nil
	one := noVersions
	one.Days = noVersions.Days[2:]
	got, dr, err = Fit(d, size(t, one))
	if err != nil || len(got.Days) != 1 || got.Days[0].Day != "2026-11-05" || dr != (Dropped{3, 2}) {
		t.Fatalf("newest day alone: %v %+v", err, dr)
	}
	if _, _, err = Fit(d, size(t, one)-1); err == nil {
		t.Fatal("a limit below the head and the newest day must be an error")
	}
	if _, _, err = Fit(d, size(t, head)-1); err == nil {
		t.Fatal("a limit below the head must be an error")
	}
}

func TestCacheAndRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCache(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing cache: %v", err)
	}
	if _, ok := LoadPublished(dir); ok {
		t.Fatal("missing record must read as none")
	}

	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := Cache{FetchedAt: at,
		Docs:    []CachedDoc{{Name: exampleName, Raw: exampleJSON}},
		Ignored: []Ignored{{Machine: "abcd1234", Reason: TooLarge}, {Machine: "", Reason: NotValid}}}
	if err := SaveCache(dir, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache(dir)
	if err != nil || !got.FetchedAt.Equal(at) || !reflect.DeepEqual(got.Docs, c.Docs) || !reflect.DeepEqual(got.Ignored, c.Ignored) {
		t.Fatalf("cache round trip: %v %+v", err, got)
	}

	p := Published{At: at, Server: "https://example.test", Until: "2026-11-05", FirstSeen: "2026-09-02", Bytes: 1234,
		FailedAt: at.Add(time.Hour), FailedSince: at, Failure: "server"}
	if err := SavePublished(dir, p); err != nil {
		t.Fatal(err)
	}
	gp, ok := LoadPublished(dir)
	if !ok || !gp.At.Equal(p.At) || !gp.FailedAt.Equal(p.FailedAt) || !gp.FailedSince.Equal(p.FailedSince) ||
		gp.Server != p.Server || gp.Until != p.Until || gp.FirstSeen != p.FirstSeen || gp.Bytes != p.Bytes || gp.Failure != p.Failure {
		t.Fatalf("record round trip: %v %+v", ok, gp)
	}

	if runtime.GOOS != "windows" {
		for _, f := range []string{CacheFile(dir), PublishedFile(dir)} {
			fi, err := os.Stat(f)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v %v", f, fi.Mode(), err)
			}
		}
	}
	if filepath.Dir(CacheFile(dir)) != dir || filepath.Base(CacheFile(dir)) != "machines.json" || filepath.Base(PublishedFile(dir)) != "published.json" {
		t.Fatal("file names")
	}
}

func TestPublishedJSONNames(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if err := SavePublished(dir, Published{At: at, Server: "s", Until: "2026-11-05", FirstSeen: "2026-09-02", Bytes: 5}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(PublishedFile(dir))
	want := `{"at":"2026-11-06T10:00:00Z","server":"s","until":"2026-11-05","first_seen":"2026-09-02","bytes":5}`
	if strings.TrimSpace(string(b)) != want {
		t.Fatalf("got %s want %s", b, want)
	}
	if err := SaveCache(dir, Cache{FetchedAt: at}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(CacheFile(dir))
	if strings.TrimSpace(string(b)) != `{"fetched_at":"2026-11-06T10:00:00Z","docs":[],"ignored":[]}` {
		t.Fatalf("cache json: %s", b)
	}
}

func TestCorruptRecordsReadAsNone(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"", "{", "[]", `{"at":"nope"}`} {
		if err := os.WriteFile(PublishedFile(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := LoadPublished(dir); ok {
			t.Errorf("%q must read as no record", body)
		}
		if err := os.WriteFile(CacheFile(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCache(dir); err == nil && body != `{"at":"nope"}` { // valid JSON of an unknown shape is an empty cache
			t.Errorf("corrupt cache %q must be an error", body)
		}
	}
}

func TestCacheOverTheCapIsRefusedFromItsSize(t *testing.T) {
	dir := t.TempDir()
	f := CacheFile(dir)
	// A sparse file one byte over the cap, made of a valid JSON prefix: were it read, only the size check could refuse it.
	if err := os.WriteFile(f, []byte(`{"fetched_at":"2026-11-06T10:00:00Z","docs":[],"ignored":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(f, MaxCacheBytes+1); err != nil {
		t.Skipf("sparse file unsupported: %v", err)
	}
	_, err := LoadCache(dir)
	if !errors.Is(err, ErrCacheTooLarge) {
		t.Fatalf("want ErrCacheTooLarge, got %v", err)
	}
	if err := os.Truncate(f, MaxCacheBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache(dir); errors.Is(err, ErrCacheTooLarge) {
		t.Fatal("exactly the cap is not over it")
	}
}

func TestReadCappedRefusesALongRead(t *testing.T) {
	if b, err := readCapped(strings.NewReader("12345"), 5); err != nil || string(b) != "12345" {
		t.Errorf("exactly the cap: %q, %v", b, err)
	}
	if _, err := readCapped(strings.NewReader("123456"), 5); !errors.Is(err, ErrCacheTooLarge) {
		t.Errorf("one over the cap: %v, want ErrCacheTooLarge", err)
	}
}

func TestCacheReadsDuringSave(t *testing.T) {
	dir := t.TempDir()
	c := Cache{FetchedAt: time.Now().UTC(), Docs: []CachedDoc{{Name: exampleName, Raw: strings.Repeat("x", 100_000)}}}
	if err := SaveCache(dir, c); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var failure error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := LoadCache(dir); err != nil {
				failure = err
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		if err := SaveCache(dir, c); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if failure != nil {
		t.Fatalf("a reader failed during a save: %v", failure)
	}
}
