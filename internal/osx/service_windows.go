//go:build windows

package osx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The service control manager through advapi32 (standard library only). UNVERIFIED on a real
// system: creating a service needs elevation, which the tests never take; FakeSCM covers the logic.
var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procOpenSCManagerW               = advapi32.NewProc("OpenSCManagerW")
	procCreateServiceW               = advapi32.NewProc("CreateServiceW")
	procOpenServiceW                 = advapi32.NewProc("OpenServiceW")
	procChangeServiceConfigW         = advapi32.NewProc("ChangeServiceConfigW")
	procChangeServiceConfig2W        = advapi32.NewProc("ChangeServiceConfig2W")
	procQueryServiceConfigW          = advapi32.NewProc("QueryServiceConfigW")
	procQueryServiceConfig2W         = advapi32.NewProc("QueryServiceConfig2W")
	procQueryServiceStatus           = advapi32.NewProc("QueryServiceStatus")
	procDeleteService                = advapi32.NewProc("DeleteService")
	procCloseServiceHandle           = advapi32.NewProc("CloseServiceHandle")
	procStartServiceCtrlDispatcherW  = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerEx = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus             = advapi32.NewProc("SetServiceStatus")
)

const (
	scManagerConnect       = 0x0001
	scManagerCreateService = 0x0002
	serviceQueryConfig     = 0x0001
	serviceChangeConfig    = 0x0002
	serviceQueryStatus     = 0x0004
	accessDelete           = 0x00010000
	serviceAllAccess       = 0x000F01FF
	serviceWin32OwnProcess = 0x00000010
	serviceAutoStart       = 2
	serviceDemandStart     = 3
	serviceDisabled        = 4
	serviceErrorNormal     = 1
	serviceNoChange        = 0xFFFFFFFF

	configDescription  = 1
	configFailure      = 2
	configDelayed      = 3
	configFailureFlag  = 4
	scActionRestart    = 1
	failureResetPeriod = 24 * 3600 // seconds

	errServiceDoesNotExist   = syscall.Errno(1060)
	errFailedServiceConnect  = syscall.Errno(1063)
	errServiceSpecificError  = 1066
	errInsufficientBuffer    = syscall.Errno(122)
	serviceStopped           = 1
	serviceStartPending      = 2
	serviceStopPending       = 3
	serviceRunning           = 4
	serviceAcceptStop        = 0x1
	serviceAcceptShutdown    = 0x4
	serviceAcceptPreshutdown = 0x100
	controlStop              = 1
	controlInterrogate       = 4
	controlShutdown          = 5
	controlPreshutdown       = 0xF
)

type queryServiceConfig struct {
	ServiceType      uint32
	StartType        uint32
	ErrorControl     uint32
	BinaryPathName   *uint16
	LoadOrderGroup   *uint16
	TagID            uint32
	Dependencies     *uint16
	ServiceStartName *uint16
	DisplayName      *uint16
}

type serviceDescription struct{ Description *uint16 }

type scAction struct{ Type, Delay uint32 }

type serviceFailureActions struct {
	ResetPeriod  uint32
	RebootMsg    *uint16
	Command      *uint16
	ActionsCount uint32
	Actions      *scAction
}

type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

type serviceTableEntry struct {
	ServiceName *uint16
	ServiceProc uintptr
}

type scm struct{}

func wstr(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func call(p *syscall.LazyProc, what string, args ...uintptr) (uintptr, error) {
	r, _, err := p.Call(args...)
	if r == 0 {
		if errors.Is(err, errorAccessDenied) {
			return 0, fmt.Errorf("%s: %w", what, ErrAccessDenied)
		}
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return r, nil
}

func closeSvc(h uintptr) { procCloseServiceHandle.Call(h) }

func openSCM(access uint32) (uintptr, error) {
	return call(procOpenSCManagerW, "OpenSCManager", 0, 0, uintptr(access))
}

func startType(s ServiceStart) uint32 {
	switch s {
	case StartDisabled:
		return serviceDisabled
	case StartDemand:
		return serviceDemandStart
	}
	return serviceAutoStart
}

func account(a string) *uint16 {
	if a == "" || a == "LocalSystem" {
		return nil
	}
	return wstr(a)
}

func (scm) Query(name string) (ServiceState, error) {
	m, err := openSCM(scManagerConnect)
	if err != nil {
		return ServiceState{}, err
	}
	defer closeSvc(m)
	h, _, err := procOpenServiceW.Call(m, uintptr(unsafe.Pointer(wstr(name))), serviceQueryConfig|serviceQueryStatus)
	if h == 0 {
		if errors.Is(err, errServiceDoesNotExist) {
			return ServiceState{}, nil
		}
		return ServiceState{}, fmt.Errorf("OpenService %s: %w", name, err)
	}
	defer closeSvc(h)
	st := ServiceState{Exists: true, Config: ServiceConfig{Name: name}}
	buf, err := queryBuf(func(b *byte, n uint32, need *uint32) (uintptr, uintptr, error) {
		return procQueryServiceConfigW.Call(h, uintptr(unsafe.Pointer(b)), uintptr(n), uintptr(unsafe.Pointer(need)))
	})
	if err != nil {
		return st, fmt.Errorf("QueryServiceConfig: %w", err)
	}
	qc := (*queryServiceConfig)(unsafe.Pointer(&buf[0]))
	st.Config.BinaryPath = utf16Ptr(qc.BinaryPathName)
	st.Config.Account = utf16Ptr(qc.ServiceStartName)
	st.Config.DisplayName = utf16Ptr(qc.DisplayName)
	switch qc.StartType {
	case serviceAutoStart:
		st.Config.Start = StartAuto
	case serviceDisabled:
		st.Config.Start = StartDisabled
	default:
		st.Config.Start = StartDemand
	}
	if b, err := queryBuf(func(b *byte, n uint32, need *uint32) (uintptr, uintptr, error) {
		return procQueryServiceConfig2W.Call(h, configDelayed, uintptr(unsafe.Pointer(b)), uintptr(n), uintptr(unsafe.Pointer(need)))
	}); err == nil {
		st.Config.Delayed = *(*int32)(unsafe.Pointer(&b[0])) != 0
	}
	if b, err := queryBuf(func(b *byte, n uint32, need *uint32) (uintptr, uintptr, error) {
		return procQueryServiceConfig2W.Call(h, configDescription, uintptr(unsafe.Pointer(b)), uintptr(n), uintptr(unsafe.Pointer(need)))
	}); err == nil {
		st.Config.Description = utf16Ptr((*serviceDescription)(unsafe.Pointer(&b[0])).Description)
	}
	if b, err := queryBuf(func(b *byte, n uint32, need *uint32) (uintptr, uintptr, error) {
		return procQueryServiceConfig2W.Call(h, configFailure, uintptr(unsafe.Pointer(b)), uintptr(n), uintptr(unsafe.Pointer(need)))
	}); err == nil {
		fa := (*serviceFailureActions)(unsafe.Pointer(&b[0]))
		if fa.ActionsCount > 0 && fa.Actions != nil && fa.Actions.Type == scActionRestart {
			st.Config.RestartDelay = time.Duration(fa.Actions.Delay) * time.Millisecond
		}
	}
	var ss serviceStatus
	if r, _, _ := procQueryServiceStatus.Call(h, uintptr(unsafe.Pointer(&ss))); r != 0 {
		st.State = stateName(ss.CurrentState)
		st.Running = ss.CurrentState == serviceRunning
	}
	return st, nil
}

// queryBuf runs a Query*Config call twice: once for the size, once with a buffer of that size.
func queryBuf(q func(b *byte, n uint32, need *uint32) (uintptr, uintptr, error)) ([]byte, error) {
	var need uint32
	var one [8]byte
	r, _, err := q(&one[0], 0, &need)
	if r != 0 {
		return one[:], nil
	}
	if !errors.Is(err, errInsufficientBuffer) || need == 0 {
		return nil, err
	}
	buf := make([]byte, need+8)
	if r, _, err := q(&buf[0], need, &need); r == 0 {
		return nil, err
	}
	return buf, nil
}

func stateName(s uint32) string {
	switch s {
	case serviceStopped:
		return "stopped"
	case serviceStartPending:
		return "start-pending"
	case serviceStopPending:
		return "stop-pending"
	case serviceRunning:
		return "running"
	}
	return fmt.Sprintf("state-%d", s)
}

func (scm) Create(c ServiceConfig) error {
	m, err := openSCM(scManagerConnect | scManagerCreateService)
	if err != nil {
		return err
	}
	defer closeSvc(m)
	h, err := call(procCreateServiceW, "CreateService "+c.Name, m, uintptr(unsafe.Pointer(wstr(c.Name))), uintptr(unsafe.Pointer(wstr(c.DisplayName))),
		serviceAllAccess, serviceWin32OwnProcess, uintptr(startType(c.Start)), serviceErrorNormal,
		uintptr(unsafe.Pointer(wstr(c.BinaryPath))), 0, 0, 0, uintptr(unsafe.Pointer(account(c.Account))), 0)
	if err != nil {
		return err
	}
	defer closeSvc(h)
	return configure2(h, c)
}

func (scm) Update(c ServiceConfig) error {
	return withService(c.Name, serviceChangeConfig|serviceQueryConfig, func(h uintptr) error {
		if _, err := call(procChangeServiceConfigW, "ChangeServiceConfig "+c.Name, h, serviceWin32OwnProcess, uintptr(startType(c.Start)), serviceErrorNormal,
			uintptr(unsafe.Pointer(wstr(c.BinaryPath))), 0, 0, 0, uintptr(unsafe.Pointer(account(c.Account))), 0, uintptr(unsafe.Pointer(wstr(c.DisplayName)))); err != nil {
			return err
		}
		return configure2(h, c)
	})
}

func (scm) SetStart(name string, start ServiceStart, delayed bool) error {
	return withService(name, serviceChangeConfig, func(h uintptr) error {
		if _, err := call(procChangeServiceConfigW, "ChangeServiceConfig "+name, h, serviceNoChange, uintptr(startType(start)), serviceNoChange, 0, 0, 0, 0, 0, 0, 0); err != nil {
			return err
		}
		if start == StartAuto {
			return setDelayed(h, delayed)
		}
		return nil
	})
}

func (scm) Delete(name string) error {
	return withService(name, accessDelete, func(h uintptr) error {
		_, err := call(procDeleteService, "DeleteService "+name, h)
		return err
	})
}

func withService(name string, access uint32, fn func(h uintptr) error) error {
	m, err := openSCM(scManagerConnect)
	if err != nil {
		return err
	}
	defer closeSvc(m)
	h, err := call(procOpenServiceW, "OpenService "+name, m, uintptr(unsafe.Pointer(wstr(name))), uintptr(access))
	if err != nil {
		return err
	}
	defer closeSvc(h)
	return fn(h)
}

func setDelayed(h uintptr, on bool) error {
	v := int32(0)
	if on {
		v = 1
	}
	_, err := call(procChangeServiceConfig2W, "delayed auto start", h, configDelayed, uintptr(unsafe.Pointer(&v)))
	return err
}

// configure2: description, delayed auto start, and the recovery actions (restart after
// RestartDelay, three times, counter reset after a day; also on a non-zero exit code).
func configure2(h uintptr, c ServiceConfig) error {
	d := serviceDescription{Description: wstr(c.Description)}
	if _, err := call(procChangeServiceConfig2W, "description", h, configDescription, uintptr(unsafe.Pointer(&d))); err != nil {
		return err
	}
	if c.Start == StartAuto {
		if err := setDelayed(h, c.Delayed); err != nil {
			return err
		}
	}
	if c.RestartDelay > 0 {
		ms := uint32(c.RestartDelay / time.Millisecond)
		acts := [3]scAction{{scActionRestart, ms}, {scActionRestart, ms}, {scActionRestart, ms}}
		fa := serviceFailureActions{ResetPeriod: failureResetPeriod, ActionsCount: 3, Actions: &acts[0]}
		if _, err := call(procChangeServiceConfig2W, "failure actions", h, configFailure, uintptr(unsafe.Pointer(&fa))); err != nil {
			return err
		}
		flag := int32(1)
		if _, err := call(procChangeServiceConfig2W, "failure actions flag", h, configFailureFlag, uintptr(unsafe.Pointer(&flag))); err != nil {
			return err
		}
	}
	return nil
}

// ---- running under the service control manager ----

var svc struct {
	once    sync.Once
	mainCB  uintptr
	ctlCB   uintptr
	name    string
	fn      func(ctx context.Context) int
	handle  uintptr
	cancel  context.CancelFunc
	mu      sync.Mutex
	status  serviceStatus
	stopped chan struct{}
}

func setStatus(state, exit uint32) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	s := &svc.status
	s.ServiceType = serviceWin32OwnProcess
	s.CurrentState = state
	s.ControlsAccepted = 0
	if state == serviceRunning {
		s.ControlsAccepted = serviceAcceptStop | serviceAcceptShutdown | serviceAcceptPreshutdown
	}
	s.Win32ExitCode, s.ServiceSpecificExitCode = 0, 0
	if exit != 0 {
		s.Win32ExitCode, s.ServiceSpecificExitCode = errServiceSpecificError, exit
	}
	s.CheckPoint, s.WaitHint = 0, 0
	if state == serviceStartPending || state == serviceStopPending {
		s.WaitHint = 30000
	}
	procSetServiceStatus.Call(svc.handle, uintptr(unsafe.Pointer(s)))
}

// bumpCheckpoint keeps a long ordered stop alive in the manager's eyes.
func bumpCheckpoint() {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.status.CheckPoint++
	procSetServiceStatus.Call(svc.handle, uintptr(unsafe.Pointer(&svc.status)))
}

func serviceCtl(ctl, _, _, _ uintptr) uintptr {
	switch ctl {
	case controlStop, controlShutdown, controlPreshutdown:
		setStatus(serviceStopPending, 0)
		if svc.cancel != nil {
			svc.cancel()
		}
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-svc.stopped:
					return
				case <-t.C:
					bumpCheckpoint()
				}
			}
		}()
	case controlInterrogate:
		svc.mu.Lock()
		procSetServiceStatus.Call(svc.handle, uintptr(unsafe.Pointer(&svc.status)))
		svc.mu.Unlock()
	}
	return 0
}

func serviceMain(_, _ uintptr) uintptr {
	h, _, _ := procRegisterServiceCtrlHandlerEx.Call(uintptr(unsafe.Pointer(wstr(svc.name))), svc.ctlCB, 0)
	if h == 0 {
		return 0
	}
	svc.handle = h
	setStatus(serviceStartPending, 0)
	ctx, cancel := context.WithCancel(context.Background())
	svc.cancel = cancel
	setStatus(serviceRunning, 0)
	code := svc.fn(ctx)
	cancel()
	close(svc.stopped)
	setStatus(serviceStopped, uint32(code))
	return 0
}

func runService(name string, fn func(ctx context.Context) int) error {
	svc.once.Do(func() {
		svc.mainCB = syscall.NewCallback(serviceMain)
		svc.ctlCB = syscall.NewCallback(serviceCtl)
	})
	svc.name, svc.fn, svc.stopped = name, fn, make(chan struct{})
	table := []serviceTableEntry{{ServiceName: wstr(name), ServiceProc: svc.mainCB}, {}}
	r, _, err := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r == 0 {
		if errors.Is(err, errFailedServiceConnect) {
			return ErrNotService
		}
		return fmt.Errorf("StartServiceCtrlDispatcher: %w", err)
	}
	return nil
}

// utf16Ptr reads a NUL-terminated UTF-16 string the system returned.
func utf16Ptr(p *uint16) string {
	if p == nil {
		return ""
	}
	var s []uint16
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		s = append(s, *(*uint16)(ptr))
	}
	return syscall.UTF16ToString(s)
}
