package clock

import "time"

// Mono is a monotonic reading: the time elapsed since an origin private to the Clock that issued
// it. Only differences between readings of the same Clock are meaningful. (A plain time.Time
// cannot carry a monotonic reading that a fake clock could forge, which is why LLD 2.1's
// Since(time.Time) is expressed over Mono.)
type Mono time.Duration

// Sub is the interval from o to m.
func (m Mono) Sub(o Mono) time.Duration { return time.Duration(m - o) }

// Clock is the time source injected into everything that measures or waits.
type Clock interface {
	// Now is the wall clock in UTC. Use it for persisted instants and for comparison with file times.
	Now() time.Time
	// Mono is the monotonic reading; it never steps with the wall clock.
	Mono() Mono
	// Since is the interval elapsed since a reading of this clock's Mono.
	Since(m Mono) time.Duration
	// Sleep blocks for d of monotonic time.
	Sleep(d time.Duration)
	// After delivers the wall time once d of monotonic time has passed.
	After(d time.Duration) <-chan time.Time
}

// Due reports whether interval has elapsed since last on the monotonic clock.
func Due(c Clock, last Mono, interval time.Duration) bool { return c.Since(last) >= interval }

type realClock struct{ origin time.Time }

// Real is the system clock. Its Mono readings use the runtime's monotonic clock.
func Real() Clock { return realClock{origin: processOrigin} }

var processOrigin = time.Now()

func (realClock) Now() time.Time               { return time.Now().UTC() }
func (r realClock) Mono() Mono                 { return Mono(time.Since(r.origin)) }
func (r realClock) Since(m Mono) time.Duration { return r.Mono().Sub(m) }
func (realClock) Sleep(d time.Duration)        { time.Sleep(d) }
func (realClock) After(d time.Duration) <-chan time.Time {
	out := make(chan time.Time, 1)
	time.AfterFunc(d, func() { out <- time.Now().UTC() })
	return out
}
