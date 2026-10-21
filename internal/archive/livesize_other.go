//go:build !windows

package archive

import "os"

// liveSize is a plain stat: outside Windows a listing's size is already live, the stat only keeps
// the scan the same on every OS.
func liveSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return -1, err
	}
	return fi.Size(), nil
}
