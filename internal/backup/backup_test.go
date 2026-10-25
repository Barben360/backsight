package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/Barben360/backsight/internal/state"
)

type fakeDB struct{ dump string }

func (f fakeDB) Query(_ context.Context, sql string) (string, error) {
	switch {
	case strings.Contains(sql, "alembic"):
		return "c7e2f9a4d1b8", nil
	case strings.Contains(sql, "pg_extension"):
		return "0.8.7", nil
	case strings.Contains(sql, "memory_units"):
		return "2204", nil
	}
	return "7", nil
}

func (f fakeDB) Dump(_ context.Context, dst string, tables []string) (map[string]int64, error) {
	counts := map[string]int64{}
	for _, t := range tables {
		counts[t] = 7
	}
	counts["memory_units"] = 2204
	return counts, os.WriteFile(dst, []byte(f.dump), 0o600)
}

func TestCreateOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	at := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	staging := t.TempDir()
	dumped := false
	path, m, err := Create(context.Background(), fakeDB{dump: "PGDMP fake dump"}, dir, staging, Manifest{CreatedAt: at, Bank: "claude-memory", Recipients: []string{id.Recipient().String()}},
		map[string][]byte{"secrets.toml": []byte("api_key = \"s3cr3t-value\"\n"), "config.toml": []byte("schema_version = 1\n")}, []age.Recipient{id.Recipient()}, func() { dumped = true })
	if err != nil || filepath.Base(path) != "backsight-20261025T040000Z.tar.gz.age" || m.Counts["memory_units"] != 2204 || m.Alembic != "c7e2f9a4d1b8" || !dumped {
		t.Fatalf("%s %+v %v", path, m, err)
	}
	// only ciphertext in the backup directory; nothing left in staging
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Errorf("backup dir: %v", ents)
	}
	if ents, _ := os.ReadDir(staging); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	raw, _ := os.ReadFile(path)
	side, _ := os.ReadFile(path + ".json")
	if strings.Contains(string(raw), "s3cr3t-value") || strings.Contains(string(raw), "PGDMP") || strings.Contains(string(side), "s3cr3t") {
		t.Error("archive or sidecar in clear")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	out := t.TempDir()
	got, err := Open(path, []age.Identity{id}, out, nil)
	if err != nil || got.DumpSHA256 != m.DumpSHA256 || got.Counts["memory_units"] != 2204 {
		t.Fatalf("%+v %v", got, err)
	}
	if s, _ := os.ReadFile(filepath.Join(out, "secrets.toml")); !strings.Contains(string(s), "s3cr3t-value") {
		t.Error("secrets not in the archive")
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := Open(path, []age.Identity{other}, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "identity import") {
		t.Errorf("a wrong identity: %v", err)
	}
	if list, _ := List(dir); len(list) != 1 || list[0].Manifest.Bank != "claude-memory" {
		t.Errorf("list %+v", list)
	}
}

func TestRecipients(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	if r, err := Recipients([]string{" " + id.Recipient().String() + " ", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHsKLqeplhpW+uObz5dvMgjz1OxfM/XXUB+VHtZ6isGN"}); err != nil || len(r) != 2 {
		t.Errorf("%v %v", r, err)
	}
	if _, err := Recipients([]string{"not-a-key"}); err == nil {
		t.Error("a bad recipient is refused")
	}
	if _, err := Recipients(nil); err == nil {
		t.Error("no recipient is refused")
	}
}

// 7 daily + 4 weekly: the newest of each of the last 7 days and of each of
// the last 4 ISO weeks stay; files not named like backups are never touched.
func TestPruneKeepsDailyAndWeekly(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC) // a Sunday
	for d := 0; d < 40; d++ {
		for _, h := range []int{0, 12} {
			at := start.AddDate(0, 0, -d).Add(time.Duration(h) * time.Hour)
			name := Name(at)
			_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
			_ = os.WriteFile(filepath.Join(dir, name+".json"), []byte(`{"format":1,"created_at":"`+at.Format(time.RFC3339)+`"}`), 0o600)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "mine.tar.gz.age"), []byte("x"), 0o600)
	// a copy brought from elsewhere, named like ours but with no sidecar
	_ = os.WriteFile(filepath.Join(dir, Name(start.AddDate(-1, 0, 0))), []byte("x"), 0o600)
	removed, err := Prune(dir, 7, 4)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := List(dir)
	if len(left)+len(removed) != 81 {
		t.Fatalf("%d left, %d removed", len(left), len(removed))
	}
	// 7 days kept, plus the newest of the 3 earlier weeks the 4-week window
	// adds, plus the copy that is not ours
	if len(left) != 11 {
		var names []string
		for _, e := range left {
			names = append(names, filepath.Base(e.Path))
		}
		t.Errorf("%d kept: %v", len(left), names)
	}
	if left[0].Manifest.CreatedAt != start.Add(12*time.Hour) {
		t.Errorf("the newest is not kept: %v", left[0].Manifest.CreatedAt)
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.tar.gz.age")); err != nil {
		t.Error("a foreign file was removed")
	}
	// the newest of each day and of each week: W42 (Oct 12-18) keeps Oct 18
	// 16:00, not that day's 04:00 nor an earlier day
	var kept []string
	for _, e := range left {
		if e.Ours {
			kept = append(kept, e.Manifest.CreatedAt.Format("01-02T15"))
		}
	}
	want := "10-25T16 10-24T16 10-23T16 10-22T16 10-21T16 10-20T16 10-19T16 10-18T16 10-11T16 10-04T16"
	if strings.Join(kept, " ") != want {
		t.Errorf("kept %v, want %s", kept, want)
	}
	// one sidecar per archive left, none orphaned
	ents, _ := os.ReadDir(dir)
	sidecars := 0
	for _, e := range ents {
		if archive, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			sidecars++
			if _, err := os.Stat(filepath.Join(dir, archive)); err != nil {
				t.Errorf("orphan sidecar %s", e.Name())
			}
		}
	}
	for _, e := range left {
		if _, err := os.Stat(e.Path + ".json"); e.Ours && err != nil {
			t.Errorf("no sidecar for %s", filepath.Base(e.Path))
		}
	}
	if sidecars != len(kept) {
		t.Errorf("%d sidecars for %d archives", sidecars, len(kept))
	}
	if b := Before(dir, start); len(b) != 10 {
		t.Errorf("before: %d", len(b))
	}
}

// From the moment the database lock is released, the archive being
// encrypted is on disk: a purge then lists it (by its final path) among the
// backups that still hold what it deletes.
func TestBeforeListsAnArchiveInProgress(t *testing.T) {
	dir := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	at := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	final := filepath.Join(dir, Name(at))
	var during []Entry
	_, _, err := Create(context.Background(), fakeDB{dump: "PGDMP"}, dir, t.TempDir(), Manifest{CreatedAt: at}, nil, []age.Recipient{id.Recipient()},
		func() { during = Before(dir, at.Add(time.Minute)) })
	if err != nil {
		t.Fatal(err)
	}
	if len(during) != 1 || during[0].Path != final {
		t.Errorf("before, while encrypting: %+v", during)
	}
	if after := Before(dir, at.Add(time.Minute)); len(after) != 1 || after[0].Path != final {
		t.Errorf("before, once written: %+v", after)
	}
	if b := Before(dir, at); len(b) != 0 {
		t.Errorf("an archive taken at t is not before t: %+v", b)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// A killed verify leaves its clear dump in staging: the next backup or
// verify removes it, never the directory of a verify still running.
func TestDeadVerifiesAreRemoved(t *testing.T) {
	staging := t.TempDir()
	dead := map[string]string{
		"verify-4242":        "", // no lock: an older layout, or killed between its steps
		"verify-4243-777777": `{"pid":` + strconv.Itoa(deadPID(t)) + `,"command":"backup verify","since":"2026-10-25T03:00:00Z"}`,
	}
	plant := func() {
		for name, lock := range dead {
			_ = os.MkdirAll(filepath.Join(staging, name), 0o700)
			_ = os.WriteFile(filepath.Join(staging, name, "db.dump"), []byte("PGDMP clear"), 0o600)
			if lock != "" {
				_ = os.WriteFile(filepath.Join(staging, name+".lock"), []byte(lock), 0o600)
			}
		}
	}
	plant()
	running, done, err := VerifyDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(running, "db.dump"), []byte("PGDMP clear"), 0o600)
	for name := range dead {
		if _, err := os.Stat(filepath.Join(staging, name)); err == nil {
			t.Errorf("verify left %s", name)
		}
	}
	plant()
	id, _ := age.GenerateX25519Identity()
	if _, _, err := Create(context.Background(), fakeDB{dump: "PGDMP"}, t.TempDir(), staging, Manifest{CreatedAt: time.Now()}, nil, []age.Recipient{id.Recipient()}, nil); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(staging)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if want := filepath.Base(running) + " " + filepath.Base(running) + ".lock"; strings.Join(names, " ") != want {
		t.Errorf("staging after create: %v, want %s", names, want)
	}
	if h := state.Inspect(running + ".lock"); h == nil || h.Stale {
		t.Errorf("the running verify's lock: %+v", h)
	}
	done()
	if ents, _ := os.ReadDir(staging); len(ents) != 0 {
		t.Errorf("staging after done: %v", ents)
	}
}

type entry struct {
	name, body string
	typ        byte
}

// forge writes an archive with arbitrary entries, encrypted to id.
func forge(t *testing.T, id *age.X25519Identity, entries []entry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forged.tar.gz.age")
	f, _ := os.Create(path)
	enc, _ := age.Encrypt(f, id.Recipient())
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Typeflag: typ, Linkname: "/etc/passwd"})
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = enc.Close()
	_ = f.Close()
	return path
}

func manifestJSON(format int, dump string) string {
	sum := sha256.Sum256([]byte(dump))
	return `{"format":` + strconv.Itoa(format) + `,"dump_sha256":"` + hex.EncodeToString(sum[:]) + `"}`
}

// Open refuses what a forged or damaged archive could carry.
func TestOpenRefusesHostileArchives(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	ok := []entry{{name: "manifest.json", body: manifestJSON(1, "PGDMP")}, {name: "db.dump", body: "PGDMP"}}
	if _, err := Open(forge(t, id, ok), []age.Identity{id}, t.TempDir(), nil); err != nil {
		t.Fatalf("a well-formed archive: %v", err)
	}
	cases := map[string][]entry{
		"a path outside the directory": append(ok[:1:1], entry{name: "../evil", body: "x"}, ok[1]),
		"a subdirectory":               append(ok[:1:1], entry{name: "sub/evil", body: "x"}, ok[1]),
		"a symlink":                    append(ok[:1:1], entry{name: "link", typ: tar.TypeSymlink}, ok[1]),
		"an unknown format":            {{name: "manifest.json", body: manifestJSON(99, "PGDMP")}, ok[1]},
		"a dump that is not the one the manifest names": {ok[0], {name: "db.dump", body: "PGDMP tampered"}},
	}
	for name, entries := range cases {
		dir := t.TempDir()
		if _, err := Open(forge(t, id, entries), []age.Identity{id}, dir, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Errorf("%s: wrote outside the directory", name)
		}
	}
	// want leaves the other entries unwritten
	dir := t.TempDir()
	withSecrets := append(append([]entry{}, ok...), entry{name: "secrets.toml", body: "api_key = 'x'"})
	if _, err := Open(forge(t, id, withSecrets), []age.Identity{id}, dir, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.toml")); err == nil {
		t.Error("an entry want refused was written")
	}
}
