package alerts

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// The Application log is written through advapi32 RegisterEventSourceW and ReportEventW, the calls
// behind PowerShell's Write-EventLog. Standard library only (no golang.org/x/sys needed): the
// source is "IPCCTV" when it is registered (installing it needs elevation and is the PowerShell
// product's -InstallEventSource), otherwise "Application", which is registered on every Windows
// and writable by a standard user (measured by the PowerShell product on 2026-10-13).
var (
	advapi32              = syscall.NewLazyDLL("advapi32.dll")
	procRegisterEvtSource = advapi32.NewProc("RegisterEventSourceW")
	procDeregisterEvtSrc  = advapi32.NewProc("DeregisterEventSource")
	procReportEvent       = advapi32.NewProc("ReportEventW")
)

const (
	evtError       = 0x0001
	evtWarning     = 0x0002
	evtInformation = 0x0004
	customSource   = "IPCCTV"
	fallbackSource = "Application"
)

type winEventLog struct {
	h      syscall.Handle
	Source string
}

var openOSEventLog = func() (EventWriter, error) {
	src := fallbackSource
	if sourceRegistered(customSource) {
		src = customSource
	}
	return openWindowsEventLog(src)
}

func openWindowsEventLog(source string) (*winEventLog, error) {
	p, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return nil, err
	}
	h, _, e := procRegisterEvtSource.Call(0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return nil, fmt.Errorf("RegisterEventSource(%s): %v", source, e)
	}
	return &winEventLog{h: syscall.Handle(h), Source: source}, nil
}

// sourceRegistered reads HKLM\SYSTEM\CurrentControlSet\Services\EventLog\Application\<name>, as
// Test-CamAlertEventSource does (EventLog.SourceExists throws for a standard user).
func sourceRegistered(name string) bool {
	p, err := syscall.UTF16PtrFromString(`SYSTEM\CurrentControlSet\Services\EventLog\Application\` + name)
	if err != nil {
		return false
	}
	var k syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ, &k) != nil {
		return false
	}
	_ = syscall.RegCloseKey(k)
	return true
}

func (w *winEventLog) Write(id int, sev Severity, msg string) error {
	typ := uintptr(evtInformation)
	switch sev {
	case Error:
		typ = evtError
	case Warning:
		typ = evtWarning
	}
	s, err := syscall.UTF16PtrFromString(msg)
	if err != nil {
		return err
	}
	strs := []*uint16{s}
	r, _, e := procReportEvent.Call(uintptr(w.h), typ, 0, uintptr(id), 0, 1, 0,
		uintptr(unsafe.Pointer(&strs[0])), 0)
	runtime.KeepAlive(strs)
	if r == 0 {
		return fmt.Errorf("ReportEvent(%s): %v", w.Source, e)
	}
	return nil
}

func (w *winEventLog) Close() error {
	procDeregisterEvtSrc.Call(uintptr(w.h))
	return nil
}
