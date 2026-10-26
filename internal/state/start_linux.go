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

// startTicks is process pid's start time, in clock ticks since boot, read
// from /proc/<pid>/stat; ok is false when it cannot be told.
func startTicks(pid int) (int64, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The command name (field 2) may hold spaces and parentheses: the
	// fields that follow start after its last ')'. Start time is field 22.
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	return ticks, err == nil
}

// bootID names this boot (/proc/sys/kernel/random/boot_id): start ticks
// are counted from it.
func bootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// processStart is when process pid started, from its ticks and the boot
// time; ok is false when it cannot be told. Only for a lock written
// without start ticks: the wall clock may have moved since.
func processStart(pid int) (time.Time, bool) {
	ticks, ok := startTicks(pid)
	if !ok {
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
