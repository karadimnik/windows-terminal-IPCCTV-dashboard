// SP2: what does a Go console program see when its terminal window is closed?
//
//	sp2.exe -dir D -tag T            run: log events, heartbeat, job-bound children
//	sp2.exe -sendctrl PID c|break    helper: attach to PID's console and send CTRL_C / CTRL_BREAK
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	k32                 = syscall.NewLazyDLL("kernel32.dll")
	pSetConsoleCtrl     = k32.NewProc("SetConsoleCtrlHandler")
	pCreateJob          = k32.NewProc("CreateJobObjectW")
	pSetInfoJob         = k32.NewProc("SetInformationJobObject")
	pAssignJob          = k32.NewProc("AssignProcessToJobObject")
	pGetConsoleWindow   = k32.NewProc("GetConsoleWindow")
	pGetConsoleProcList = k32.NewProc("GetConsoleProcessList")
	pGetModuleHandle    = k32.NewProc("GetModuleHandleW")
	pAttachConsole      = k32.NewProc("AttachConsole")
	pFreeConsole        = k32.NewProc("FreeConsole")
	pGenerateCtrlEvent  = k32.NewProc("GenerateConsoleCtrlEvent")
	logMu               sync.Mutex
	logF                *os.File
	evAt                time.Time // first termination event (either route)
	evMu                sync.Mutex
	eventName           = map[uint32]string{0: "CTRL_C_EVENT", 1: "CTRL_BREAK_EVENT", 2: "CTRL_CLOSE_EVENT", 5: "CTRL_LOGOFF_EVENT", 6: "CTRL_SHUTDOWN_EVENT"}
)

func logf(format string, a ...any) {
	now := time.Now().UTC()
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(logF, "%d %s %s\n", now.UnixMilli(), now.Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

func markEvent() {
	evMu.Lock()
	if evAt.IsZero() {
		evAt = time.Now()
	}
	evMu.Unlock()
}

func main() {
	dir := flag.String("dir", ".", "log/pid folder")
	tag := flag.String("tag", "run", "run tag")
	hb := flag.Int("hb", 10, "seconds to keep running after the first event")
	maxs := flag.Int("max", 90, "failsafe: exit after this many seconds without an event")
	send := flag.String("sendctrl", "", "helper mode: PID of the console owner to signal")
	flag.Parse()
	if *send != "" {
		sendCtrl(*send, flag.Arg(0))
		return
	}
	var err error
	logF, err = os.OpenFile(filepath.Join(*dir, "sp2-"+*tag+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	defer logF.Close()

	var procs [8]uint32
	n, _, _ := pGetConsoleProcList.Call(uintptr(unsafe.Pointer(&procs[0])), 8)
	hw, _, _ := pGetConsoleWindow.Call()
	u32, _ := syscall.UTF16PtrFromString("user32.dll")
	u32h, _, _ := pGetModuleHandle.Call(uintptr(unsafe.Pointer(u32)))
	logf("start pid=%d ppid=%d consoleHwnd=%#x consoleProcs=%d%v user32Loaded=%v go=%s", os.Getpid(), os.Getppid(), hw, n, procs[:min(int(n), 8)], u32h != 0, "windows")

	// Raw route: registered after Go's own handler, so it runs first; returning 0 passes the event on to Go's.
	cb := syscall.NewCallback(func(ev uint32) uintptr {
		markEvent()
		logf("RAW ctrl event %d (%s)", ev, eventName[ev])
		return 0
	})
	pSetConsoleCtrl.Call(cb, 1)

	// Go route.
	sig := make(chan os.Signal, 8)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	// Job with kill-on-close; the handle is held for the process life.
	job, _, _ := pCreateJob.Call(0, 0)
	var info [144]byte // JOBOBJECT_EXTENDED_LIMIT_INFORMATION on amd64: LimitFlags at offset 16
	*(*uint32)(unsafe.Pointer(&info[16])) = 0x2000
	if r, _, e := pSetInfoJob.Call(job, 9, uintptr(unsafe.Pointer(&info[0])), unsafe.Sizeof(info)); r == 0 {
		logf("SetInformationJobObject failed: %v", e)
	}
	shared := startChild(job, "sharedConsole", 0)
	own := startChild(job, "ownConsole", 0x08000000) // CREATE_NO_WINDOW: its own (hidden) console
	pidFile := filepath.Join(*dir, "sp2-"+*tag+".pids")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("sp2=%d shared=%d own=%d\n", os.Getpid(), shared, own)), 0o644)
	logf("children started shared=%d own=%d (both in the kill-on-close job)", shared, own)

	t0 := time.Now()
	lastProbe := probe()
	logf("console probe: %s", lastProbe)
	tick := time.NewTicker(250 * time.Millisecond)
	for {
		select {
		case s := <-sig:
			markEvent()
			logf("GO signal %v (%d)", s, s)
			logf("cleanup: final line written (this is what a handler can do)")
			logf("cleanup: done")
		case <-tick.C:
			if pr := probe(); pr != lastProbe {
				logf("console probe CHANGED: %s", pr)
				lastProbe = pr
			}
			evMu.Lock()
			ea := evAt
			evMu.Unlock()
			if ea.IsZero() {
				if time.Since(t0) > time.Duration(*maxs)*time.Second {
					logf("failsafe exit (no event)")
					return
				}
				logf("tick")
				continue
			}
			age := time.Since(ea)
			logf("tick +%dms after first event", age.Milliseconds())
			if age > time.Duration(*hb)*time.Second {
				logf("exit: survived the whole %ds window, exiting normally", *hb)
				return
			}
		}
	}
}

// probe asks the console three cheap questions; after the console host dies they should fail.
func probe() string {
	var b [2]uint32
	n, _, e1 := pGetConsoleProcList.Call(uintptr(unsafe.Pointer(&b[0])), 2)
	var m uint32
	e2 := syscall.GetConsoleMode(syscall.Stdin, &m)
	hw, _, _ := pGetConsoleWindow.Call()
	return fmt.Sprintf("GetConsoleProcessList=%d err=%v | GetConsoleMode(stdin) err=%v | GetConsoleWindow=%#x", n, e1, e2, hw)
}

func startChild(job uintptr, role string, flags uint32) int {
	cmd := exec.Command(`C:\Windows\System32\PING.EXE`, "-t", "127.0.0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	if err := cmd.Start(); err != nil {
		logf("child %s start failed: %v", role, err)
		return 0
	}
	// PROCESS_SET_QUOTA | PROCESS_TERMINATE
	h, err := syscall.OpenProcess(0x0100|0x0001, false, uint32(cmd.Process.Pid))
	if err != nil {
		logf("OpenProcess child %s: %v", role, err)
		return cmd.Process.Pid
	}
	defer syscall.CloseHandle(h)
	if r, _, e := pAssignJob.Call(job, uintptr(h)); r == 0 {
		logf("assign child %s to job failed: %v", role, e)
	}
	return cmd.Process.Pid
}

func sendCtrl(pidStr, kind string) {
	var pid uintptr
	fmt.Sscan(pidStr, &pid)
	ev := uintptr(0)
	if kind == "break" {
		ev = 1
	}
	pFreeConsole.Call()
	if r, _, e := pAttachConsole.Call(pid); r == 0 {
		fmt.Println("AttachConsole failed:", e)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt) // absorb our own copy of the event
	pSetConsoleCtrl.Call(0, 1)       // and ignore CTRL_C for this process
	r, _, e := pGenerateCtrlEvent.Call(ev, 0)
	fmt.Printf("GenerateConsoleCtrlEvent(%d, 0) ok=%v err=%v at=%d\n", ev, r != 0, e, time.Now().UTC().UnixMilli())
	time.Sleep(300 * time.Millisecond)
	pFreeConsole.Call()
}
