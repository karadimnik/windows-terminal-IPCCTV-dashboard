//go:build windows

package osx

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Win32 entry points used by the scope, the lock, the console handling and the inspector. Standard
// library only (no golang.org/x/sys), same technique as spikes/sp3.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW           = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject    = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject   = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject         = kernel32.NewProc("TerminateJobObject")
	procResumeThread               = kernel32.NewProc("ResumeThread")
	procInitProcThreadAttrList     = kernel32.NewProc("InitializeProcThreadAttributeList")
	procUpdateProcThreadAttribute  = kernel32.NewProc("UpdateProcThreadAttribute")
	procDeleteProcThreadAttrList   = kernel32.NewProc("DeleteProcThreadAttributeList")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procSetHandleInformation       = kernel32.NewProc("SetHandleInformation")
	procLockFileEx                 = kernel32.NewProc("LockFileEx")
	procUnlockFileEx               = kernel32.NewProc("UnlockFileEx")
	procSetConsoleCtrlHandler      = kernel32.NewProc("SetConsoleCtrlHandler")
	procGetConsoleWindow           = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList      = kernel32.NewProc("GetConsoleProcessList")
)

const (
	createSuspended          = 0x00000004
	createUnicodeEnvironment = 0x00000400
	createNoWindow           = 0x08000000
	extendedStartupInfo      = 0x00080000
	attrHandleList           = 0x00020002 // PROC_THREAD_ATTRIBUTE_HANDLE_LIST
	limitKillOnJobClose      = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE (never with BREAKAWAY_OK, SP3 w5)
	jobObjectExtendedLimit   = 9
	stillActive              = 259
	processQueryLimited      = 0x1000
	processTerminate         = 0x0001
	synchronize              = 0x00100000
	handleFlagInherit        = 0x00000001
	errorAccessDenied        = syscall.Errno(5)
	errorInvalidParameter    = syscall.Errno(87)
)

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct{ ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64 }

// jobExtendedLimit is JOBOBJECT_EXTENDED_LIMIT_INFORMATION (144 bytes on 64-bit Windows).
type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// newKillOnCloseJob creates an unnamed job (default security: the handle is not inheritable) whose
// members are all killed when its last handle closes.
func newKillOnCloseJob() (syscall.Handle, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info jobExtendedLimit
	info.Basic.LimitFlags = limitKillOnJobClose
	if r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimit, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0, fmt.Errorf("SetInformationJobObject(kill on close): %w", err)
	}
	return syscall.Handle(h), nil
}

// processIdentity reads the start token (creation FILETIME as a decimal) and the image path of an
// open process handle.
func processIdentity(h syscall.Handle) (start, image string, err error) {
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return "", "", err
	}
	var name [1024]uint16
	n := uint32(len(name))
	if r, _, err := procQueryFullProcessImageNameW.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return "", "", err
	}
	return fmt.Sprint(uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime)), syscall.UTF16ToString(name[:n]), nil
}

func alive(h syscall.Handle) bool {
	var code uint32
	return syscall.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
