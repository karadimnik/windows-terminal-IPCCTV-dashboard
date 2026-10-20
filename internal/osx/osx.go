package osx

import "runtime"

// Protected is a path the archive root must neither equal nor contain (be an ancestor of).
type Protected struct {
	Path string
	Why  string // for the refusal message, for example "the Windows directory"
}

// Dirs are the default directories of the running OS (LLD section 4). Every field is absolute.
type Dirs struct {
	Root   string // default archive root (configurable)
	Config string // holocron.config.json and cameras.json
	State  string // <Root>/_state
	Logs   string // <Root>/_logs
	Auth   string // users, sessions, auth log
	TLS    string // local CA and server certificate
}

// Platform is what the archive and config code needs from the operating system.
type Platform interface {
	// Name is the GOOS value: "windows", "linux" or "darwin". It selects the path syntax
	// (separators, drive and UNC roots, case folding) used when judging a path.
	Name() string
	// DiskFree is the number of bytes available to the current user on the volume holding path.
	DiskFree(path string) (uint64, error)
	// ProtectedRoots is the set of paths an archive root may not equal or contain.
	ProtectedRoots() []Protected
	// SetOwnerOnly restricts a file (0600) or directory (0700 / inheritable owner-only ACL) to its owner.
	SetOwnerOnly(path string) error
	// Dirs returns the default directories.
	Dirs() Dirs
}

// Current returns the Platform of the running OS.
func Current() Platform { return real{} }

type real struct{}

func (real) Name() string { return runtime.GOOS }
