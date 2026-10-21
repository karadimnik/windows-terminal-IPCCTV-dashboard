// Package osx isolates what differs per operating system: disk free space, the protected-root sets
// for the archive guards, owner-only permissions, the default directories, and (W8) the process
// scope that binds children to this process, the instance lock, the child registry with its
// verified reap, console events, and (W12) the Windows service manager seam. Notifications arrive later.
// See tasks/portable-nvr/os-independent-lld.md section 2.9 and README.md in this folder.
package osx
