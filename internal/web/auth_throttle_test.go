package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock               { return &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLockoutDelaySchedule(t *testing.T) {
	want := map[int]time.Duration{
		0: 0, 1: 0, 2: 0, 3: 0, 4: 0,
		5: 30 * time.Second, 6: time.Minute, 7: 2 * time.Minute, 8: 4 * time.Minute, 9: 8 * time.Minute,
		10: 15 * time.Minute, 11: 15 * time.Minute, 40: 15 * time.Minute, 1000: 15 * time.Minute, 1 << 30: 15 * time.Minute,
	}
	for count, d := range want {
		if got := lockoutDelay(count); got != d {
			t.Errorf("failure %d: delay %v, want %v", count, got, d)
		}
	}
}

func TestLockoutSequence(t *testing.T) {
	clock := newFakeClock()
	store := newFileUserStore(t.TempDir(), clock.now)
	const name = "anna"
	lock := newLockout(store, clock.now)
	if _, locked := lock.locked(name); locked {
		t.Fatal("locked before any failure")
	}
	for i := 1; i <= 4; i++ {
		lock.fail(name)
		if _, locked := lock.locked(name); locked {
			t.Fatalf("locked after failure %d: the first four are free", i)
		}
	}
	lock.fail(name) // the 5th
	remaining, locked := lock.locked(name)
	if !locked || remaining != 30*time.Second {
		t.Fatalf("5th failure: locked=%v for %v, want 30s", locked, remaining)
	}
	clock.advance(29 * time.Second)
	if remaining, locked := lock.locked(name); !locked || remaining != time.Second {
		t.Errorf("after 29s: %v %v", locked, remaining)
	}
	clock.advance(time.Second)
	if _, locked := lock.locked(name); locked {
		t.Error("still locked after the 30 seconds")
	}
	lock.fail(name) // the 6th
	if remaining, _ := lock.locked(name); remaining != time.Minute {
		t.Errorf("6th failure: %v, want 1m", remaining)
	}
	// Names are compared ignoring case.
	if _, locked := lock.locked("ANNA"); !locked {
		t.Error("the lock does not follow the case-insensitive name")
	}
	// A success resets it all.
	lock.reset(name)
	if _, locked := lock.locked(name); locked {
		t.Error("locked after a reset")
	}
	lock.fail(name)
	if st, _ := store.Failure(name); st.Count != 1 {
		t.Errorf("count after reset + 1 failure = %d", st.Count)
	}
}

func TestLockoutNeverPermanentAndForgottenAfter24h(t *testing.T) {
	clock := newFakeClock()
	store := newFileUserStore(t.TempDir(), clock.now)
	lock := newLockout(store, clock.now)
	for i := 0; i < 50; i++ {
		lock.fail("anna")
		clock.advance(lock.mustRemaining("anna"))
	}
	if remaining, locked := lock.locked("anna"); locked {
		t.Errorf("still locked for %v after waiting each lock out", remaining)
	}
	lock.fail("anna")
	if remaining, _ := lock.locked("anna"); remaining != 15*time.Minute {
		t.Errorf("the cap is %v, want 15m", remaining)
	}
	clock.advance(15*time.Minute + 23*time.Hour)
	if _, ok := store.Failure("anna"); !ok {
		t.Fatal("forgotten before 24 hours of quiet")
	}
	clock.advance(time.Hour + time.Second)
	if _, ok := store.Failure("anna"); ok {
		t.Error("counter survived 24 hours of quiet")
	}
	lock.fail("anna")
	if st, _ := store.Failure("anna"); st.Count != 1 {
		t.Errorf("count restarted at %d, want 1", st.Count)
	}
}

func (l *lockout) mustRemaining(name string) time.Duration {
	d, _ := l.locked(name)
	return d
}

func TestFailureTableIsBounded(t *testing.T) {
	clock := newFakeClock()
	table := newFailureTable(100, 24*time.Hour, clock.now)
	for i := 0; i < 5000; i++ {
		clock.advance(time.Second)
		table.SetFailure(fmt.Sprintf("user%d", i), FailureState{Count: 1, LastFail: clock.now()})
		if table.size() > 100 {
			t.Fatalf("table grew to %d", table.size())
		}
	}
	// The newest are kept, the oldest idle dropped.
	if _, ok := table.Failure("user4999"); !ok {
		t.Error("the newest entry was dropped")
	}
	if _, ok := table.Failure("user0"); ok {
		t.Error("the oldest entry was kept")
	}
	// A locked name is dropped only when everything is locked.
	table = newFailureTable(3, 24*time.Hour, clock.now)
	table.SetFailure("locked", FailureState{Count: 6, LastFail: clock.now(), LockedUntil: clock.now().Add(time.Hour)})
	clock.advance(time.Second)
	table.SetFailure("idle1", FailureState{Count: 1, LastFail: clock.now()})
	clock.advance(time.Second)
	table.SetFailure("idle2", FailureState{Count: 1, LastFail: clock.now()})
	clock.advance(time.Second)
	table.SetFailure("new", FailureState{Count: 1, LastFail: clock.now()})
	if _, ok := table.Failure("locked"); !ok {
		t.Error("a locked name was evicted while idle ones existed")
	}
	if _, ok := table.Failure("idle1"); ok {
		t.Error("the oldest idle entry was kept")
	}
}

func TestIPLimiterTokenBucket(t *testing.T) {
	clock := newFakeClock()
	l := newIPLimiter(10, 5, 4096, clock.now)
	const ip = "192.168.1.20"
	for i := 1; i <= 5; i++ {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("attempt %d of the burst refused", i)
		}
	}
	ok, retry := l.allow(ip)
	if ok || retry < 5*time.Second || retry > 7*time.Second {
		t.Fatalf("6th immediate attempt: ok=%v retry=%v, want refused with about 6s", ok, retry)
	}
	if ok, _ := l.allow("192.168.1.21"); !ok {
		t.Error("another address is affected")
	}
	clock.advance(5 * time.Second)
	if ok, _ := l.allow(ip); ok {
		t.Error("allowed after 5s: the refill is one token per 6s")
	}
	clock.advance(time.Second)
	if ok, _ := l.allow(ip); !ok {
		t.Error("refused after 6s: one token should be back")
	}
	if ok, _ := l.allow(ip); ok {
		t.Error("two attempts on one token")
	}
	// Sustained: over one minute at the full rate, exactly 10 pass (after the burst is spent).
	clock.advance(time.Hour)
	for i := 0; i < 5; i++ {
		l.allow(ip)
	}
	passed := 0
	for s := 0; s < 60; s++ {
		clock.advance(time.Second)
		for k := 0; k < 3; k++ {
			if ok, _ := l.allow(ip); ok {
				passed++
			}
		}
	}
	if passed != 10 {
		t.Errorf("%d attempts passed in a minute of hammering, want 10", passed)
	}
}

func TestIPLimiterIsBoundedAndExpiresIdle(t *testing.T) {
	clock := newFakeClock()
	l := newIPLimiter(10, 5, 1000, clock.now)
	for i := 0; i < 5000; i++ {
		clock.advance(time.Millisecond)
		l.allow(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
		if l.size() > 1000 {
			t.Fatalf("limiter grew to %d", l.size())
		}
	}
	clock.advance(time.Minute)
	l.allow("203.0.113.1")
	if l.size() > 1 {
		// Everything else was idle long enough to be indistinguishable from a new address.
		t.Errorf("idle entries were not expired: %d", l.size())
	}
}

// The production caps, with 5,000 distinct names and addresses each.
func TestProductionCapsHoldUnder5000Distinct(t *testing.T) {
	clock := newFakeClock()
	store := newFileUserStore(t.TempDir(), clock.now)
	lock := newLockout(store, clock.now)
	limiter := newIPLimiter(loginRatePerMinute, loginBurst, maxIPEntries, clock.now)
	huge := strings.Repeat("x", 4000)
	for i := 0; i < 5000; i++ {
		clock.advance(time.Millisecond)
		lock.fail(fmt.Sprintf("name%d-%s", i, huge))
		limiter.allow(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
	}
	if n := store.size(); n > maxFailureEntries {
		t.Errorf("failure table holds %d entries, cap %d", n, maxFailureEntries)
	}
	if n := limiter.size(); n > maxIPEntries {
		t.Errorf("address table holds %d entries, cap %d", n, maxIPEntries)
	}
	for key := range store.entries {
		if len(key) > maxNameKey {
			t.Fatalf("a %d byte key got into the table", len(key))
		}
	}
}
