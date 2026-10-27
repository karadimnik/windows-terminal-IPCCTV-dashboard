//go:build !windows

package archivetest

import (
	"os"
	"time"
)

// setCreated: no creation time to set outside Windows.
func setCreated(string, time.Time) error { return nil }

// junction is a symbolic link outside Windows, the nearest kind.
func junction(link, target string) error { return os.Symlink(target, link) }
