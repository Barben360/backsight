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

func main() {
	// Ctrl-C cancels the command's context so long runs (backfill) can stop
	// cleanly and record why.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.ExecuteContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
