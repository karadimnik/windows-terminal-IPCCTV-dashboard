//go:build !windows

package archive

import (
	"io/fs"
	"path/filepath"
)

func reparseAttr(fs.FileInfo) bool { return false }

// realPath is where path lands, every symbolic link followed.
func realPath(path string) (string, error) { return filepath.EvalSymlinks(path) }
