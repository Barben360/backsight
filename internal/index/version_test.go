package index

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFileVersionSeesAnotherBinary: a version written by another
// connection (another binary migrating the file) is what FileVersion
// reads, while Schema keeps the version Open left.
func TestFileVersionSeesAnotherBinary(t *testing.T) {
	ix, _ := openT(t)
	ctx := context.Background()
	head := ix.Schema()
	if v, err := ix.FileVersion(ctx); err != nil || v != head {
		t.Fatalf("FileVersion before = %d, %v; want %d", v, err, head)
	}
	db, err := sql.Open(driverName, dsn(ix.Path(), ix.FS(), false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", head+1)); err != nil {
		t.Fatal(err)
	}
	if v, err := ix.FileVersion(ctx); err != nil || v != head+1 {
		t.Errorf("FileVersion = %d, %v; want %d", v, err, head+1)
	}
	if got := ix.Schema(); got != head {
		t.Errorf("Schema = %d, want %d", got, head)
	}
}
