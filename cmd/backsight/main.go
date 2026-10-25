// Command backsight installs, wires, updates and backs up a self-hosted
// Hindsight memory for Claude Code.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Barben360/backsight/internal/cli"
)

// signals cancel the command's context instead of killing the process.
var signals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	// Ctrl-C, a kill or a closed terminal cancels the command's context so
	// long runs (backfill, restore) stop cleanly, roll back and record why.
	ctx, stop := signal.NotifyContext(context.Background(), signals...)
	code := cli.ExecuteContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
