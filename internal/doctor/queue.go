package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
)

// queueAgeLimit: a retain waiting on the server this long is stuck. A
// healthy server stores a session's part in seconds to minutes, and a
// backfill, which jobs tick paces, keeps the queue's oldest retain well
// under an hour. Six hours is a quarter of the day a session's pending
// check waits before it gives up repairing a part (retain.PendingLimit),
// so a warning seen within a day leaves time to act before a part is
// given up, while a slow model on a long backfill does not raise it.
const queueAgeLimit = 6 * time.Hour

// queuePages: the pages of the queue read per status (100 operations
// each).
const queuePages = 5

// checkQueueAge warns when the bank's oldest pending or processing retain
// is queueAgeLimit old or more (decision 0090): while it waits, the
// repair of a session's parts and the purge of a part landing after its
// session's purge both wait for it.
func checkQueueAge(ctx context.Context, cl *hindsight.Client, bank string, now time.Time) Check {
	c := Check{ID: "queue-age"}
	var oldest time.Time
	complete := true
	for _, status := range []string{"pending", "processing"} {
		ops, all, err := cl.QueuedOperationsOf(ctx, bank, status, queuePages)
		if err != nil {
			c.Status, c.Summary, c.Detail = Info, "cannot read the server's queue", err.Error()
			return c
		}
		complete = complete && all
		for _, op := range ops {
			if strings.Contains(op.Type, "retain") && !op.CreatedAt.IsZero() && (oldest.IsZero() || op.CreatedAt.Before(oldest)) {
				oldest = op.CreatedAt
			}
		}
	}
	age := now.Sub(oldest)
	if oldest.IsZero() || age < queueAgeLimit {
		c.Status, c.Summary = OK, "no retain waits on the server for long"
		return c
	}
	at := ""
	if !complete {
		at = "at least "
	}
	c.Status = Warn
	c.Summary = fmt.Sprintf("a retain has waited on the server for %s%d h", at, int(age.Hours()))
	c.Detail = "while the server's queue does not move, what was sent is not in memory yet, the repair of a session part another PC replaced or the server lost waits, and so does the purge of a part that lands after its session was purged"
	c.Fix = "on the server's host: backsight server status (is it running?), then backsight server llm check (a provider that keeps failing leaves operations queued); backsight server start restarts a stalled server"
	return c
}
