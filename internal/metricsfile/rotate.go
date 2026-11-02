package metricsfile

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/fsutil"
)

var fileRE = regexp.MustCompile(`^(server-daily|doctor-history|memory-docs|scan-history)-(\d{4})-(\d{2})\.jsonl$`)

func monthIndex(t time.Time) int {
	t = t.UTC()
	return t.Year()*12 + int(t.Month()) - 1
}

// Rotate removes the month files more than keep months old at at (keep
// months: the one of at and the keep-1 before it) and returns their names.
// Other files, and names that are not a month file, are left alone.
func Rotate(dir string, at time.Time, keep int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	oldest := monthIndex(at) - (keep - 1)
	var removed []string
	for _, e := range entries {
		m := fileRE.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		y, _ := strconv.Atoi(m[2])
		mo, _ := strconv.Atoi(m[3])
		if mo < 1 || mo > 12 || y*12+mo-1 >= oldest {
			continue
		}
		if fsutil.RemoveRetry(filepath.Join(dir, e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	slices.Sort(removed)
	return removed
}
