//go:build !windows

package status

import "os"

// replaceFile: rename(2) is atomic and readers need nothing.
func replaceFile(tmp, path string) (fallback bool, err error) { return false, os.Rename(tmp, path) }
