package index

import (
	"database/sql"
	"regexp"
	"testing"
)

func TestSQLiteDriverIsLinked(t *testing.T) {
	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&v); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !regexp.MustCompile(`^3\.\d+\.\d+`).MatchString(v) {
		t.Fatalf("sqlite_version() = %q, want 3.x.y", v)
	}
	t.Logf("sqlite_version=%s", v)
}
