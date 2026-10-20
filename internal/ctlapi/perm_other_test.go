//go:build !windows

package ctlapi

import (
	"os"
	"testing"
)

func assertOwnerOnlyOS(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s: %v %v", path, fi.Mode(), err)
	}
}
