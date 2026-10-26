//go:build linux

package state

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is Linux's USER_HZ, the unit of /proc/<pid>/stat's start
// time: 100 on every architecture Go supports.
const clockTicks = 100

// processStart is when process pid started, read from /proc; ok is false
// when it cannot be told.
func processStart(pid int) (time.Time, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	// The command name (field 2) may hold spaces and parentheses: the
	// fields that follow start after its last ')'. Start time is field 22.
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return time.Time{}, false
	}
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	boot, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}
	return boot.Add(time.Duration(ticks) * time.Second / clockTicks), true
}

// bootTime is /proc/stat's btime.
func bootTime() (time.Time, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return time.Unix(s, 0), err == nil
		}
	}
	return time.Time{}, false
}
