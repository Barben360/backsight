package views

import (
	"testing"
	"time"
)

func TestWhen(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("no tzdata: ", err)
	}
	utc := time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC) // 01:30 CET, before the change at 01:00 UTC
	after := utc.Add(2 * time.Hour)                      // 02:30 UTC = 04:30 CEST: the clock skipped an hour
	for _, c := range []struct {
		name string
		t    time.Time
		loc  *time.Location
		want string
	}{
		{"nil location is UTC", time.Date(2026, 3, 29, 22, 5, 59, 0, paris), nil, "2026-03-29 20:05"},
		{"zero time is empty", time.Time{}, paris, ""},
		{"zero time with nil location", time.Time{}, nil, ""},
		{"UTC", utc, time.UTC, "2026-03-29 00:30"},
		{"before the DST change", utc, paris, "2026-03-29 01:30"},
		{"after the DST change", after, paris, "2026-03-29 04:30"},
		{"seconds are dropped", time.Date(2026, 1, 2, 3, 4, 59, 0, time.UTC), time.UTC, "2026-01-02 03:04"},
	} {
		if got := When(c.t, c.loc); got != c.want {
			t.Errorf("%s: When = %q, want %q", c.name, got, c.want)
		}
	}
}
