package metrics

import (
	"errors"
	"testing"
	"time"
)

func TestZoneName(t *testing.T) {
	saved := time.Local
	defer func() { time.Local = saved }()
	none := func(string) string { return "" }
	noLink := func(string) (string, error) { return "", errors.New("no link") }
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "TZ" {
				return v
			}
			return ""
		}
	}
	link := func(target string) func(string) (string, error) {
		return func(p string) (string, error) {
			if p != "/etc/localtime" {
				t.Errorf("readlink of %q, want /etc/localtime", p)
			}
			return target, nil
		}
	}
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no tz database:", err)
	}

	// The process-local zone is named by TZ first, then /etc/localtime; this
	// fixed zone stands for it (its abbreviation is "Local").
	time.Local = time.FixedZone("Local", 3600)
	for name, c := range map[string]struct {
		getenv   func(string) string
		readlink func(string) (string, error)
		want     string
	}{
		"TZ":                {env("Europe/Paris"), noLink, "Europe/Paris"},
		"TZ with colon":     {env(":Europe/Paris"), noLink, "Europe/Paris"},
		"TZ wins":           {env("Europe/Paris"), link("/usr/share/zoneinfo/America/New_York"), "Europe/Paris"},
		"readlink":          {none, link("/usr/share/zoneinfo/America/New_York"), "America/New_York"},
		"readlink macOS":    {none, link("/var/db/timezone/zoneinfo/Asia/Tokyo"), "Asia/Tokyo"},
		"bad TZ then link":  {env("not/a-zone"), link("/usr/share/zoneinfo/America/New_York"), "America/New_York"},
		"link without name": {none, link("/etc/other"), "Local"},
		"TZ=UTC":            {env("UTC"), noLink, "UTC"},
	} {
		t.Run(name, func(t *testing.T) {
			// "link without name" falls to the zone's abbreviation.
			if got := ZoneName(time.Local, c.getenv, c.readlink); got != c.want {
				t.Errorf("ZoneName = %q, want %q", got, c.want)
			}
		})
	}

	// Nothing known: the zone abbreviation.
	time.Local = time.FixedZone("CET", 3600)
	if got := ZoneName(time.Local, none, noLink); got != "CET" {
		t.Errorf("nothing known: %q, want CET", got)
	}

	// A named zone is its own name, whatever TZ says; UTC and nil are UTC.
	if got := ZoneName(paris, env("America/New_York"), link("/usr/share/zoneinfo/Asia/Tokyo")); got != "Europe/Paris" {
		t.Errorf("named zone: %q", got)
	}
	if got := ZoneName(time.UTC, env("Europe/Paris"), noLink); got != "UTC" {
		t.Errorf("UTC: %q", got)
	}
	if got := ZoneName(nil, none, noLink); got != "UTC" {
		t.Errorf("nil: %q", got)
	}
}
