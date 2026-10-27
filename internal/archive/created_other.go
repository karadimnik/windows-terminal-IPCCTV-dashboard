//go:build !windows

package archive

import (
	"io/fs"
	"time"
)

// CreatedTime: the standard library reports no creation time outside Windows.
func CreatedTime(fs.FileInfo) time.Time { return time.Time{} }
