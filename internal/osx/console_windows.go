//go:build windows

package osx

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Console events (wincon.h).
const (
	ctrlCloseEvent    = 2
	ctrlLogoffEvent   = 5
	ctrlShutdownEvent = 6
)

// lastCtrl is the last close/logoff/shutdown event the raw handler saw, plus one (0 = none).
var lastCtrl atomic.Uint32

var installRaw = sync.OnceFunc(func() {
	// Registered after the Go runtime's own handler, so it runs first (SP2: 0 to 10 ms before the Go
	// signal). It only records which event it was and returns FALSE, so the runtime's handler still
	// turns the event into SIGTERM and keeps the process alive (it blocks) while the cleanup runs.
	cb := syscall.NewCallback(func(ev uint32) uintptr {
		switch ev {
		case ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent:
			lastCtrl.Store(ev + 1)
		}
		return 0
	})
	procSetConsoleCtrlHandler.Call(cb, 1)
})

// OnConsoleClose calls fn for every console request to end: CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT and
// CTRL_SHUTDOWN_EVENT (which the Go runtime delivers as SIGTERM) and Ctrl+C / Ctrl+Break (SIGINT).
// After a close event Windows ends the process about 5.04 s later (SP2) whatever happens, so the
// caller must finish its ordered stop within ConsoleBudget and then exit. One call at a time on
// one goroutine, until stop is called.
func OnConsoleClose(fn func(Reason)) (stop func()) {
	installRaw()
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				if s != syscall.SIGTERM {
					fn(ReasonInterrupt)
					continue
				}
				switch lastCtrl.Load() {
				case ctrlCloseEvent + 1:
					fn(ReasonClose)
				case ctrlLogoffEvent + 1:
					fn(ReasonLogoff)
				case ctrlShutdownEvent + 1:
					fn(ReasonShutdown)
				default:
					fn(ReasonTerminate)
				}
			case <-quit:
				return
			}
		}
	}()
	return onceStop(func() { signal.Stop(ch); close(quit) })
}

// ConsolePresent asks the console three cheap questions (SP2). The console is present while
// GetConsoleProcessList lists at least this process; it is gone when that list is empty AND
// GetConsoleMode(stdin) fails or GetConsoleWindow is 0 (both changed with it in every SP2 run).
func ConsolePresent() (bool, string) {
	var b [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	var m uint32
	modeErr := syscall.GetConsoleMode(syscall.Stdin, &m)
	hw, _, _ := procGetConsoleWindow.Call()
	detail := fmt.Sprintf("GetConsoleProcessList=%d GetConsoleMode(stdin) err=%v GetConsoleWindow=%#x", n, modeErr, hw)
	if n > 0 {
		return true, detail
	}
	return !(modeErr != nil || hw == 0), detail
}

// WatchConsole polls ConsolePresent every interval (ProbeInterval when 0) and calls fn once with
// ReasonConsoleGone when the console host has died: an abrupt death of the terminal sends no event
// at all (SP2 routes 4a/4b). Use it only in attached mode; it refuses to arm without a console.
func WatchConsole(fn func(Reason), interval time.Duration) (stop func(), err error) {
	if ok, detail := ConsolePresent(); !ok {
		return func() {}, fmt.Errorf("%w (%s)", ErrNoConsole, detail)
	}
	if interval <= 0 {
		interval = ProbeInterval
	}
	quit := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if ok, _ := ConsolePresent(); !ok {
					fn(ReasonConsoleGone)
					return
				}
			case <-quit:
				return
			}
		}
	}()
	return onceStop(func() { close(quit) }), nil
}
