package session

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, 100 on every Linux ABI Go supports (sysconf needs
// cgo).
const clockTicks = 100

// ProcessStart is when process pid started, from /proc; false when it
// cannot be told.
func ProcessStart(pid int) (time.Time, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	// The command name (field 2) may hold spaces and parentheses: fields
	// are counted after its last ')'. starttime is field 22.
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return time.Time{}, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	boot, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}
	return boot.Add(time.Duration(ticks) * time.Second / clockTicks), true
}

func bootTime() (time.Time, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(n, 0), true
			}
		}
	}
	return time.Time{}, false
}
