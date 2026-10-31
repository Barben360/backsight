package metrics

import (
	"strings"
	"time"
)

// ZoneName names loc for the views' "tz" field. A named zone is its own
// name ("UTC" for time.UTC). The process-local zone (time.Local, whose own
// name is "Local") is named by the TZ variable when it loads, else by the
// target of /etc/localtime (the part after "zoneinfo/"), else by its
// abbreviation (for example "CET"). getenv and readlink are parameters so
// tests pin each source.
func ZoneName(loc *time.Location, getenv func(string) string, readlink func(string) (string, error)) string {
	if loc == nil || loc == time.UTC {
		return "UTC"
	}
	if loc != time.Local {
		return loc.String()
	}
	if tz := strings.TrimPrefix(getenv("TZ"), ":"); tz != "" {
		if tz == "UTC" {
			return "UTC"
		}
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if target, err := readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && name != "" {
			return name
		}
	}
	abbr, _ := time.Now().In(loc).Zone()
	return abbr
}
