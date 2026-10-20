//go:build linux

package status

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// OSProcesses inspects real processes through /proc.
type OSProcesses struct{}

const userHZ = 100 // fixed ABI value of the clock ticks in /proc/<pid>/stat

// Alive reads /proc/<pid>/stat (field 22, start time in ticks since boot) and
// /proc/uptime, so the start time does not depend on the wall clock at boot.
// Zombies are not alive.
func (OSProcesses) Alive(pid int64) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	data, err := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	s := string(data)
	rest := s[strings.LastIndexByte(s, ')')+1:] // comm may contain spaces and parentheses
	f := strings.Fields(rest)                   // f[0] is field 3 (state)
	if len(f) == 0 || f[0] == "Z" || f[0] == "X" {
		return time.Time{}, false
	}
	if len(f) < 20 {
		return time.Time{}, true
	}
	ticks, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return time.Time{}, true
	}
	up, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}, true
	}
	uf := strings.Fields(string(up))
	if len(uf) == 0 {
		return time.Time{}, true
	}
	uptime, err := strconv.ParseFloat(uf[0], 64)
	if err != nil {
		return time.Time{}, true
	}
	age := uptime - float64(ticks)/userHZ
	return time.Now().Add(-time.Duration(age * float64(time.Second))), true
}
