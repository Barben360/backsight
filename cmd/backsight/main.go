// Command backsight installs, wires, updates and backs up a self-hosted
// Hindsight memory for Claude Code.
package main

import (
	"os"

	"github.com/Barben360/backsight/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
