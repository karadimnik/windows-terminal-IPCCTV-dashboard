package clock

import (
	"sort"
	"sync"
	"time"
)

// Fake is a Clock moved by the test. Advance moves the wall and the monotonic readings together;
// StepWall moves only the wall clock (a daylight-saving misreading, an NTP or manual step).
type Fake struct {
	mu      sync.Mutex
	wall    time.Time
	mono    Mono
	waiters []*waiter
}

type waiter struct {
	at Mono
	ch chan time.Time
}

// NewFake starts a fake at the given wall instant (stored in UTC).
func NewFake(start time.Time) *Fake { return &Fake{wall: start.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wall
}

func (f *Fake) Mono() Mono {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mono
}

func (f *Fake) Since(m Mono) time.Duration { return f.Mono().Sub(m) }

func (f *Fake) Sleep(d time.Duration) { <-f.After(d) }

func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.wall
		return ch
	}
	f.waiters = append(f.waiters, &waiter{at: f.mono + Mono(d), ch: ch})
	return ch
}

// Advance moves wall and monotonic time forward by d and releases the sleepers that are due,
// earliest first.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = f.wall.Add(d)
	f.mono += Mono(d)
	sort.SliceStable(f.waiters, func(i, j int) bool { return f.waiters[i].at < f.waiters[j].at })
	keep := f.waiters[:0]
	for _, w := range f.waiters {
		if w.at <= f.mono {
			w.ch <- f.wall
		} else {
			keep = append(keep, w)
		}
	}
	f.waiters = keep
}

// StepWall moves only the wall clock by d (negative for a step back). Monotonic readings, and so
// every interval and sleeper, are unaffected.
func (f *Fake) StepWall(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = f.wall.Add(d)
}

// Sleepers is the number of pending Sleep and After calls (lets a test wait for a goroutine to block).
func (f *Fake) Sleepers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}
