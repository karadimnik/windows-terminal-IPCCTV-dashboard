// Package osx isolates what differs per operating system. This first slice holds disk free space,
// the protected-root sets for the archive guards, owner-only permissions and the default
// directories; process lifetime, console events and services arrive in later tasks.
// See tasks/portable-nvr/os-independent-lld.md section 2.9 and README.md in this folder.
package osx
