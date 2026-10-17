//go:build !windows

package main

import (
	"os"
	"testing"
)

func assertOwnerOnly(t *testing.T, path string, isDir bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := os.FileMode(0o600)
	if isDir {
		want = 0o700
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s mode %v, want %v", path, info.Mode().Perm(), want)
	}
}
