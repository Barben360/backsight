package index

import (
	"io"

	"github.com/Barben360/backsight/internal/sqlitedb"
)

// parseMountInfo returns the filesystem type of the longest mount point
// holding dir, from /proc/self/mountinfo lines (sqlitedb.ParseMountInfo).
func parseMountInfo(r io.Reader, dir string) string { return sqlitedb.ParseMountInfo(r, dir) }
