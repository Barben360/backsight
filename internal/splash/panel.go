package splash

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// Sources is what the panel is built from. Dirs come from paths.Default,
// never from a helper that creates them.
type Sources struct {
	Dirs               paths.Dirs
	Now                time.Time
	Zone               *time.Location // nil: local time
	Version, Hindsight string
	ASCII              bool                // a console that is not UTF-8
	Mask               func(string) string // nil: unchanged
	Deadline           time.Duration       // 0: 50 ms
}

// unreachableWarn: a server silent this long is a failure, not a warning
// (doctor's own threshold).
const unreachableWarn = time.Hour

// facts is what the readers found; nil fields were missing, unreadable,
// over their cap or late.
type facts struct {
	config   *configFacts
	noConfig bool
	reach    *state.Reach
	breaker  *state.Breaker
	state    *stateFacts
	notes    *count
	snapshot *snapshotFacts
}

type configFacts struct {
	Server struct {
		URL     string `toml:"url"`
		Mode    string `toml:"mode"`
		Role    string `toml:"role"`
		Runtime string `toml:"runtime"`
	} `toml:"server"`
	Terminal struct {
		Logo any `toml:"logo"` // any: a wrong type falls back, never fails the read
	} `toml:"terminal"`
}

type stateFacts struct {
	UpdateCheck  *state.UpdateCheck  `json:"update_check"`
	RemoteServer *state.RemoteServer `json:"remote_server"`
}

type snapshotFacts struct {
	GeneratedAt time.Time `json:"generated_at"`
	Report      struct {
		Checks []struct {
			Status string `json:"status"`
		} `json:"checks"`
	} `json:"report"`
}

type count struct {
	n    int
	more bool
}

// Build reads the sources under the deadline and returns the panel and
// the configured logo kind. A reader that fails, panics or is late drops
// only its line; nothing is written or created.
func Build(src Sources) ([]Line, string) {
	d := src.Dirs
	read := readFile // once: a late reader never sees the seam change
	readers := []func(*facts){
		func(f *facts) {
			raw, err := read(d.ConfigFile(), fileCap)
			if errors.Is(err, fs.ErrNotExist) {
				f.noConfig = true
				return
			}
			var c configFacts
			if err == nil && toml.Unmarshal(raw, &c) == nil {
				f.config = &c
			}
		},
		func(f *facts) { decodeJSON(read, filepath.Join(d.State, "reach.json"), reachCap, &f.reach) },
		func(f *facts) { decodeJSON(read, state.BreakerPath(d), fileCap, &f.breaker) },
		func(f *facts) { decodeJSON(read, d.StateFile(), stateCap, &f.state) },
		func(f *facts) {
			decodeJSON(read, filepath.Join(d.State, "doctor-snapshot.json"), snapshotCap, &f.snapshot)
		},
		func(f *facts) {
			n, more, err := countLines(filepath.Join(d.OutboxDir(), "notes.jsonl"), outboxMax)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				f.notes = &count{n, more}
			}
		},
	}
	results := make(chan func(*facts), len(readers))
	for _, r := range readers {
		go func() {
			var part facts
			defer func() {
				if recover() != nil {
					results <- nil
				}
			}()
			r(&part)
			results <- func(f *facts) { merge(f, &part) }
		}()
	}
	var f facts
	timer := time.NewTimer(cmp.Or(src.Deadline, 50*time.Millisecond))
	defer timer.Stop()
collect:
	for range readers {
		select {
		case apply := <-results:
			if apply != nil {
				apply(&f)
			}
		case <-timer.C:
			break collect
		}
	}
	return lines(src, &f), logoKind(f.config)
}

func decodeJSON[T any](read func(string, int64) ([]byte, error), path string, limit int64, into **T) {
	raw, err := read(path, limit)
	if err != nil {
		return
	}
	var v T
	if json.Unmarshal(raw, &v) == nil {
		*into = &v
	}
}

// merge copies what one reader found into f.
func merge(f, part *facts) {
	f.config = cmp.Or(part.config, f.config)
	f.noConfig = f.noConfig || part.noConfig
	f.reach = cmp.Or(part.reach, f.reach)
	f.breaker = cmp.Or(part.breaker, f.breaker)
	f.state = cmp.Or(part.state, f.state)
	f.notes = cmp.Or(part.notes, f.notes)
	f.snapshot = cmp.Or(part.snapshot, f.snapshot)
}

func logoKind(c *configFacts) string {
	if c != nil {
		if s, ok := c.Terminal.Logo.(string); ok && s == "blocks" {
			return "blocks"
		}
	}
	return "braille"
}
