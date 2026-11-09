package cli

import (
	"runtime"

	"github.com/Barben360/backsight/internal/shellword"
)

// resumeWord quotes s for the shell a resume command is pasted in, when s
// is not one plain word there: POSIX sh on Linux and macOS (shellWord),
// PowerShell on Windows (psWord). Commands run on another host over ssh
// stay POSIX (shellWord).
func resumeWord(s string) string { return shellword.Word(s) }

// resumeQuote always quotes s, for the same shell as resumeWord.
func resumeQuote(s string) string {
	if runtime.GOOS == "windows" {
		return psQuote(s)
	}
	return shellQuote(s)
}

// psQuote quotes s for PowerShell (shellword.PowerShellQuote).
func psQuote(s string) string { return shellword.PowerShellQuote(s) }

// psWord quotes s for PowerShell when it is not one plain word there
// (shellword.PowerShell).
func psWord(s string) string { return shellword.PowerShell(s) }
