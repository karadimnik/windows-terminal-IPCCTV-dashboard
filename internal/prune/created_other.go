//go:build !windows

package prune

import (
	"io/fs"
	"time"
)

// created: the standard library reports no creation time outside Windows.
func created(fs.FileInfo) time.Time { return time.Time{} }
