package retain

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

// ReattributionStatus is what the last re-attribution run left to do
// (<state>/reattribute.json): the re-sends waiting for an answer, with
// their cost, for doctor (decision 0072: nothing may diverge unseen).
type ReattributionStatus struct {
	JudgedAt time.Time `json:"judged_at"`
	Retagged int       `json:"retagged"` // documents this run moved in place
	Pending  []Resend  `json:"pending"`  // sessions still to send again
	Cost     Estimate  `json:"cost"`
	Declined bool      `json:"declined,omitempty"` // the user answered resend=no
	// Skipped: why the last daily run did not judge (backups off, a remote
	// server, a backup that did not verify…), "" when it did; SkippedAt when.
	Skipped   string    `json:"skipped,omitempty"`
	SkippedAt time.Time `json:"skipped_at,omitempty"`
}

// SamePending says whether a and b leave the same sessions to send again.
func SamePending(a, b []Resend) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, r := range a {
		seen[r.Session] = true
	}
	for _, r := range b {
		if !seen[r.Session] {
			return false
		}
	}
	return true
}

// ReattributionStatusFile is where it is kept.
func ReattributionStatusFile(stateDir string) string {
	return filepath.Join(stateDir, "reattribute.json")
}

// SaveReattributionStatus writes s.
func SaveReattributionStatus(stateDir string, s ReattributionStatus) error {
	if s.Pending == nil {
		s.Pending = []Resend{}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(ReattributionStatusFile(stateDir), raw, 0o600)
}

// LoadReattributionStatus reads it; ok is false when no run left one.
func LoadReattributionStatus(stateDir string) (ReattributionStatus, bool, error) {
	raw, err := os.ReadFile(ReattributionStatusFile(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return ReattributionStatus{}, false, nil
	}
	if err != nil {
		return ReattributionStatus{}, false, err
	}
	var s ReattributionStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return ReattributionStatus{}, false, err
	}
	return s, true, nil
}

// NoisePoint is one daily reading of the transverse pool: how many of its
// observations name another project (doctor's noise-transverse).
type NoisePoint struct {
	At    time.Time `json:"at"`
	Named int       `json:"named"`
	Total int       `json:"total"`
}

// Share is the part of the observations that name another project.
func (p NoisePoint) Share() float64 {
	if p.Total == 0 {
		return 0
	}
	return float64(p.Named) / float64(p.Total)
}

// NoiseHistoryFile keeps the daily readings, so doctor sees a trend.
func NoiseHistoryFile(stateDir string) string { return filepath.Join(stateDir, "noise-history.jsonl") }

// noiseKeep: readings kept (about four months of daily ones).
const noiseKeep = 120

// RecordNoise appends p to the history, keeping the last noiseKeep. A
// history that cannot be read is left as it is (nothing recorded).
func RecordNoise(stateDir string, p NoisePoint) error {
	all, err := LoadNoise(stateDir)
	if err != nil {
		return err
	}
	all = append(all, p)
	if len(all) > noiseKeep {
		all = all[len(all)-noiseKeep:]
	}
	var buf []byte
	for _, x := range all {
		raw, _ := json.Marshal(x)
		buf = append(append(buf, raw...), '\n')
	}
	return fsutil.WriteAtomic(NoiseHistoryFile(stateDir), buf, 0o600)
}

// LoadNoise reads the history, oldest first; lines that do not parse are
// skipped.
func LoadNoise(stateDir string) ([]NoisePoint, error) {
	fh, err := os.Open(NoiseHistoryFile(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	var out []NoisePoint
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var p NoisePoint
		if json.Unmarshal(sc.Bytes(), &p) == nil && !p.At.IsZero() {
			out = append(out, p)
		}
	}
	return out, sc.Err()
}

// WeekBefore is the reading closest to a week before now, between 6 and
// 10 days old; ok is false without one.
func WeekBefore(points []NoisePoint, now time.Time) (NoisePoint, bool) {
	var best NoisePoint
	found := false
	for _, p := range points {
		age := now.Sub(p.At)
		if age < 6*24*time.Hour || age > 10*24*time.Hour {
			continue
		}
		if !found || absDur(age-7*24*time.Hour) < absDur(now.Sub(best.At)-7*24*time.Hour) {
			best, found = p, true
		}
	}
	return best, found
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
