package cli

// Links the index package (and its SQLite driver) into the binary so the
// dependency gate measures its real size; removed once the index is really
// imported.
import _ "github.com/Barben360/backsight/internal/index"
