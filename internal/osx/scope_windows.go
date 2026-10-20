//go:build windows

package osx

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

type scopeSys struct{ job syscall.Handle }

func (ss *scopeSys) open(s *Scope) error {
	h, err := newKillOnCloseJob()
	if err != nil {
		return err
	}
	ss.job = h
	return nil
}

type procSys struct {
	mu sync.Mutex
	h  syscall.Handle // the process; 0 after the waiter closed it
}

// startupInfoEx is STARTUPINFOEXW.
type startupInfoEx struct {
	syscall.StartupInfo
	attrs uintptr
}

// start is CreateProcess(CREATE_SUSPENDED|CREATE_NO_WINDOW) -> AssignProcessToJobObject ->
// ResumeThread (SP3 w3). os/exec cannot do it: it has no job hook and discards the thread handle.
// Only the three standard handles are inherited (PROC_THREAD_ATTRIBUTE_HANDLE_LIST), so no other
// handle of this process (a pipe a caller reads, the instance lock) leaks into a child.
func (ss *scopeSys) start(s *Scope, p *proc, path string, c Cmd) (ProcInfo, error) {
	app, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ProcInfo{}, err
	}
	argv := append([]string{path}, c.Args...)
	for i := range argv {
		argv[i] = syscall.EscapeArg(argv[i])
	}
	cmdline, err := syscall.UTF16PtrFromString(strings.Join(argv, " "))
	if err != nil {
		return ProcInfo{}, err
	}
	var dir *uint16
	if c.Dir != "" {
		if dir, err = syscall.UTF16PtrFromString(c.Dir); err != nil {
			return ProcInfo{}, err
		}
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	block, err := envBlock(env)
	if err != nil {
		return ProcInfo{}, err
	}

	var childSide []syscall.Handle // closed after CreateProcess
	defer func() {
		for _, h := range childSide {
			syscall.CloseHandle(h)
		}
	}()
	stdin, err := openNul(syscall.GENERIC_READ)
	if err != nil {
		return ProcInfo{}, err
	}
	childSide = append(childSide, stdin)
	var readers []pipeOut
	output := func(w io.Writer, name string) (syscall.Handle, error) {
		if w == nil {
			h, err := openNul(syscall.GENERIC_WRITE)
			if err == nil {
				childSide = append(childSide, h)
			}
			return h, err
		}
		r, wh, err := inheritablePipe()
		if err != nil {
			return 0, err
		}
		childSide = append(childSide, wh)
		readers = append(readers, pipeOut{os.NewFile(uintptr(r), name), w})
		return wh, nil
	}
	stdout, err := output(c.Stdout, "stdout")
	if err != nil {
		closeAll(readers)
		return ProcInfo{}, err
	}
	stderr, err := output(c.Stderr, "stderr")
	if err != nil {
		closeAll(readers)
		return ProcInfo{}, err
	}

	handles := []syscall.Handle{stdin, stdout, stderr}
	var size uintptr
	procInitProcThreadAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))
	attrs := make([]byte, size)
	if r, _, err := procInitProcThreadAttrList.Call(uintptr(unsafe.Pointer(&attrs[0])), 1, 0, uintptr(unsafe.Pointer(&size))); r == 0 {
		closeAll(readers)
		return ProcInfo{}, fmt.Errorf("InitializeProcThreadAttributeList: %w", err)
	}
	defer procDeleteProcThreadAttrList.Call(uintptr(unsafe.Pointer(&attrs[0])))
	if r, _, err := procUpdateProcThreadAttribute.Call(uintptr(unsafe.Pointer(&attrs[0])), 0, attrHandleList,
		uintptr(unsafe.Pointer(&handles[0])), uintptr(len(handles))*unsafe.Sizeof(handles[0]), 0, 0); r == 0 {
		closeAll(readers)
		return ProcInfo{}, fmt.Errorf("UpdateProcThreadAttribute(handle list): %w", err)
	}
	var si startupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = stdin, stdout, stderr
	si.attrs = uintptr(unsafe.Pointer(&attrs[0]))

	flags := uint32(createNoWindow | createUnicodeEnvironment | extendedStartupInfo)
	suspended := s.assignGap == 0
	if suspended {
		flags |= createSuspended
	}
	var pi syscall.ProcessInformation
	err = syscall.CreateProcess(app, cmdline, nil, nil, true, flags, block, dir, &si.StartupInfo, &pi)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(attrs)
	if err != nil {
		closeAll(readers)
		return ProcInfo{}, fmt.Errorf("CreateProcess %s: %w", path, err)
	}
	defer syscall.CloseHandle(pi.Thread)
	p.pid = int(pi.ProcessId)
	if s.afterCreateHook != nil {
		s.afterCreateHook(p.pid)
	}
	if !suspended {
		time.Sleep(s.assignGap) // negative control: the gap SP3 w2 showed grandchildren escaping through
	}
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(ss.job), uintptr(pi.Process)); r == 0 {
		// Fatal for this child: it is never run unprotected.
		syscall.TerminateProcess(pi.Process, 1)
		syscall.WaitForSingleObject(pi.Process, 2000)
		syscall.CloseHandle(pi.Process)
		closeAll(readers)
		return ProcInfo{}, fmt.Errorf("AssignProcessToJobObject pid %d: %w (the child was killed)", p.pid, err)
	}
	start, image, _ := processIdentity(pi.Process)
	if suspended {
		if r, _, err := procResumeThread.Call(uintptr(pi.Thread)); r == ^uintptr(0) {
			syscall.TerminateProcess(pi.Process, 1)
			syscall.CloseHandle(pi.Process)
			closeAll(readers)
			return ProcInfo{}, fmt.Errorf("ResumeThread pid %d: %w (the child was killed)", p.pid, err)
		}
	}
	p.sys.h = pi.Process

	copies := make(chan struct{}, len(readers))
	for _, r := range readers {
		go func(r pipeOut) {
			_, _ = io.Copy(r.w, r.f)
			r.f.Close()
			copies <- struct{}{}
		}(r)
	}
	go func(h syscall.Handle, n int) {
		syscall.WaitForSingleObject(h, syscall.INFINITE)
		var code uint32
		if err := syscall.GetExitCodeProcess(h, &code); err != nil {
			p.code, p.werr = -1, err
		} else {
			p.code = int(int32(code))
		}
		drain := time.After(OutputDrain)
	wait:
		for ; n > 0; n-- {
			select {
			case <-copies:
			case <-drain: // a grandchild still holds the pipe: give up on the rest of the output
				break wait
			}
		}
		p.sys.mu.Lock()
		syscall.CloseHandle(h)
		p.sys.h = 0
		p.sys.mu.Unlock()
		p.s.finish(p)
	}(pi.Process, len(readers))
	return ProcInfo{Start: start, Image: image}, nil
}

func (ps *procSys) kill(p *proc) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.h == 0 {
		return nil
	}
	if err := syscall.TerminateProcess(ps.h, 1); err != nil && alive(ps.h) {
		return err
	}
	return nil
}

func (ss *scopeSys) stopAll(s *Scope, ps []*proc) {
	procTerminateJobObject.Call(uintptr(ss.job), 1)
}

func (ss *scopeSys) close(s *Scope) error {
	if ss.job == 0 {
		return nil
	}
	err := syscall.CloseHandle(ss.job) // kill on close: every member still running dies now
	ss.job = 0
	return err
}

func openNul(access uint32) (syscall.Handle, error) {
	p, _ := syscall.UTF16PtrFromString("NUL")
	sa := &syscall.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return syscall.CreateFile(p, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, sa, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
}

// inheritablePipe returns a read end private to this process and an inheritable write end.
func inheritablePipe() (r, w syscall.Handle, err error) {
	sa := &syscall.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	if err := syscall.CreatePipe(&r, &w, sa, 0); err != nil {
		return 0, 0, err
	}
	if rr, _, err := procSetHandleInformation.Call(uintptr(r), handleFlagInherit, 0); rr == 0 {
		syscall.CloseHandle(r)
		syscall.CloseHandle(w)
		return 0, 0, err
	}
	return r, w, nil
}

type pipeOut struct {
	f *os.File
	w io.Writer
}

func closeAll(ps []pipeOut) {
	for _, p := range ps {
		p.f.Close()
	}
}

// envBlock is the CREATE_UNICODE_ENVIRONMENT block: last value of a name wins (names compared
// case-insensitively, as os/exec does) and SYSTEMROOT is added when missing (Winsock needs it).
func envBlock(env []string) (*uint16, error) {
	seen := map[string]int{}
	var kept []string
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			return nil, fmt.Errorf("environment entry contains NUL: %q", kv)
		}
		name := kv
		if i := strings.IndexByte(kv[min(1, len(kv)):], '='); i >= 0 {
			name = kv[:i+min(1, len(kv))]
		}
		key := strings.ToUpper(name)
		if j, ok := seen[key]; ok {
			kept[j] = kv
			continue
		}
		seen[key] = len(kept)
		kept = append(kept, kv)
	}
	if _, ok := seen["SYSTEMROOT"]; !ok {
		if v := os.Getenv("SYSTEMROOT"); v != "" {
			kept = append(kept, "SYSTEMROOT="+v)
		}
	}
	var b []uint16
	for _, kv := range kept {
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	b = append(b, 0)
	return &b[0], nil
}
