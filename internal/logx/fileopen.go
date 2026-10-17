package logx

import (
	"io"
	"os"
	"path/filepath"
)

// openAppend opens a log file for appending, creating it (and its directory) owner-only.
// On Unix the file is created 0600 and the directory 0700 (before umask). On Windows the mode bits
// are ignored and the file inherits the default ACL of its folder, which is acceptable here.
func openAppend(path string) (io.WriteCloser, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if os.IsNotExist(err) {
		if merr := os.MkdirAll(filepath.Dir(path), 0o700); merr == nil {
			f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		}
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
