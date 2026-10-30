package server

import (
	"context"
	"strconv"
	"strings"
)

// RenameBank runs Hindsight's own rename (hindsight-admin rename-bank) in
// the server's container: one transaction moves every row of the bank to
// the new id, then its vector indexes are rebuilt. It refuses while the
// bank has pending or processing operations, and changes nothing then;
// dryRun runs it and rolls back.
func (c Compose) RenameBank(ctx context.Context, from, to string, dryRun bool) ([]byte, error) {
	args := []string{"hindsight-admin", "rename-bank", "--from", from, "--to", to}
	if dryRun {
		args = append(args, "--dry-run")
	}
	return c.Exec(ctx, "hindsight", args...)
}

// PendingOperations counts the operations of banks the server has not
// finished (consolidation after a send): a rename waits for them.
func (d DB) PendingOperations(ctx context.Context, banks []string) (int, error) {
	return pendingOperations(ctx, d.Query, banks)
}

func pendingOperations(ctx context.Context, query queryFunc, banks []string) (int, error) {
	q := make([]string, len(banks))
	for i, b := range banks {
		q[i] = sqlString(b)
	}
	out, err := query(ctx, "select count(*) from async_operations where bank_id in ("+strings.Join(q, ", ")+") and status in ('pending', 'processing')")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}
