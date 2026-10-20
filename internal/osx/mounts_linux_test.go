//go:build linux

package osx

import (
	"reflect"
	"testing"
)

func TestParseMountinfo(t *testing.T) {
	in := "22 28 0:21 / /sys rw,nosuid shared:7 - sysfs sysfs rw\n" +
		"30 28 8:1 / /mnt/my\\040disk rw - ext4 /dev/sda1 rw\n" +
		"31 28 8:1 / /mnt/my\\040disk rw - ext4 /dev/sda1 rw\n" +
		"short line\n" +
		"40 28 0:5 / / rw - ext4 /dev/sda2 rw\n"
	want := []string{"/sys", "/mnt/my disk", "/"}
	if got := parseMountinfo(in); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if len(mountPoints()) == 0 {
		t.Error("/proc/self/mountinfo gave no mount points on Linux")
	}
}
