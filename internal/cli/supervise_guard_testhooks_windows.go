//go:build windows && testhooks

package cli

import (
	"context"
	"io"

	"github.com/Barben360/backsight/internal/server"
)

// TEMPORARY, Task 10 deletes this file: the guard's stray scan reads
// /proc, which Windows lacks, so a real supervisor would retry it forever
// and never start the database. A testhooks build (TestTaskStartStopForReal
// runs one under the task) keeps the identity check only. A release has
// no testhooks tag.
func init() {
	superviseGuard = func(ctx context.Context, n server.Native, _ io.Writer) error { return n.CheckInstance(ctx) }
}
