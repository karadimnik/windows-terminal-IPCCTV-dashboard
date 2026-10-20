package supervise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"holocron/internal/ffmpeg"
	"holocron/internal/osx"
)

func TestOSLockOneHolder(t *testing.T) {
	l := OSLock{Path: osx.LockPath(filepath.Join(t.TempDir(), "_state"))}
	rel, owner, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(owner, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("owner %q", owner)
	}
	_, owner2, err := l.Acquire()
	if !errors.Is(err, osx.ErrHeld) || !strings.Contains(owner2, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Fatalf("second acquire: %q %v", owner2, err)
	}
	if ExitCode(&AnotherSupervisorError{Owner: owner2}) != ExitAnotherSupervisor {
		t.Error("exit code")
	}
	rel()
	rel2, _, err := l.Acquire()
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

// Open reaps the earlier run's provable orphans BEFORE handing out the starter; the starter
// records every child in the registry; Close leaves nothing.
func TestOSScopeReapsThenStartsInside(t *testing.T) {
	state := filepath.Join(t.TempDir(), "_state")
	old := osx.NewRegistry(osx.RegistryPath(state))
	old.Add(osx.ChildEntry{Pid: 4242, Start: "s", Image: "/x", Role: "ffmpeg"})
	old.Add(osx.ChildEntry{Pid: 4343, Start: "old", Image: "/x", Role: "decoy"})
	in := &osx.FakeInspector{Procs: map[int]osx.ProcInfo{4242: {Start: "s", Image: "/x"}, 4343: {Start: "new", Image: "/x"}}}
	sc := &OSScope{StateDir: state, Inspector: in}
	st, err := sc.Open(ffmpeg.ExecStarter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Stopped(); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("reaped %v, want [4242]", got)
	}
	exe, args := "/bin/sleep", []string{"0.2"}
	if runtime.GOOS == "windows" {
		exe, args = `C:\Windows\System32\PING.EXE`, []string{"-n", "1", "127.0.0.1"}
	}
	p, err := st.Start(ffmpeg.StartConfig{Exe: exe, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := osx.NewRegistry(osx.RegistryPath(state)).Load()
	if len(entries) != 1 || entries[0].Pid != p.Pid() {
		t.Errorf("registry while running: %+v", entries)
	}
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Errorf("exit %d %v", code, err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := osx.NewRegistry(osx.RegistryPath(state)).Load(); len(entries) != 0 {
		t.Errorf("registry after close: %+v", entries)
	}
	if _, err := st.Start(ffmpeg.StartConfig{Exe: exe, Args: args}); !errors.Is(err, osx.ErrScopeClosed) {
		t.Errorf("start after close: %v", err)
	}
}

func TestNewProductionWiresOSBindings(t *testing.T) {
	root := t.TempDir()
	s := NewProduction(SupervisorDeps{}, root)
	if l, ok := s.d.Lock.(OSLock); !ok || l.Path != filepath.Join(root, "_state", "supervisor.lock") {
		t.Errorf("lock %#v", s.d.Lock)
	}
	if sc, ok := s.d.Scope.(*OSScope); !ok || sc.StateDir != filepath.Join(root, "_state") {
		t.Errorf("scope %#v", s.d.Scope)
	}
	mem := &MemoryLock{}
	if s := NewProduction(SupervisorDeps{Lock: mem, Scope: NoScope{}}, root); s.d.Lock != mem {
		t.Error("an injected lock was replaced")
	}
}
