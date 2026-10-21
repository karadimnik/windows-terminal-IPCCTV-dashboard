package osx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The starter honours the command: argv quoting, environment, stdout/stderr, exit code, registry.
func TestRealScopeRunsCommandFaithfully(t *testing.T) {
	needReal(t)
	reg := NewRegistry(RegistryPath(t.TempDir()))
	s, err := NewScope("t", ScopeOptions{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := selfCmd("echo", "HOLOCRON_OSX_ECHO=v a=l")
	args := []string{"plain", "with space", `q"uote`, `back\slash\`, "", `tail\\`}
	c.Args = append(c.Args, args...)
	var out, errOut syncBuf
	c.Stdout, c.Stderr = &out, &errOut
	p, err := s.Start(c)
	if err != nil {
		t.Fatal(err)
	}
	if e := reg.Entries(); len(e) != 1 || e[0].Pid != p.Pid() || e[0].Start == "" || e[0].Image == "" {
		t.Errorf("registry while running: %+v", e)
	}
	code, err := p.Wait()
	if err != nil || code != 7 {
		t.Fatalf("exit %d %v, want 7", code, err)
	}
	if want := "args=" + strings.Join(args, "|") + "\n"; out.String() != want {
		t.Errorf("stdout %q, want %q", out.String(), want)
	}
	if want := "env=v a=l\n"; errOut.String() != want {
		t.Errorf("stderr %q, want %q", errOut.String(), want)
	}
	if e := reg.Entries(); len(e) != 0 {
		t.Errorf("registry after the exit: %+v", e)
	}
	if err := p.Kill(); err != nil {
		t.Errorf("kill after exit: %v", err)
	}
}

// G5: the supervisor (a helper process holding a Scope) is hard-killed; every descendant dies.
func TestRealScopeHardKillLeavesNoSurvivors(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h := startHelper(t, dir, "scope-sup", "HOLOCRON_OSX_N=3")
	var st helperState
	waitFile(t, filepath.Join(dir, "state.json"), 10*time.Second, &st)
	if len(st.Children) != 3 {
		t.Fatalf("children %v", st.Children)
	}
	time.Sleep(500 * time.Millisecond)
	tree := descendants(t, h.Process.Pid)
	sw := &sweep{}
	sw.add(t, tree...)
	defer func() { killed := sw.run(t); t.Logf("sweep killed %v", killed) }()
	w := newWatch(t, tree)
	defer w.close()
	t0 := time.Now()
	hardKill(t, h.Cmd)
	surv := w.survivors(5 * time.Second)
	t.Logf("tree %v (children %v); survivors %v; all gone after %v", tree, st.Children, surv, time.Since(t0))
	if len(tree) < 3 {
		t.Fatalf("snapshot %v misses children", tree)
	}
	if len(surv) != 0 {
		t.Errorf("survivors %v", surv)
	}
}

// G5 with grandchildren: each child starts a long-running grandchild at once. Windows: membership is
// inherited, 0 survivors. Linux: Pdeathsig does not reach grandchildren (SP3 l6), the children die,
// the grandchildren survive a HARD kill (the orderly stop gets them through the group).
func TestRealScopeHardKillWithGrandchildren(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h := startHelper(t, dir, "scope-sup", "HOLOCRON_OSX_N=3", "HOLOCRON_OSX_KIND=grand")
	var st helperState
	waitFile(t, filepath.Join(dir, "state.json"), 15*time.Second, &st)
	if len(st.Grand) != 3 {
		t.Fatalf("grandchildren %v", st.Grand)
	}
	time.Sleep(300 * time.Millisecond)
	all := union(descendants(t, h.Process.Pid), st.Grand)
	sw := &sweep{}
	sw.add(t, all...)
	defer func() { t.Logf("sweep killed %v", sw.run(t)) }()
	w := newWatch(t, all)
	defer w.close()
	hardKill(t, h.Cmd)
	surv := w.survivors(5 * time.Second)
	t.Logf("children %v grandchildren %v tree %v; survivors %v", st.Children, st.Grand, all, surv)
	if runtime.GOOS == "windows" {
		if len(surv) != 0 {
			t.Errorf("survivors %v", surv)
		}
		return
	}
	for _, pid := range surv {
		for _, c := range st.Children {
			if pid == c {
				t.Errorf("direct child %d survived", pid)
			}
		}
	}
	t.Logf("Linux: %d of 3 grandchildren survived the hard kill (documented SP3 l6 limit)", len(intersect(surv, st.Grand)))
}

// A kill that lands while the supervisor is starting children back to back.
func TestRealScopeKillRacesStart(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h := startHelper(t, dir, "race")
	pids := filepath.Join(dir, "race.pids")
	deadline := time.Now().Add(15 * time.Second)
	for len(readPids(pids)) < 8 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	before := descendants(t, h.Process.Pid)
	sw := &sweep{}
	sw.add(t, before...)
	w := newWatch(t, before)
	defer w.close()
	hardKill(t, h.Cmd)
	after := leftovers(t, h.Process.Pid, dir) // includes a child created while the kill landed
	after = union(nil, minus(after, before))
	sw.add(t, after...)
	defer func() { t.Logf("sweep killed %v", sw.run(t)) }()
	w2 := newWatch(t, after)
	defer w2.close()
	surv := append(w.survivors(5*time.Second), w2.survivors(time.Second)...)
	t.Logf("recorded %d starts, %d descendants before the kill, %d seen after; survivors %v", len(readPids(pids)), len(before), len(after), surv)
	if len(surv) != 0 {
		t.Errorf("survivors %v", surv)
	}
}

// Orphans of an unprotected supervisor are reaped; a decoy whose pid is recorded with a wrong start
// time, a wrong image and a dead pid are left alone.
func TestRealRegistryReapSparesDecoy(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h := startHelper(t, dir, "orphans")
	h.Wait()
	var st helperState
	waitFile(t, filepath.Join(dir, "state.json"), 5*time.Second, &st)
	sw := &sweep{}
	sw.add(t, st.Children...)
	decoy, err := spawnUnbound()
	if err != nil {
		t.Fatal(err)
	}
	sw.add(t, decoy)
	defer func() { t.Logf("sweep killed %v (the decoy %d is expected here)", sw.run(t), decoy) }()
	info, err := OSInspector{}.Inspect(decoy)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := NewRegistry(RegistryPath(dir)).Load()
	entries = append(entries,
		ChildEntry{Pid: decoy, Start: "1" + info.Start, Image: info.Image, Role: "fake-wrongstart"},
		ChildEntry{Pid: decoy, Start: info.Start, Image: info.Image + ".other", Role: "fake-wrongimage"},
		ChildEntry{Pid: 4000000, Start: info.Start, Image: info.Image, Role: "fake-dead"})
	file := NewRegistry(RegistryPath(dir)) // rewrite the file: the orphans plus the three fakes
	for _, e := range entries {
		file.Add(e)
	}
	w := newWatch(t, st.Children)
	defer w.close()
	verdicts, err := Reap(file, OSInspector{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var killed []int
	for _, v := range verdicts {
		t.Logf("reap pid %d %s: killed=%v %s", v.Entry.Pid, v.Entry.Role, v.Killed, v.Why)
		if v.Killed {
			killed = append(killed, v.Entry.Pid)
		}
	}
	sort.Ints(killed)
	want := append([]int(nil), st.Children...)
	sort.Ints(want)
	if fmt.Sprint(killed) != fmt.Sprint(want) {
		t.Errorf("killed %v, want the 3 orphans %v", killed, want)
	}
	if surv := w.survivors(3 * time.Second); len(surv) != 0 {
		t.Errorf("orphans still alive: %v", surv)
	}
	if _, err := (OSInspector{}).Inspect(decoy); err != nil {
		t.Errorf("the decoy %d was killed: %v", decoy, err)
	}
}

// G8: two processes, one lock. The second gets ErrHeld naming the first; killing the holder frees it
// at once (no stale lock).
func TestRealLockTwoProcesses(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	path := LockPath(filepath.Join(dir, "_state"))
	mine, err := AcquireInstanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	h := startHelper(t, dir, "lock", "HOLOCRON_OSX_LOCK="+path)
	h.Wait()
	var r map[string]any
	waitFile(t, filepath.Join(dir, "lock.json"), 5*time.Second, &r)
	if r["held"] != true || int(r["owner"].(float64)) != os.Getpid() {
		t.Fatalf("second process: %v, want held by pid %d", r, os.Getpid())
	}
	t.Logf("second process refused: %v", r["err"])
	mine.Release()

	os.Remove(filepath.Join(dir, "lock.json"))
	h2 := startHelper(t, dir, "lock", "HOLOCRON_OSX_LOCK="+path)
	waitFile(t, filepath.Join(dir, "lock.json"), 5*time.Second, &r)
	if r["acquired"] != true {
		t.Fatalf("helper did not get the free lock: %v", r)
	}
	_, err = AcquireInstanceLock(path)
	var he *HeldError
	if !errors.As(err, &he) || he.Owner.Pid != h2.Process.Pid {
		t.Fatalf("while the helper holds it: %v", err)
	}
	t.Logf("refused while held: %v", err)
	hardKill(t, h2.Cmd)
	h2.Wait()
	t0 := time.Now()
	for {
		l, err := AcquireInstanceLock(path)
		if err == nil {
			t.Logf("lock free %v after the holder was killed", time.Since(t0))
			l.Release()
			break
		}
		if time.Since(t0) > time.Second {
			t.Fatalf("lock still held 1 s after the holder died: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func intersect(a, b []int) []int {
	var out []int
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
			}
		}
	}
	return out
}

func union(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, x := range append(append([]int(nil), a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func minus(a, b []int) []int {
	var out []int
	for _, x := range a {
		if len(intersect([]int{x}, b)) == 0 {
			out = append(out, x)
		}
	}
	return out
}
