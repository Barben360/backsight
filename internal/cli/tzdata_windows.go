//go:build windows

package cli

// --tz names a zone whatever the host has: Windows ships no zoneinfo (the
// other targets read the host's).
import _ "time/tzdata"
