//go:build unix

package osx

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// OnConsoleClose calls fn for every SIGHUP (hangup), SIGTERM (terminate) and SIGINT (interrupt),
// one call at a time on one goroutine, until stop is called. fn should hand the event to the
// caller's own loop and return.
func OnConsoleClose(fn func(Reason)) (stop func()) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				switch s {
				case syscall.SIGHUP:
					fn(ReasonHangup)
				case syscall.SIGTERM:
					fn(ReasonTerminate)
				default:
					fn(ReasonInterrupt)
				}
			case <-quit:
				return
			}
		}
	}()
	return onceStop(func() { signal.Stop(ch); close(quit) })
}

// WatchConsole is the console probe; on Unix the terminal's death arrives as SIGHUP, so it does
// nothing.
func WatchConsole(fn func(Reason), interval time.Duration) (stop func(), err error) {
	return func() {}, nil
}

// ConsolePresent reports whether this process has a live console. On Unix: stdin is a terminal.
func ConsolePresent() (bool, string) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false, err.Error()
	}
	return fi.Mode()&os.ModeCharDevice != 0, "stdin mode " + fi.Mode().String()
}
