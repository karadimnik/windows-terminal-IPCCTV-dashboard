package osx

import "sync"

// Fake is a Platform for tests. The zero value is a Linux-style platform with no free space data.
type Fake struct {
	OS          string            // "windows", "linux" or "darwin"; empty means "linux"
	Free        map[string]uint64 // path -> bytes; DiskFree falls back to DefaultFree
	DefaultFree uint64
	Protected   []Protected
	DirsValue   Dirs
	FreeErr     error // returned by DiskFree for a path not in Free
	OwnerErr    error

	mu    sync.Mutex
	owned []string
}

func (f *Fake) Name() string {
	if f.OS == "" {
		return "linux"
	}
	return f.OS
}

func (f *Fake) DiskFree(path string) (uint64, error) {
	if v, ok := f.Free[path]; ok {
		return v, nil
	}
	return f.DefaultFree, f.FreeErr
}

func (f *Fake) ProtectedRoots() []Protected { return f.Protected }

func (f *Fake) Dirs() Dirs { return f.DirsValue }

func (f *Fake) SetOwnerOnly(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.OwnerErr != nil {
		return f.OwnerErr
	}
	f.owned = append(f.owned, path)
	return nil
}

// OwnerOnlyCalls lists the paths SetOwnerOnly was called with.
func (f *Fake) OwnerOnlyCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.owned...)
}

// NewFake returns a Fake for an OS with that OS's real protected-set builder over env e.
func NewFake(goos string, e Env) *Fake {
	return &Fake{OS: goos, Protected: ProtectedFor(goos, e), DirsValue: DefaultDirs(goos, e)}
}

var _ Platform = (*Fake)(nil)
