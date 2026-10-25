package session

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProcessStart is when process pid started, from ps; false when it cannot
// be told.
func ProcessStart(pid int) (time.Time, bool) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C") // the English date layout below
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	return t, err == nil
}
