package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
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

func (f fakeDB) Dump(_ context.Context, dst string) error {
	return os.WriteFile(dst, []byte(f.dump), 0o600)
}

func TestCreateOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	at := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	path, m, err := Create(context.Background(), fakeDB{dump: "PGDMP fake dump"}, dir, Manifest{CreatedAt: at, Bank: "claude-memory", Recipients: []string{id.Recipient().String()}},
		map[string][]byte{"secrets.toml": []byte("api_key = \"s3cr3t-value\"\n"), "config.toml": []byte("schema_version = 1\n")}, []age.Recipient{id.Recipient()})
	if err != nil || filepath.Base(path) != "backsight-20261025T040000Z.tar.gz.age" || m.Counts["memory_units"] != 2204 || m.Alembic != "c7e2f9a4d1b8" {
		t.Fatalf("%s %+v %v", path, m, err)
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
	got, err := Open(path, []age.Identity{id}, out)
	if err != nil || got.DumpSHA256 != m.DumpSHA256 || got.Counts["memory_units"] != 2204 {
		t.Fatalf("%+v %v", got, err)
	}
	if s, _ := os.ReadFile(filepath.Join(out, "secrets.toml")); !strings.Contains(string(s), "s3cr3t-value") {
		t.Error("secrets not in the archive")
	}
	other, _ := age.GenerateX25519Identity()
	if _, err := Open(path, []age.Identity{other}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "identity import") {
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
			name := Name(start.AddDate(0, 0, -d).Add(time.Duration(h) * time.Hour))
			_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "mine.tar.gz.age"), []byte("x"), 0o600)
	removed, err := Prune(dir, 7, 4)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := List(dir)
	if len(left)+len(removed) != 80 {
		t.Fatalf("%d left, %d removed", len(left), len(removed))
	}
	// 7 days kept, plus the newest of the 3 earlier weeks the 4-week window adds
	if len(left) != 10 {
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
	if b := Before(dir, start); len(b) != 9 {
		t.Errorf("before: %d", len(b))
	}
}
