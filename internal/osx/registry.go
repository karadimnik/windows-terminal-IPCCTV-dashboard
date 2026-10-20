package osx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ChildEntry is one child recorded in _state/children.json (SP3): enough to prove, at the next
// start, that a live pid is still the same process.
type ChildEntry struct {
	Pid   int    `json:"pid"`
	Start string `json:"start"`           // start-time token (Windows creation FILETIME; Linux <boot_id>:<ticks>)
	Image string `json:"image"`           // full image path
	Group int    `json:"group,omitempty"` // process group id (Unix)
	Role  string `json:"role,omitempty"`  // for the log only
}

type registryFile struct {
	Supervisor int          `json:"supervisor"`
	Children   []ChildEntry `json:"children"`
}

// ChildRegistry is the persisted list of the children of this supervisor. Every change rewrites
// the whole file atomically (temp file, sync, rename). Safe for concurrent use.
type ChildRegistry struct {
	path string
	pid  int

	mu      sync.Mutex
	entries []ChildEntry
}

// RegistryPath is <state>/children.json.
func RegistryPath(stateDir string) string { return filepath.Join(stateDir, "children.json") }

// NewRegistry returns a registry writing to path, owned by this process. It does not read the
// file: Reap reads what an earlier supervisor left.
func NewRegistry(path string) *ChildRegistry { return &ChildRegistry{path: path, pid: os.Getpid()} }

// Path is the file.
func (r *ChildRegistry) Path() string { return r.path }

// Load reads the file (what is on disk, not this process's entries). A missing file is empty.
func (r *ChildRegistry) Load() ([]ChildEntry, error) {
	b, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(strings.TrimSpace(string(b))) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path, err)
	}
	return f.Children, nil
}

// Add records a started child and rewrites the file.
func (r *ChildRegistry) Add(e ChildEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return r.writeLocked()
}

// Remove drops the child with this pid (it has exited) and rewrites the file.
func (r *ChildRegistry) Remove(pid int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.Pid != pid {
			kept = append(kept, e)
		}
	}
	r.entries = kept
	return r.writeLocked()
}

// Entries is this process's current list.
func (r *ChildRegistry) Entries() []ChildEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ChildEntry(nil), r.entries...)
}

// Reset empties the list and rewrites the file.
func (r *ChildRegistry) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
	return r.writeLocked()
}

func (r *ChildRegistry) writeLocked() error {
	b, err := json.MarshalIndent(registryFile{Supervisor: r.pid, Children: append([]ChildEntry{}, r.entries...)}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(r.path, append(b, '\n'))
}

// writeAtomic is the SP6 technique without the share-delete rename: only the supervisor opens this
// file, so a plain rename never meets a reader's handle.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ProcInfo is what the OS says about a live pid.
type ProcInfo struct {
	Start string // same token format as ChildEntry.Start
	Image string
	Group int // Unix process group; 0 elsewhere
}

// ErrNotRunning: no live process has the pid (a zombie counts as not running).
var ErrNotRunning = errors.New("not running")

// Inspector reads and stops processes for the reap. OSInspector is the real one.
type Inspector interface {
	// Inspect describes a live pid, or returns ErrNotRunning.
	Inspect(pid int) (ProcInfo, error)
	// Stop kills the process of a verified entry (its whole group on Unix when Group is set). Where
	// the OS allows it the identity is checked again on the handle that is used for the kill.
	Stop(e ChildEntry) error
}

// ReapVerdict is the outcome for one entry.
type ReapVerdict struct {
	Entry  ChildEntry
	Killed bool
	Why    string
}

// sameImage compares image paths: case-insensitive on Windows.
func sameImage(goos, a, b string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Verify decides whether an entry is a provable orphan: alive, same start time, same image path
// (and, when recorded, the same process group). Never by name, never by pid alone.
func Verify(e ChildEntry, in Inspector, goos string) (ok bool, why string) {
	if e.Pid <= 0 {
		return false, "no pid"
	}
	info, err := in.Inspect(e.Pid)
	switch {
	case errors.Is(err, ErrNotRunning):
		return false, "not running"
	case err != nil:
		return false, "cannot inspect: " + err.Error()
	case e.Start == "" || info.Start != e.Start:
		return false, "start time differs (pid reused)"
	case !sameImage(goos, info.Image, e.Image):
		return false, "image path differs"
	case e.Group != 0 && info.Group != e.Group:
		return false, "process group differs"
	}
	return true, "pid alive, same start time, same image"
}

// Reap is what a supervisor does at start, holding the instance lock and BEFORE it starts any
// child: every entry an earlier supervisor left in the registry file is verified and stopped only
// when it is a provable orphan; every other entry is dropped and logged at WARN. The file is then
// emptied. A nil log discards.
func Reap(reg *ChildRegistry, in Inspector, log *slog.Logger) ([]ReapVerdict, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	entries, err := reg.Load()
	if err != nil {
		log.Warn("the child registry cannot be read; nothing is reaped: "+err.Error(), "action", "child.reap", "target", reg.Path(), "result", "fail")
		return nil, errors.Join(err, reg.Reset())
	}
	var out []ReapVerdict
	for _, e := range entries {
		ok, why := Verify(e, in, runtime.GOOS)
		v := ReapVerdict{Entry: e, Why: why}
		if ok {
			if err := in.Stop(e); err != nil {
				v.Why = "kill failed: " + err.Error()
			} else {
				v.Killed = true
				waitGone(in, e.Pid, 2*time.Second)
			}
		}
		msg := fmt.Sprintf("child registry: pid %d (%s, %s): ", e.Pid, e.Role, e.Image)
		if v.Killed {
			log.Warn(msg+"stopped an orphan of an earlier supervisor ("+why+")", "action", "child.reap", "target", e.Role, "result", "ok", "pid", e.Pid)
		} else {
			log.Warn(msg+"left alone: "+v.Why, "action", "child.reap", "target", e.Role, "result", "refused", "pid", e.Pid)
		}
		out = append(out, v)
	}
	return out, reg.Reset()
}

func waitGone(in Inspector, pid int, within time.Duration) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := in.Inspect(pid); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// FakeInspector is an Inspector for tests: Procs holds the live processes, Stop removes one and
// records it.
type FakeInspector struct {
	mu      sync.Mutex
	Procs   map[int]ProcInfo
	stopped []int
}

func (f *FakeInspector) Inspect(pid int) (ProcInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.Procs[pid]
	if !ok {
		return ProcInfo{}, ErrNotRunning
	}
	return p, nil
}

func (f *FakeInspector) Stop(e ChildEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Procs, e.Pid)
	f.stopped = append(f.stopped, e.Pid)
	return nil
}

// Stopped lists the pids Stop was called for, in order.
func (f *FakeInspector) Stopped() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.stopped...)
}
