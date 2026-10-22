//go:build linux

package osx

import (
	"os"
	"strconv"
	"strings"
)

// ListProcs reads /proc/<pid>/stat of every process: name (comm) and parent pid. Read only.
func ListProcs() ([]ProcEntry, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []ProcEntry
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // gone
		}
		s := string(b)
		open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || close < open {
			continue
		}
		f := strings.Fields(s[close+1:]) // f[0] state, f[1] ppid
		if len(f) < 2 || f[0] == "Z" || f[0] == "X" {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		out = append(out, ProcEntry{Pid: pid, PPid: ppid, Name: s[open+1 : close]})
	}
	return out, nil
}
