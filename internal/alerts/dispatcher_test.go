package alerts

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/clock"
)

func ev(id int) Event { return Event{ID: id, Severity: Error, Title: "TEST", Message: "TEST"} }

func TestRaiseDeliversToEnabledChannelsOnly(t *testing.T) {
	lg := newTestLog()
	a := &fakeChannel{name: "a"}
	b := &fakeChannel{name: "b"}
	off := &fakeChannel{name: "off", enabled: func() bool { return false }}
	d := New(Config{}, []Channel{a, b, off}, lg.For("alerts"))
	d.Raise(ev(100))
	d.Raise(ev(101))
	closeD(t, d)
	for _, c := range []*fakeChannel{a, b} {
		if got := ids(c.delivered()); len(got) != 2 || got[0] != 100 || got[1] != 101 {
			t.Errorf("%s got %v, want [100 101] in order", c.name, got)
		}
	}
	if off.attempts() != 0 {
		t.Errorf("disabled channel was called")
	}
	// the log channel is always on and records at ERROR with audit=true
	l := lg.lines("ALERT TEST - TEST", "ERROR", "audit=true", "action=alert.deliver", "target=log")
	if len(l) != 2 {
		t.Errorf("log channel records: %v", lg.mem.Lines())
	}
	if len(lg.audit.Lines()) < 2 {
		t.Errorf("audit sink got %v", lg.audit.Lines())
	}
	// a raise record and delivery records with the contract fields
	if len(lg.lines("action=alert.raise", "result=ok")) != 2 {
		t.Errorf("raise records: %v", lg.mem.Lines())
	}
	if len(lg.lines("action=alert.deliver", "target=a", "result=ok", "duration_ms=")) != 2 {
		t.Errorf("deliver records: %v", lg.mem.Lines())
	}
}

func TestLogChannelLevelsBySeverity(t *testing.T) {
	lg := newTestLog()
	c := newLogChannel(lg.For("alerts"))
	for _, s := range []Severity{Error, Warning, Info} {
		if err := c.Send(context.Background(), Event{ID: 1, Severity: s, Title: "t" + s.String()}); err != nil {
			t.Fatal(err)
		}
	}
	for sev, lvl := range map[string]string{"tError": "ERROR", "tWarning": "WARN", "tInfo": "INFO"} {
		if len(lg.lines(sev, lvl, "audit=true")) != 1 {
			t.Errorf("%s not at %s: %v", sev, lvl, lg.mem.Lines())
		}
	}
}

// A stalled channel must not delay another (one worker per channel).
func TestSlowChannelDoesNotDelayAnother(t *testing.T) {
	release := make(chan struct{})
	slow := &fakeChannel{name: "slow", send: func(ctx context.Context, _ Event) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	fast := &fakeChannel{name: "fast"}
	d := New(Config{TimeoutSeconds: 30}, []Channel{slow, fast}, newTestLog().For("alerts"))
	start := time.Now()
	for i := 0; i < 5; i++ {
		d.Raise(ev(100 + i))
	}
	eventually(t, "fast channel to get 5 events while slow is stalled", func() bool { return len(fast.delivered()) == 5 })
	if time.Since(start) > 2*time.Second {
		t.Errorf("fast channel took %v", time.Since(start))
	}
	if len(slow.delivered()) != 0 {
		t.Errorf("slow channel should still be stalled")
	}
	close(release)
	closeD(t, d)
	if len(slow.delivered()) != 5 {
		t.Errorf("slow channel delivered %d after release", len(slow.delivered()))
	}
}

func TestRaiseNeverBlocksWithAStalledChannel(t *testing.T) {
	stall := make(chan struct{})
	slow := &fakeChannel{name: "slow", send: func(ctx context.Context, _ Event) error {
		select {
		case <-stall:
		case <-ctx.Done():
		}
		return nil
	}}
	// A discarding logger: the cost measured is the queue's. A logx record costs about 25 us
	// (0.4 ms under -race, redaction runs before the level filter) and is not what this test is about.
	d := New(Config{QueueSize: 64, TimeoutSeconds: 30}, []Channel{slow}, nil)
	start := time.Now()
	for i := 0; i < 10000; i++ {
		d.Raise(Event{ID: 110, Severity: Warning, Title: "TEST", Message: "TEST"})
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("10000 raises took %v with a stalled channel", el)
	}
	close(stall)
	closeD(t, d)
}

func TestOverflowDropsOldestWithOneNoticePerEpisode(t *testing.T) {
	lg := newTestLog()
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	var once sync.Once
	ch := &fakeChannel{name: "c", send: func(ctx context.Context, _ Event) error {
		started <- struct{}{}
		once.Do(func() { <-release })
		return nil
	}}
	d := New(Config{QueueSize: 4, TimeoutSeconds: 30}, []Channel{ch}, lg.For("alerts"))

	d.Raise(ev(1))
	<-started // the worker holds event 1; the queue is empty
	for i := 2; i <= 11; i++ {
		d.Raise(ev(i))
	}
	notices := lg.lines("alert queue full", "target=c")
	if len(notices) != 1 {
		t.Fatalf("want exactly one notice for the episode, got %d: %v", len(notices), notices)
	}
	close(release)
	eventually(t, "queue to drain", func() bool { return len(ch.delivered()) == 5 })
	if got := ids(ch.delivered()); len(got) != 5 || got[0] != 1 || got[1] != 8 || got[4] != 11 {
		t.Errorf("delivered %v, want [1 8 9 10 11] (oldest dropped)", got)
	}
	eventually(t, "episode summary", func() bool { return len(lg.lines("alert queue drained", "dropped=6")) == 1 })

	// a new episode is announced again
	block := make(chan struct{})
	ch.mu.Lock()
	ch.send = func(ctx context.Context, _ Event) error { <-block; return nil }
	ch.mu.Unlock()
	d.Raise(ev(20))
	eventually(t, "worker to take 20", func() bool { return ch.attempts() >= 6 })
	for i := 21; i <= 30; i++ {
		d.Raise(ev(i))
	}
	if n := len(lg.lines("alert queue full", "target=c")); n != 2 {
		t.Errorf("want a second notice for the second episode, total %d", n)
	}
	close(block)
	closeD(t, d)
}

func TestPerChannelTimeoutAndPanicAreContained(t *testing.T) {
	lg := newTestLog()
	hang := make(chan struct{})
	var n int
	var mu sync.Mutex
	ch := &fakeChannel{name: "c", send: func(ctx context.Context, e Event) error {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		switch k {
		case 1:
			<-hang // ignores its context entirely
		case 2:
			panic("boom")
		}
		return nil
	}}
	d := New(Config{TimeoutSeconds: 0.05}, []Channel{ch}, lg.For("alerts"))
	d.Raise(ev(1))
	d.Raise(ev(2))
	d.Raise(ev(3))
	eventually(t, "third event despite a hung and a panicking Send", func() bool { return len(ch.delivered()) == 1 })
	if len(lg.lines("WARN", "action=alert.deliver", "target=c", "result=fail", "timed out")) != 1 {
		t.Errorf("no timeout WARN: %v", lg.mem.Lines())
	}
	if len(lg.lines("WARN", "channel panicked: boom")) != 1 {
		t.Errorf("no panic WARN: %v", lg.mem.Lines())
	}
	close(hang)
	closeD(t, d)
}

func waitSleepers(t *testing.T, f *clock.Fake, n int) {
	t.Helper()
	eventually(t, "retry wait", func() bool { return f.Sleepers() == n })
}

func TestNetworkChannelRetriesOnTheBackoffSchedule(t *testing.T) {
	lg := newTestLog()
	fk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	ch := &fakeChannel{name: "webhook", network: true}
	ch.send = func(ctx context.Context, _ Event) error {
		if ch.attempts() < 3 { // attempts is incremented before send
			return errors.New("connection refused")
		}
		return nil
	}
	d := NewWith(Config{}, []Channel{ch}, lg.For("alerts"), WithClock(fk))
	d.Raise(ev(100))
	waitSleepers(t, fk, 1)
	if ch.attempts() != 1 {
		t.Fatalf("attempts %d", ch.attempts())
	}
	fk.Advance(999 * time.Millisecond) // not yet 1 s
	time.Sleep(20 * time.Millisecond)
	if ch.attempts() != 1 {
		t.Fatalf("retried before the 1 s backoff")
	}
	fk.Advance(time.Millisecond)
	eventually(t, "attempt 2", func() bool { return ch.attempts() == 2 })
	waitSleepers(t, fk, 1)
	fk.Advance(4 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if ch.attempts() != 2 {
		t.Fatalf("second wait must be 5 s")
	}
	fk.Advance(time.Second)
	eventually(t, "attempt 3 delivers", func() bool { return len(ch.delivered()) == 1 })
	if n := len(lg.lines("WARN", "target=webhook", "result=fail")); n != 2 {
		t.Errorf("%d WARNs, want 2", n)
	}
	if len(lg.lines("ERROR", "giving up")) != 0 {
		t.Errorf("unexpected ERROR")
	}
	closeD(t, d)
}

func TestRetriesExhaustedDropWithError(t *testing.T) {
	lg := newTestLog()
	fk := clock.NewFake(time.Now())
	ch := &fakeChannel{name: "push", network: true, send: func(context.Context, Event) error { return errors.New("HTTP 500") }}
	d := NewWith(Config{}, []Channel{ch}, lg.For("alerts"), WithClock(fk))
	d.Raise(ev(100))
	for i, wait := range []time.Duration{time.Second, 5 * time.Second, 30 * time.Second} {
		waitSleepers(t, fk, 1)
		if ch.attempts() != i+1 {
			t.Fatalf("before wait %d: attempts %d", i, ch.attempts())
		}
		fk.Advance(wait)
	}
	eventually(t, "4th attempt and the drop", func() bool { return len(lg.lines("ERROR", "giving up", "target=push")) == 1 })
	if ch.attempts() != 4 {
		t.Errorf("attempts %d, want 1 + 3 retries", ch.attempts())
	}
	if n := len(lg.lines("WARN", "target=push", "result=fail")); n != 3 {
		t.Errorf("%d WARNs before the final ERROR, want 3", n)
	}
	closeD(t, d)
}

func TestNonNetworkChannelIsNotRetried(t *testing.T) {
	lg := newTestLog()
	ch := &fakeChannel{name: "desktop", send: func(context.Context, Event) error { return errors.New("no display") }}
	d := New(Config{}, []Channel{ch}, lg.For("alerts"))
	d.Raise(ev(100))
	closeD(t, d)
	if ch.attempts() != 1 {
		t.Errorf("attempts %d", ch.attempts())
	}
	if len(lg.lines("WARN", "target=desktop", "result=fail", "no display")) != 1 {
		t.Errorf("%v", lg.mem.Lines())
	}
}

func TestCloseDuringRetryWaitDoesNotWait(t *testing.T) {
	fk := clock.NewFake(time.Now())
	ch := &fakeChannel{name: "w", network: true, send: func(context.Context, Event) error { return errors.New("down") }}
	d := NewWith(Config{}, []Channel{ch}, newTestLog().For("alerts"), WithClock(fk))
	d.Raise(ev(100))
	waitSleepers(t, fk, 1)
	start := time.Now()
	closeD(t, d) // the fake clock never advances: Close must not wait for the backoff
	if time.Since(start) > 2*time.Second {
		t.Errorf("Close waited %v", time.Since(start))
	}
}

func TestDedupeByKeyWithinMinRepeat(t *testing.T) {
	lg := newTestLog()
	fk := clock.NewFake(time.Now())
	ch := &fakeChannel{name: "c"}
	d := NewWith(Config{}, []Channel{ch}, lg.For("alerts"), WithClock(fk))
	k := func(key string) Event { e := ev(110); e.Key = key; return e }
	d.Raise(k("110:cam1"))
	d.Raise(k("110:cam1")) // identical Key within 60 s: dropped
	d.Raise(k("110:cam2")) // other key
	d.Raise(ev(100))       // no key: never deduplicated
	d.Raise(ev(100))
	fk.Advance(59 * time.Second)
	d.Raise(k("110:cam1")) // still inside the window
	fk.Advance(time.Second)
	d.Raise(k("110:cam1")) // 60 s: allowed again
	closeD(t, d)
	if got := len(ch.delivered()); got != 5 {
		t.Errorf("delivered %d, want 5 (cam1, cam2, 100, 100, cam1 again)", got)
	}
	if n := len(lg.lines("result=deduped")); n != 2 {
		t.Errorf("%d dedupe records, want 2", n)
	}
}

func TestDedupeWindowIsConfigurable(t *testing.T) {
	fk := clock.NewFake(time.Now())
	ch := &fakeChannel{name: "c"}
	d := NewWith(Config{MinRepeatSeconds: 10}, []Channel{ch}, nil, WithClock(fk))
	e := ev(110)
	e.Key = "k"
	d.Raise(e)
	fk.Advance(10 * time.Second)
	d.Raise(e)
	closeD(t, d)
	if len(ch.delivered()) != 2 {
		t.Errorf("%d", len(ch.delivered()))
	}
}

func TestCloseFlushesAndIsIdempotent(t *testing.T) {
	ch := &fakeChannel{name: "c", send: func(context.Context, Event) error { time.Sleep(5 * time.Millisecond); return nil }}
	d := New(Config{}, []Channel{ch}, newTestLog().For("alerts"))
	for i := 0; i < 10; i++ {
		d.Raise(ev(100 + i))
	}
	closeD(t, d)
	if len(ch.delivered()) != 10 {
		t.Errorf("flushed %d of 10", len(ch.delivered()))
	}
	closeD(t, d)
	d.Raise(ev(1)) // after Close: ignored, no panic
	if len(ch.delivered()) != 10 {
		t.Errorf("event accepted after Close")
	}
}

func TestCloseDeadlineAbandonsStuckDelivery(t *testing.T) {
	lg := newTestLog()
	ch := &fakeChannel{name: "c", send: func(ctx context.Context, _ Event) error { <-ctx.Done(); return ctx.Err() }}
	d := New(Config{TimeoutSeconds: 60}, []Channel{ch}, lg.For("alerts"))
	for i := 0; i < 3; i++ {
		d.Raise(ev(100 + i))
	}
	eventually(t, "worker busy", func() bool { return ch.attempts() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := d.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Close took %v", time.Since(start))
	}
	if len(lg.lines("WARN", "not delivered before the shutdown deadline", "target=c")) != 1 {
		t.Errorf("no shutdown notice: %v", lg.mem.Lines())
	}
}

func TestNoGoroutineLeakAfterClose(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()
	for round := 0; round < 3; round++ {
		fail := &fakeChannel{name: "fail", network: true, send: func(context.Context, Event) error { return errors.New("x") }}
		slow := &fakeChannel{name: "slow", send: func(ctx context.Context, _ Event) error { time.Sleep(time.Millisecond); return nil }}
		d := New(Config{RetryBackoffSeconds: []float64{0.001}}, []Channel{fail, slow}, newTestLog().For("alerts"))
		for i := 0; i < 20; i++ {
			d.Raise(ev(100))
		}
		d.Test(context.Background(), "")
		closeD(t, d)
	}
	eventually(t, "goroutines to return to the baseline", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before
	})
}

func TestDisabledAtStartIsNeverStarted(t *testing.T) {
	lg := newTestLog()
	off := &fakeChannel{name: "off", enabled: func() bool { return false }}
	d := New(Config{}, []Channel{off}, lg.For("alerts"))
	d.Raise(ev(100))
	closeD(t, d)
	if off.attempts() != 0 || len(lg.lines("disabled")) == 0 {
		t.Errorf("attempts %d, log %v", off.attempts(), lg.mem.Lines())
	}
}

func TestChannelThatDisablesItselfIsSkipped(t *testing.T) {
	var mu sync.Mutex
	on := true
	ch := &fakeChannel{name: "c", enabled: func() bool { mu.Lock(); defer mu.Unlock(); return on },
		send: func(context.Context, Event) error {
			mu.Lock()
			on = false
			mu.Unlock()
			return errors.New("cannot open")
		}}
	d := New(Config{}, []Channel{ch}, newTestLog().For("alerts"))
	d.Raise(ev(1))
	eventually(t, "first attempt", func() bool { return ch.attempts() == 1 })
	d.Raise(ev(2))
	closeD(t, d)
	if ch.attempts() != 1 {
		t.Errorf("attempts %d", ch.attempts())
	}
}

func TestTestSendsLabelledEventAndReportsPerChannel(t *testing.T) {
	lg := newTestLog()
	good := &fakeChannel{name: "good"}
	bad := &fakeChannel{name: "bad", send: func(context.Context, Event) error { return errors.New("HTTP 500") }}
	off := &fakeChannel{name: "off", enabled: func() bool { return false }}
	d := New(Config{}, []Channel{good, bad, off}, lg.For("alerts"))
	res := d.Test(context.Background(), "")
	by := map[string]Result{}
	for _, r := range res {
		by[r.Channel] = r
	}
	if len(res) != 4 || by["log"].Err != nil || by["good"].Err != nil || by["bad"].Err == nil || by["off"].Err == nil || by["off"].Enabled {
		t.Errorf("%+v", res)
	}
	got := good.delivered()
	if len(got) != 1 || !strings.HasPrefix(got[0].Title, "TEST") || !strings.Contains(got[0].Message, "TEST") || got[0].ID != IDTest {
		t.Errorf("test event not labelled TEST: %+v", got)
	}
	if one := d.Test(context.Background(), "good"); len(one) != 1 || one[0].Err != nil || good.attempts() != 2 {
		t.Errorf("single channel test: %+v", one)
	}
	if nope := d.Test(context.Background(), "nope"); len(nope) != 1 || nope[0].Err == nil {
		t.Errorf("unknown channel: %+v", nope)
	}
	// Test bypasses dedupe
	d.Test(context.Background(), "good")
	if good.attempts() != 3 {
		t.Errorf("attempts %d", good.attempts())
	}
	closeD(t, d)
}

func TestDispatcherIsSafeWithNilsAndPanickingChannelMethods(t *testing.T) {
	d := New(Config{}, []Channel{nil, &panicky{}}, nil)
	d.Raise(ev(1))
	d.RaiseFrom(prodEvent{ID: 110, Severity: "Warning"})
	closeD(t, d)
}

type panicky struct{}

func (*panicky) Name() string                      { return "p" }
func (*panicky) Enabled() bool                     { panic("enabled") }
func (*panicky) Send(context.Context, Event) error { return nil }

// Secrets: Title, Message and Camera are redacted before any channel sees them, and the logs are masked.
func TestRedactionInChannelsAndLogs(t *testing.T) {
	const secret = "s3cr3t-Pa55w0rd"
	lg := newTestLog()
	lg.Redactor().Register(secret)
	ch := &fakeChannel{name: "c"}
	d := NewWith(Config{}, []Channel{ch}, lg.For("alerts"), WithRedactor(lg.Redactor().String))
	d.Raise(Event{ID: 110, Severity: Warning, Title: "login " + secret, Message: "rtsp://admin:" + secret + "@10.0.0.9/stream failed with " + secret, Camera: "cam-" + secret, Key: "k-" + secret})
	closeD(t, d)

	got := ch.delivered()
	if len(got) != 1 {
		t.Fatal("not delivered")
	}
	all := got[0].Title + got[0].Message + got[0].Camera
	if strings.Contains(all, secret) || !strings.Contains(all, "***") {
		t.Errorf("channel saw %q", all)
	}
	if strings.Contains(lg.mem.String(), secret) || strings.Contains(lg.audit.String(), secret) {
		t.Errorf("secret reached the log:\n%s", lg.mem.String())
	}
	if !strings.Contains(lg.mem.String(), "***") {
		t.Errorf("expected a mask in the log")
	}

	// positive control: the same event with nothing registered and no redactor DOES leak, so the
	// assertions above can fail
	lg2 := newTestLog()
	ch2 := &fakeChannel{name: "c"}
	d2 := New(Config{}, []Channel{ch2}, lg2.For("alerts"))
	d2.Raise(Event{ID: 110, Severity: Warning, Title: "login " + secret, Message: "x " + secret})
	closeD(t, d2)
	if !strings.Contains(lg2.mem.String(), secret) || !strings.Contains(ch2.delivered()[0].Title, secret) {
		t.Errorf("positive control did not leak; the redaction assertions prove nothing")
	}
}

func TestRedactionOfWebhookURLQueryInLogs(t *testing.T) {
	const secret = "qs3cretvalue99"
	lg := newTestLog()
	addr := refusedAddr(t)
	wh, err := NewWebhook(WebhookConfig{Enabled: true, URL: "http://" + addr + "/hook/abc" + secret + "?token=" + secret + "&x=1"}, nil, "h")
	if err != nil {
		t.Fatal(err)
	}
	d := NewWith(Config{RetryBackoffSeconds: []float64{}}, []Channel{wh}, lg.For("alerts"))
	d.Raise(ev(100))
	closeD(t, d)
	if len(lg.lines("target=webhook", "result=fail")) != 1 {
		t.Fatalf("no failure logged: %v", lg.mem.Lines())
	}
	if strings.Contains(lg.mem.String(), secret) {
		t.Errorf("URL secret in log:\n%s", lg.mem.String())
	}
	// positive control: the raw net/http error does carry the URL
	_, rawErr := http.Post("http://"+addr+"/hook?token="+secret, "text/plain", nil)
	if rawErr == nil || !strings.Contains(rawErr.Error(), secret) {
		t.Errorf("positive control: raw error %v does not contain the secret", rawErr)
	}
}
