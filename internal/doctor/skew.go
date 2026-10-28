package doctor

import (
	"fmt"
	"strings"

	"github.com/Barben360/backsight/internal/server"
)

// VersionSkew compares the Hindsight version a server runs with the one
// this backsight pins: OK equal, Info when only the patch differs, Warn
// otherwise or when either does not parse. Never a refusal: hooks keep
// working across a skew.
func VersionSkew(serverVersion, pinned string) Status {
	if serverVersion == pinned {
		return OK
	}
	s, p := strings.Split(serverVersion, "."), strings.Split(pinned, ".")
	if len(s) != 3 || len(p) != 3 {
		return Warn
	}
	for _, part := range append(append([]string{}, s...), p...) {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return Warn
		}
	}
	if s[0] == p[0] && s[1] == p[1] {
		return Info
	}
	return Warn
}

// checkSkew: a client compares its server's Hindsight version with its pin.
func checkSkew(serverVersion string) Check {
	pinned := server.PinnedVersion()
	c := Check{ID: "version-skew", Status: VersionSkew(serverVersion, pinned)}
	c.Summary = fmt.Sprintf("server runs Hindsight %s, this backsight pins %s", serverVersion, pinned)
	if c.Status == Warn {
		c.Fix = "on the server's host: backsight update apply; or update this PC to the host's backsight"
	}
	return c
}
