//go:build !windows

package archive

import "io/fs"

func reparseAttr(fs.FileInfo) bool { return false }
