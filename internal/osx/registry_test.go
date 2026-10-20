package osx

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestRegistryRoundTripAtomic(t *testing.T) {
	path := RegistryPath(filepath.Join(t.TempDir(), "_state"))
	r := NewRegistry(path)
	if got, err := r.Load(); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
	a := ChildEntry{Pid: 10, Start: "s10", Image: "/bin/a", Group: 10, Role: "ffmpeg:A"}
	b := ChildEntry{Pid: 11, Start: "s11", Image: "/bin/b", Role: "relay"}
	if err := r.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(b); err != nil {
		t.Fatal(err)
	}
	got, err := r.Load()
	if err != nil || !reflect.DeepEqual(got, []ChildEntry{a, b}) {
		t.Fatalf("after two adds: %+v %v", got, err)
	}
	if err := r.Remove(10); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Load(); !reflect.DeepEqual(got, []ChildEntry{b}) {
		t.Fatalf("after remove: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left behind: %v", err)
	}
	if err := r.Reset(); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Load(); len(got) != 0 {
		t.Fatalf("after reset: %+v", got)
	}
}

func TestSameImageCaseRule(t *testing.T) {
	if !sameImage("windows", `C:\ffmpeg\bin\FFMPEG.EXE`, `c:\ffmpeg\bin\ffmpeg.exe`) {
		t.Error("windows must fold case")
	}
	if sameImage("linux", "/usr/bin/FFMPEG", "/usr/bin/ffmpeg") || sameImage("darwin", "/a/B", "/a/b") {
		t.Error("unix must not fold case")
	}
}

// The reap kills only provable orphans: alive AND same start AND same image (AND same group when
// recorded). The decoy cases are SP3's: a reused pid (wrong start), a wrong image, a dead pid.
func TestReapVerifiesEveryEntry(t *testing.T) {
	img := "/opt/ffmpeg"
	if runtime.GOOS == "windows" {
		img = `C:\ffmpeg\ffmpeg.exe`
	}
	in := &FakeInspector{Procs: map[int]ProcInfo{
		1: {Start: "t1", Image: img, Group: 1},
		2: {Start: "t2", Image: img, Group: 2},
		3: {Start: "t3", Image: img, Group: 3},
		7: {Start: "t7-new", Image: img, Group: 7}, // pid 7 was reused by another process
		8: {Start: "t8", Image: "/usr/bin/other"},  // same pid and start, different program
		9: {Start: "t9", Image: img, Group: 99},    // moved to another group
	}}
	r := NewRegistry(filepath.Join(t.TempDir(), "children.json"))
	for _, e := range []ChildEntry{
		{Pid: 1, Start: "t1", Image: img, Group: 1}, {Pid: 2, Start: "t2", Image: img, Group: 2}, {Pid: 3, Start: "t3", Image: img},
		{Pid: 7, Start: "t7", Image: img, Group: 7}, {Pid: 8, Start: "t8", Image: img}, {Pid: 9, Start: "t9", Image: img, Group: 9},
		{Pid: 4000000, Start: "x", Image: img}, {Pid: 5, Start: "", Image: img},
	} {
		if err := r.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	verdicts, err := Reap(r, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := in.Stopped(); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("stopped %v, want exactly the 3 provable orphans", got)
	}
	why := map[int]string{}
	for _, v := range verdicts {
		why[v.Entry.Pid] = v.Why
	}
	want := map[int]string{7: "start time differs (pid reused)", 8: "image path differs", 9: "process group differs", 4000000: "not running", 5: "not running"}
	for pid, w := range want {
		if pid == 5 {
			continue
		}
		if why[pid] != w {
			t.Errorf("pid %d: %q, want %q", pid, why[pid], w)
		}
	}
	for _, pid := range []int{7, 8, 9} {
		if _, ok := in.Procs[pid]; !ok {
			t.Errorf("decoy pid %d was stopped", pid)
		}
	}
	if left, _ := r.Load(); len(left) != 0 {
		t.Errorf("registry not emptied after the reap: %+v", left)
	}
}

// Negative control: a reap that trusts the pid alone stops the decoy whose pid was reused.
func TestReapByPidOnlyWouldKillDecoy(t *testing.T) {
	in := &FakeInspector{Procs: map[int]ProcInfo{7: {Start: "t7-new", Image: "/x"}}}
	e := ChildEntry{Pid: 7, Start: "t7", Image: "/x"}
	pidOnly := func(e ChildEntry) bool { _, err := in.Inspect(e.Pid); return err == nil }
	if !pidOnly(e) {
		t.Fatal("control broken: the pid-only check should accept the reused pid")
	}
	if ok, _ := Verify(e, in, runtime.GOOS); ok {
		t.Fatal("Verify accepted a reused pid")
	}
}

func TestReapUnreadableRegistryKillsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "children.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	in := &FakeInspector{Procs: map[int]ProcInfo{1: {Start: "t1", Image: "/x"}}}
	if _, err := Reap(NewRegistry(path), in, nil); err == nil {
		t.Error("an unreadable registry must be reported")
	}
	if len(in.Stopped()) != 0 {
		t.Error("an unreadable registry stopped something")
	}
}

func TestLockInProcessSecondHolderRefused(t *testing.T) {
	path := LockPath(filepath.Join(t.TempDir(), "_state"))
	l, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireInstanceLock(path)
	var he *HeldError
	if !errors.Is(err, ErrHeld) || !errors.As(err, &he) {
		t.Fatalf("second acquire: %v, want ErrHeld", err)
	}
	if he.Owner.Pid != os.Getpid() || he.Owner.Exe == "" {
		t.Errorf("owner %+v, want this pid and an executable", he.Owner)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal("second release:", err)
	}
	l2, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
	if o, _ := ReadLockOwner(path); o.Pid != 0 {
		t.Errorf("owner text left after release: %+v", o)
	}
}
