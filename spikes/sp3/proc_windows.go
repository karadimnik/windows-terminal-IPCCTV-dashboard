//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	createSuspended    = 0x00000004
	createNoWindow     = 0x08000000
	createBreakaway    = 0x01000000
	extendedStartup    = 0x00080000
	attrJobList        = 0x0002000D // PROC_THREAD_ATTRIBUTE_JOB_LIST
	limitKillOnClose   = 0x2000
	limitBreakawayOK   = 0x0800
	pingExe            = `C:\Windows\System32\PING.EXE`
	stillActive        = 259
	processQueryLimit  = 0x1000
	processTerminate   = 0x0001
	processSetQuota    = 0x0100
	jobObjectPidList   = 3
	jobObjectExtLimits = 9
)

var (
	k32         = syscall.NewLazyDLL("kernel32.dll")
	pCreateJob  = k32.NewProc("CreateJobObjectW")
	pSetInfo    = k32.NewProc("SetInformationJobObject")
	pQueryInfo  = k32.NewProc("QueryInformationJobObject")
	pAssign     = k32.NewProc("AssignProcessToJobObject")
	pIsInJob    = k32.NewProc("IsProcessInJob")
	pResume     = k32.NewProc("ResumeThread")
	pInitAttr   = k32.NewProc("InitializeProcThreadAttributeList")
	pUpdateAttr = k32.NewProc("UpdateProcThreadAttribute")
	pHandleInfo = k32.NewProc("GetHandleInformation")
	pTermJob    = k32.NewProc("TerminateJobObject")
	pImageName  = k32.NewProc("QueryFullProcessImageNameW")
)

func longRunner() []string { return []string{pingExe, "-t", "127.0.0.1"} }

func spawnLong(breakaway bool) (int, error) {
	c := exec.Command(pingExe, "-t", "127.0.0.1")
	fl := uint32(createNoWindow)
	if breakaway {
		fl |= createBreakaway
	}
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: fl}
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}

func platCaseFold(a, b string) bool { return strings.EqualFold(a, b) }

func newJob(flags uint32) uintptr {
	h, _, _ := pCreateJob.Call(0, 0) // no security attributes: the handle is not inheritable
	var info [144]byte               // JOBOBJECT_EXTENDED_LIMIT_INFORMATION, amd64; LimitFlags at offset 16
	*(*uint32)(unsafe.Pointer(&info[16])) = flags
	if r, _, e := pSetInfo.Call(h, jobObjectExtLimits, uintptr(unsafe.Pointer(&info[0])), unsafe.Sizeof(info)); r == 0 {
		panic(e)
	}
	return h
}

func platInit(o *opts) {
	var in int32
	pIsInJob.Call(uintptr(^uintptr(0)), 0, uintptr(unsafe.Pointer(&in))) // current process, any job
	o.log("supervisor is already inside a job: %v", in != 0)
	if o.nojob {
		o.log("no job (control)")
		return
	}
	fl := uint32(limitKillOnClose)
	if o.breakaway {
		fl |= limitBreakawayOK
	}
	o.job = newJob(fl)
	var hf uint32
	pHandleInfo.Call(o.job, uintptr(unsafe.Pointer(&hf)))
	o.log("job created flags=%#x handle inheritable=%v", fl, hf&1 != 0)
}

func platRun(o *opts, fn func()) { fn() }

type startupInfoEx struct {
	syscall.StartupInfo
	attrs *byte
}

func platStart(o *opts, argv []string) (int, error) {
	var parts []string
	for _, a := range argv {
		parts = append(parts, syscall.EscapeArg(a))
	}
	app, _ := syscall.UTF16PtrFromString(argv[0])
	cl, _ := syscall.UTF16PtrFromString(strings.Join(parts, " "))
	flags := uint32(createNoWindow)
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	psi := &si
	var ex startupInfoEx
	var buf []byte
	job := o.job
	assign := o.assign
	if o.nojob {
		assign = "none"
	}
	switch assign {
	case "suspended":
		flags |= createSuspended
	case "joblist":
		var sz uintptr
		pInitAttr.Call(0, 1, 0, uintptr(unsafe.Pointer(&sz)))
		buf = make([]byte, sz)
		if r, _, e := pInitAttr.Call(uintptr(unsafe.Pointer(&buf[0])), 1, 0, uintptr(unsafe.Pointer(&sz))); r == 0 {
			return 0, e
		}
		if r, _, e := pUpdateAttr.Call(uintptr(unsafe.Pointer(&buf[0])), 0, attrJobList, uintptr(unsafe.Pointer(&job)), 8, 0, 0); r == 0 {
			return 0, e
		}
		ex.StartupInfo = si
		ex.Cb = uint32(unsafe.Sizeof(ex))
		ex.attrs = &buf[0]
		psi = (*syscall.StartupInfo)(unsafe.Pointer(&ex))
		flags |= extendedStartup
	}
	if err := syscall.CreateProcess(app, cl, nil, nil, false, flags, nil, nil, psi, &pi); err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(pi.Process)
	defer syscall.CloseHandle(pi.Thread)
	if o.gap > 0 {
		time.Sleep(time.Duration(o.gap) * time.Millisecond) // widen the CreateProcess -> assignment window on purpose
	}
	switch assign {
	case "suspended", "after":
		if r, _, e := pAssign.Call(job, uintptr(pi.Process)); r == 0 {
			o.log("assign pid=%d FAILED: %v", pi.ProcessId, e)
		} else {
			o.log("assigned pid=%d (%s, gap %d ms)", pi.ProcessId, assign, o.gap)
		}
		if assign == "suspended" {
			pResume.Call(uintptr(pi.Thread))
		}
	case "joblist":
		o.log("pid=%d created inside the job by PROC_THREAD_ATTRIBUTE_JOB_LIST", pi.ProcessId)
	}
	return int(pi.ProcessId), nil
}

func platWait(o *opts) {
	for {
		time.Sleep(time.Hour)
	}
}

func procInfo(pid int) (string, string, error) {
	h, err := syscall.OpenProcess(processQueryLimit, false, uint32(pid))
	if err != nil {
		return "", "", err
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return "", "", fmt.Errorf("exited")
	}
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return "", "", err
	}
	var name [1024]uint16
	n := uint32(len(name))
	if r, _, e2 := pImageName.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return "", "", e2
	}
	return fmt.Sprint(uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime)), syscall.UTF16ToString(name[:n]), nil
}

func killPid(pid int) error {
	h, err := syscall.OpenProcess(processTerminate, false, uint32(pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	return syscall.TerminateProcess(h, 1)
}

func platWrapAttr(cmd *exec.Cmd) {}

func platKillTree(pid int) { killPid(pid) }

// cmdJobWrap: the "test driver puts the supervisor in its own job first" step, done by a helper so the driver needs no C#.
func cmdJobWrap(args []string) {
	var dir string
	kill := false
	for len(args) > 0 && args[0] != "--" {
		switch args[0] {
		case "-dir":
			dir, args = args[1], args[2:]
		case "-kill":
			kill, args = true, args[1:]
		default:
			args = args[1:]
		}
	}
	args = args[1:]
	fl := uint32(0)
	if kill {
		fl = limitKillOnClose
	}
	job := newJob(fl)
	o := &opts{assign: "suspended", job: job}
	o.logf, _ = os.OpenFile(filepath.Join(dir, "jobwrap.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	pid, err := platStart(o, args)
	if err != nil {
		panic(err)
	}
	o.log("outer job holds pid=%d (killOnClose=%v)", pid, kill)
	h, _ := syscall.OpenProcess(0x100000|processQueryLimit, false, uint32(pid))
	time.Sleep(4 * time.Second)
	var b [256]byte
	if r, _, _ := pQueryInfo.Call(job, jobObjectPidList, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0); r != 0 {
		n := *(*uint32)(unsafe.Pointer(&b[4]))
		var ids []string
		for i := uint32(0); i < n; i++ {
			ids = append(ids, fmt.Sprint(*(*uintptr)(unsafe.Pointer(&b[8+8*i]))))
		}
		o.log("outer job process list (includes processes of nested jobs): %s", strings.Join(ids, ","))
	}
	syscall.WaitForSingleObject(h, syscall.INFINITE)
	o.log("wrapped process gone, jobwrap exits")
}
