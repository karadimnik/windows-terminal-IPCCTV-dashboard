//go:build darwin

package osx

import "os"

// mountPoints lists the volumes under /Volumes (read only).
func mountPoints() []string {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, "/Volumes/"+e.Name())
	}
	return out
}
