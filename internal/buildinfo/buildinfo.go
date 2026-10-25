// Package buildinfo holds the version stamped at link time by goreleaser.
package buildinfo

// Set with -ldflags "-X github.com/Barben360/backsight/internal/buildinfo.Version=…".
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Release reports a goreleaser build, which stamps Date (make build does
// not): only such a build is expected to carry signed checksums.
func Release() bool { return Date != "unknown" }
