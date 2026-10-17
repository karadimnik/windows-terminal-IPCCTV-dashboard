package main

import (
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Brute-force protection, two independent layers:
//
//   - ipLimiter: a token bucket per client address (the TCP peer, nothing a
//     client can write), so one machine cannot try passwords quickly.
//   - lockout: consecutive failures per USERNAME (names that do not exist
//     included, so the answer never says which names do) with an exponential
//     delay, so many machines together cannot either.
// ---------------------------------------------------------------------------

const (
	loginRatePerMinute = 10
	loginBurst         = 5
	maxIPEntries       = 4096

	lockoutAfter = 5 // failures that are free; the 5th one starts the delay
	lockoutBase  = 30 * time.Second
	lockoutMax   = 15 * time.Minute

	// failureForget is how long a username stays quiet before its counter is dropped.
	failureForget     = 24 * time.Hour
	maxFailureEntries = 4096
)

// lockoutDelay is how long a username is refused after its count-th consecutive
// failure: nothing up to the 4th, then 30 s doubling per further failure, capped.
func lockoutDelay(count int) time.Duration {
	if count < lockoutAfter {
		return 0
	}
	shift := count - lockoutAfter
	if shift > 10 {
		return lockoutMax
	}
	if d := lockoutBase << shift; d < lockoutMax {
		return d
	}
	return lockoutMax
}

// FailureState is one username's run of failures.
type FailureState struct {
	Count       int
	LastFail    time.Time
	LockedUntil time.Time
}

// failureTable is the in-memory failure state. It is bounded: a state is
// forgotten after `forget` of quiet, and when the table is full the oldest
// state that is not currently locked makes room.
type failureTable struct {
	max    int
	forget time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]FailureState
}

func newFailureTable(max int, forget time.Duration, now func() time.Time) *failureTable {
	return &failureTable{max: max, forget: forget, now: now, entries: map[string]FailureState{}}
}

func (t *failureTable) Failure(username string) (FailureState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := normalizeUsername(username)
	st, ok := t.entries[key]
	if ok && t.now().Sub(st.LastFail) >= t.forget {
		delete(t.entries, key)
		return FailureState{}, false
	}
	return st, ok
}

func (t *failureTable) SetFailure(username string, st FailureState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := normalizeUsername(username)
	if _, exists := t.entries[key]; !exists && len(t.entries) >= t.max {
		t.makeRoomLocked()
	}
	t.entries[key] = st
}

func (t *failureTable) ClearFailure(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, normalizeUsername(username))
}

func (t *failureTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

func (t *failureTable) makeRoomLocked() {
	now := t.now()
	for key, st := range t.entries {
		if now.Sub(st.LastFail) >= t.forget {
			delete(t.entries, key)
		}
	}
	if len(t.entries) < t.max {
		return
	}
	var oldest string
	var oldestAt time.Time
	pick := func(skipLocked bool) {
		for key, st := range t.entries {
			if skipLocked && now.Before(st.LockedUntil) {
				continue
			}
			if oldest == "" || st.LastFail.Before(oldestAt) {
				oldest, oldestAt = key, st.LastFail
			}
		}
	}
	pick(true)
	if oldest == "" {
		pick(false)
	}
	delete(t.entries, oldest)
}

// lockout applies the delay schedule to a store's failure state.
type lockout struct {
	store UserStore
	now   func() time.Time
}

func newLockout(store UserStore, now func() time.Time) *lockout {
	return &lockout{store: store, now: now}
}

// locked reports whether username is refused right now and for how much longer.
func (l *lockout) locked(username string) (time.Duration, bool) {
	st, ok := l.store.Failure(username)
	if !ok {
		return 0, false
	}
	if remaining := st.LockedUntil.Sub(l.now()); remaining > 0 {
		return remaining, true
	}
	return 0, false
}

// fail records one failed attempt and starts the delay its count calls for.
func (l *lockout) fail(username string) {
	st, _ := l.store.Failure(username)
	now := l.now()
	st.Count++
	st.LastFail = now
	st.LockedUntil = now.Add(lockoutDelay(st.Count))
	l.store.SetFailure(username, st)
}

func (l *lockout) reset(username string) { l.store.ClearFailure(username) }

// ---------------------------------------------------------------------------

// ipLimiter is a token bucket per client address: `perMinute` tokens a minute,
// up to `burst` banked. It is kept as a "theoretical arrival time" (GCRA), which
// is a token bucket in exact integer time arithmetic. At most maxEntries
// addresses are tracked; a bucket that has been idle long enough to be full again
// is the same as a new one, so those are dropped first, then the least recently used.
type ipLimiter struct {
	interval   time.Duration // one token
	window     time.Duration // burst tokens
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time // address -> theoretical arrival time
}

func newIPLimiter(perMinute, burst, maxEntries int, now func() time.Time) *ipLimiter {
	interval := time.Minute / time.Duration(perMinute)
	return &ipLimiter{interval: interval, window: interval * time.Duration(burst), maxEntries: maxEntries, now: now, entries: map[string]time.Time{}}
}

// allow takes one token for ip, or says how long until there is one.
func (l *ipLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	tat, ok := l.entries[ip]
	if !ok && len(l.entries) >= l.maxEntries {
		l.makeRoomLocked(now)
	}
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(l.interval)
	if over := next.Sub(now) - l.window; over > 0 {
		l.entries[ip] = tat
		return false, over
	}
	l.entries[ip] = next
	return true, 0
}

func (l *ipLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *ipLimiter) makeRoomLocked(now time.Time) {
	for ip, tat := range l.entries {
		if !tat.After(now) {
			delete(l.entries, ip)
		}
	}
	if len(l.entries) < l.maxEntries {
		return
	}
	// Still full: every address is mid-burst. Drop the one that will be free soonest.
	var soonest string
	var soonestAt time.Time
	for ip, tat := range l.entries {
		if soonest == "" || tat.Before(soonestAt) {
			soonest, soonestAt = ip, tat
		}
	}
	delete(l.entries, soonest)
}
