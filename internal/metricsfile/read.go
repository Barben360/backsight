package metricsfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// Line is one parsed line: its common fields and the whole line.
type Line struct {
	TS   time.Time
	Kind string
	Raw  json.RawMessage
}

// ReadRecent returns the lines of family's last months month files (the
// month of at and the months-1 before it), oldest file first, in file
// order. Torn lines and lines of another schema are skipped.
func ReadRecent(dir, family string, at time.Time, months int) ([]Line, error) {
	var out []Line
	first := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := months - 1; i >= 0; i-- {
		data, err := fsutil.ReadFileShared(monthFile(dir, family, first.AddDate(0, -i, 0)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		for _, raw := range bytes.Split(data, []byte{'\n'}) {
			var c struct {
				Schema int    `json:"schema"`
				TS     string `json:"ts"`
				Kind   string `json:"kind"`
			}
			if json.Unmarshal(raw, &c) != nil || c.Schema != Schema || c.Kind == "" {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, c.TS)
			if err != nil {
				continue
			}
			out = append(out, Line{TS: ts.UTC(), Kind: c.Kind, Raw: json.RawMessage(bytes.Clone(raw))})
		}
	}
	return out, nil
}
