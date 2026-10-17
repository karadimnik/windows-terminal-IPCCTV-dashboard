package osx

import (
	"os"
	"path/filepath"
	"runtime"
)

func (real) env() Env {
	e := Env{Getenv: os.Getenv}
	e.Home, _ = os.UserHomeDir()
	if exe, err := os.Executable(); err == nil {
		e.ExeDir = filepath.Dir(exe)
	}
	return e
}

func (r real) Dirs() Dirs { return DefaultDirs(runtime.GOOS, r.env()) }

func (r real) ProtectedRoots() []Protected {
	e := r.env()
	e.Mounts = mountPoints()
	return ProtectedFor(runtime.GOOS, e)
}

func (real) DiskFree(path string) (uint64, error) { return diskFree(path) }

func (real) SetOwnerOnly(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return setOwnerOnly(path, fi.IsDir())
}
