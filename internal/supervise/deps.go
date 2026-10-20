package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/prune"
	"holocron/internal/relay"
)

// ConfigSource hands out the configuration (read at start and re-read every ReconcileSeconds to
// notice a change) and the camera list (re-read every ReconcileSeconds).
type ConfigSource interface {
	Config() (config.Config, error)
	Cameras() ([]config.Camera, error)
}

// FileSource is the production ConfigSource: holocron.config.json and cameras.json.
type FileSource struct {
	ConfigPath  string
	CamerasPath string
	Platform    osx.Platform
}

func (f FileSource) Config() (config.Config, error) {
	c, _, err := config.Load(f.ConfigPath, f.Platform)
	return c, err
}

func (f FileSource) Cameras() ([]config.Camera, error) {
	c, err := config.LoadCameras(f.CamerasPath)
	if err != nil {
		return nil, err
	}
	return c.List, nil
}

// RelayAPI is the part of relay.API the supervisor uses (*relay.API satisfies it).
type RelayAPI interface {
	Ready(ctx context.Context) (relay.ReadySet, error)
	WaitStarted(ctx context.Context, p relay.ReadinessPolicy, exited <-chan struct{}) (relay.ReadySet, bool)
}

// RootScanner adds the archive-wide scan (Get-CamArchiveScan) to the per-camera Scanner.
type RootScanner interface {
	Scanner
	ScanRoot(root string, now time.Time) (ArchiveScan, error)
}

// ArchiveScan is the archive-wide picture behind the Disk figures of status.json.
type ArchiveScan struct {
	ScannedAt      time.Time
	Files          int64
	Bytes          int64
	Oldest, Newest time.Time                  // min / max write time over every file
	Dirs           map[string]archive.DirScan // by folder name (= safe name)
	Skipped        []string
}

// ScanRoot is Get-CamArchiveScan: every immediate subfolder of root that does not start with "_" and
// is not a link or reparse point, each scanned like a camera folder.
func (a ArchiveScanner) ScanRoot(root string, now time.Time) (ArchiveScan, error) {
	out := ArchiveScan{ScannedAt: now, Dirs: map[string]archive.DirScan{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "_") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil {
			continue
		}
		if archive.IsReparse(fi) {
			out.Skipped = append(out.Skipped, dir)
			continue
		}
		if !fi.IsDir() {
			continue
		}
		s, err := a.Scan(dir, now)
		if err != nil {
			continue
		}
		out.Dirs[e.Name()] = s
		out.Files += int64(s.Files)
		out.Bytes += s.Bytes
		if !s.OldestWrite.IsZero() && (out.Oldest.IsZero() || s.OldestWrite.Before(out.Oldest)) {
			out.Oldest = s.OldestWrite
		}
		if s.NewestWrite.After(out.Newest) {
			out.Newest = s.NewestWrite
		}
	}
	return out, nil
}

// Pruner is the archive pruner as the supervisor drives it.
type Pruner interface {
	// Claim is Initialize-CamArchiveOwner, once at start: write the ownership marker when the root
	// qualifies. A refusal is a Problem, not an error.
	Claim(now time.Time) (archive.ClaimResult, error)
	// Run is one prune cycle; st carries the episodes between runs. Alerts are in the result.
	Run(now time.Time, st *prune.State) (prune.Result, error)
}

// ArchivePruner is the production Pruner: archive.ClaimRoot, prune.Collect (root verdict and
// marker first: a refusal reads nothing else and deletes nothing) and prune.Executor. When a sandbox
// is in force (Sandbox, or SANDBOX_ONLY in the environment) a root outside it is refused before
// anything is read or written.
type ArchivePruner struct {
	Root      string
	Platform  osx.Platform
	Rules     prune.Rules
	Container string // extension, ".mkv"
	Log       *slog.Logger
	Sandbox   string
}

func (a ArchivePruner) sandbox() string {
	if a.Sandbox != "" {
		return a.Sandbox
	}
	return sandboxFromEnv()
}

func sandboxFromEnv() string {
	switch v := os.Getenv(prune.SandboxEnv); v {
	case "", "0":
		return ""
	case "1":
		return os.TempDir()
	default:
		return v
	}
}

func (a ArchivePruner) Claim(now time.Time) (archive.ClaimResult, error) {
	if err := prune.CheckSandbox(a.Root, a.sandbox()); err != nil {
		return archive.ClaimResult{Path: archive.MarkerPath(a.Root), Problem: err.Error()}, nil
	}
	return archive.ClaimRoot(a.Root, a.Platform, a.Container, now)
}

func (a ArchivePruner) Run(now time.Time, st *prune.State) (prune.Result, error) {
	if err := prune.CheckSandbox(a.Root, a.sandbox()); err != nil {
		return prune.Result{}, err
	}
	in, err := prune.Collect(a.Root, a.Platform, a.Rules, now, false)
	if err != nil {
		return prune.Result{}, err
	}
	ex := prune.Executor{FS: prune.OSFS{Platform: a.Platform}, Log: a.Log, Actor: "supervisor", Sandbox: a.Sandbox}
	return ex.Run(in, st)
}

// InstanceLock is the one-supervisor-per-archive lock. Acquire returns a release function and this
// holder's identity, or an error and the current holder (exit 20). The OS lock arrives in W8.
type InstanceLock interface {
	Acquire() (release func(), owner string, err error)
}

// ErrLockHeld is MemoryLock's refusal.
var ErrLockHeld = errors.New("the supervisor lock is held")

// MemoryLock is an in-process InstanceLock (one holder at a time). Placeholder until the OS lock (W8).
type MemoryLock struct {
	Owner string // identity recorded by Acquire; default "pid <n>"

	mu     sync.Mutex
	holder string
	held   bool
	acqs   int
	rels   int
}

func (l *MemoryLock) Acquire() (func(), string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return nil, l.holder, ErrLockHeld
	}
	owner := l.Owner
	if owner == "" {
		owner = fmt.Sprintf("pid %d", os.Getpid())
	}
	l.held, l.holder = true, owner
	l.acqs++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.held, l.holder = false, ""
			l.rels++
			l.mu.Unlock()
		})
	}, owner, nil
}

// Held reports whether the lock is held.
func (l *MemoryLock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// Counts returns how often the lock was acquired and released.
func (l *MemoryLock) Counts() (acquired, released int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acqs, l.rels
}

// ProcessScope ties every child to the supervisor's lifetime (W8: Windows job object, Linux process
// group with a parent-death signal). Open is called once, before the first child starts, and returns
// the Starter every child (ffmpeg and MediaMTX) is created with; Close runs after the ordered stop.
type ProcessScope interface {
	Open(base ffmpeg.Starter) (ffmpeg.Starter, error)
	Close() error
}

// NoScope is the no-op ProcessScope used until W8.
type NoScope struct{}

func (NoScope) Open(base ffmpeg.Starter) (ffmpeg.Starter, error) { return base, nil }
func (NoScope) Close() error                                     { return nil }

// LogSource hands out component loggers (*logx.Logger satisfies it).
type LogSource interface {
	For(component string) *slog.Logger
}

// Registrar takes a secret to mask (*logx.Redactor and *status.Redactor satisfy it).
type Registrar interface {
	Register(secret string)
}

// FileChildLogs returns the production ChildLog: one rotating file per child output log (a camera's
// <safe>.log, restreamer.log, liveview.log), lines redacted with red.
func FileChildLogs(maxBytes int64, red *logx.Redactor) func(component, path string) *slog.Logger {
	return func(component, path string) *slog.Logger {
		l, _ := logx.New(logx.Options{
			Config:   logx.Config{Level: "trace"},
			Getenv:   func(string) string { return "" },
			Sink:     logx.NewRotatingFile(logx.FileOptions{Path: path, MaxBytes: maxBytes}),
			Redactor: red,
		})
		return l.For(component)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type discardLogs struct{}

func (discardLogs) For(string) *slog.Logger { return discardLogger() }
