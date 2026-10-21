package osx

import (
	"errors"
	"sync"
)

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

// FakeSCM is an in-memory ServiceManager. Fail maps an operation ("query", "create", "update",
// "setstart", "delete") to the error it returns; Calls lists "op:name" for every call.
type FakeSCM struct {
	mu       sync.Mutex
	Services map[string]ServiceConfig
	Running  map[string]bool
	Fail     map[string]error
	Calls    []string
}

func (f *FakeSCM) note(op, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, op+":"+name)
	if f.Services == nil {
		f.Services = map[string]ServiceConfig{}
	}
	return f.Fail[op]
}

func (f *FakeSCM) Query(name string) (ServiceState, error) {
	if err := f.note("query", name); err != nil {
		return ServiceState{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Services[name]
	if !ok {
		return ServiceState{}, nil
	}
	st := ServiceState{Exists: true, Config: c, Running: f.Running[name], State: "stopped"}
	if st.Running {
		st.State = "running"
	}
	return st, nil
}

func (f *FakeSCM) Create(c ServiceConfig) error {
	if err := f.note("create", c.Name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Services[c.Name]; ok {
		return errors.New("CreateService: the specified service already exists")
	}
	f.Services[c.Name] = c
	return nil
}

func (f *FakeSCM) Update(c ServiceConfig) error {
	if err := f.note("update", c.Name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Services[c.Name]; !ok {
		return errors.New("OpenService: the specified service does not exist")
	}
	f.Services[c.Name] = c
	return nil
}

func (f *FakeSCM) SetStart(name string, start ServiceStart, delayed bool) error {
	if err := f.note("setstart", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Services[name]
	if !ok {
		return errors.New("OpenService: the specified service does not exist")
	}
	c.Start, c.Delayed = start, delayed && start == StartAuto
	f.Services[name] = c
	return nil
}

func (f *FakeSCM) Delete(name string) error {
	if err := f.note("delete", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Services[name]; !ok {
		return errors.New("OpenService: the specified service does not exist")
	}
	if !f.Running[name] { // a running service is only marked for deletion, as with the real manager
		delete(f.Services, name)
	}
	return nil
}

// CallList returns a copy of Calls.
func (f *FakeSCM) CallList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

var _ ServiceManager = (*FakeSCM)(nil)
