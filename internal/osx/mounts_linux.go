//go:build linux

package osx

import (
	"os"
	"strings"
)

// mountPoints lists the mount points from /proc/self/mountinfo (read only); an unreadable file
// yields none.
func mountPoints() []string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseMountinfo(string(b))
}

func parseMountinfo(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		m := unescapeMount(f[4])
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// unescapeMount decodes the \040 style octal escapes of mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v := 0
			ok := true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
