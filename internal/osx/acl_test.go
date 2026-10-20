package osx

import (
	"reflect"
	"testing"
)

func TestOwnerOnlyICACLS(t *testing.T) {
	got := ownerOnlyICACLS(`C:\x\f.txt`, "S-1-5-21-1", false)
	want := [][]string{
		{`C:\x\f.txt`, "/reset"},
		{`C:\x\f.txt`, "/inheritance:r", "/grant:r", "*S-1-5-21-1:F"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("file: got %v want %v", got, want)
	}
	dir := ownerOnlyICACLS(`C:\x\d`, "S-1-5-21-1", true)
	if dir[0][1] != "/reset" || dir[1][3] != "*S-1-5-21-1:(OI)(CI)F" {
		t.Errorf("dir: %v", dir)
	}
}
