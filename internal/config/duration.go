package config

import (
	"fmt"
	"time"
)

// Duration is a setting written as a Go duration string ("6h", "90m",
// "0"), kept to whole seconds. A struct, so that a bare TOML integer is
// refused rather than read as nanoseconds.
type Duration struct{ d time.Duration }

// Hours is a Duration of n hours.
func Hours(n int) Duration { return Duration{time.Duration(n) * time.Hour} }

// UnmarshalText parses a Go duration of whole seconds.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%q: a duration such as \"6h\", \"90m\" or \"0\"", string(b))
	}
	if v%time.Second != 0 {
		return fmt.Errorf("%q: whole seconds only", string(b))
	}
	d.d = v
	return nil
}

// MarshalText writes it in its largest whole unit: "6h", "90m", "45s", "0".
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d Duration) String() string {
	switch v := d.d; {
	case v == 0:
		return "0"
	case v%time.Hour == 0:
		return fmt.Sprintf("%dh", v/time.Hour)
	case v%time.Minute == 0:
		return fmt.Sprintf("%dm", v/time.Minute)
	default:
		return fmt.Sprintf("%ds", v/time.Second)
	}
}

// Seconds is d in whole seconds.
func (d Duration) Seconds() int { return int(d.d / time.Second) }
