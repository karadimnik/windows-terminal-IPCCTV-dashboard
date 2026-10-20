package status

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeTicker struct{ c chan time.Time }

func (f *fakeTicker) newTicker(time.Duration) (<-chan time.Time, func()) { return f.c, func() {} }

func TestHeartbeaterWritesFirstThenOnEveryTickAndStopsCleanly(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	base := fixture(t, "running-full.json")
	ft := &fakeTicker{c: make(chan time.Time)}
	var mu sync.Mutex
	n := 0.0
	snapshots := make(chan struct{}, 16)
	h := &Heartbeater{
		Writer:    NewWriter(out, Options{}),
		Interval:  5 * time.Second,
		NewTicker: ft.newTicker,
		Snapshot: func() *Document {
			mu.Lock()
			defer mu.Unlock()
			n++
			v := n
			d := *base
			d.UptimeSeconds = &v
			snapshots <- struct{}{}
			return &d
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()

	<-snapshots // first write happens without a tick
	waitUptime := func(want float64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if d, err := Read(out); err == nil && d.UptimeSeconds != nil && *d.UptimeSeconds == want {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("uptime %v never appeared", want)
	}
	waitUptime(1)
	ft.c <- time.Time{}
	<-snapshots
	waitUptime(2)
	ft.c <- time.Time{}
	<-snapshots
	waitUptime(3)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	select {
	case ft.c <- time.Time{}:
		t.Error("tick accepted after stop")
	default:
	}
}

func TestHeartbeaterReportsErrorsAndKeepsGoing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	w := NewWriter(out, Options{RetryDelay: time.Millisecond})
	boom := errors.New("boom")
	fail := true
	w.replace = func(tmp, path string) (bool, error) {
		if fail {
			return false, boom
		}
		return replaceFile(tmp, path)
	}
	ft := &fakeTicker{c: make(chan time.Time)}
	errs := make(chan error, 4)
	doc := fixture(t, "running-full.json")
	h := &Heartbeater{Writer: w, Interval: time.Second, NewTicker: ft.newTicker,
		Snapshot: func() *Document { return doc }, OnError: func(err error) { errs <- err }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	if err := <-errs; !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	fail = false // only touched between cycles: Run is parked on the ticker
	ft.c <- time.Time{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := Read(out); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat never recovered")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestHeartbeaterNilSnapshotSkipsTheCycle(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	ft := &fakeTicker{c: make(chan time.Time)}
	called := make(chan struct{}, 1)
	h := &Heartbeater{Writer: NewWriter(out, Options{}), Interval: time.Second, NewTicker: ft.newTicker,
		Snapshot: func() *Document { called <- struct{}{}; return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	<-called
	cancel()
	<-done
	if _, err := Read(out); !errors.Is(err, ErrNoStatus) {
		t.Errorf("file written for a nil snapshot: %v", err)
	}
}

// Written fires after each successful write only (a failed write calls OnError instead).
func TestHeartbeaterWrittenOnlyAfterSuccess(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	w := NewWriter(out, Options{RetryDelay: time.Millisecond})
	fail := true
	w.replace = func(tmp, path string) (bool, error) {
		if fail {
			return false, errors.New("boom")
		}
		return replaceFile(tmp, path)
	}
	ft := &fakeTicker{c: make(chan time.Time)}
	errs, written := make(chan error, 4), make(chan struct{}, 4)
	doc := fixture(t, "running-full.json")
	h := &Heartbeater{Writer: w, Interval: time.Second, NewTicker: ft.newTicker,
		Snapshot: func() *Document { return doc }, OnError: func(err error) { errs <- err }, Written: func() { written <- struct{}{} }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	<-errs
	select {
	case <-written:
		t.Fatal("Written after a failed write")
	default:
	}
	fail = false
	ft.c <- time.Time{}
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("Written not called after a good write")
	}
	if _, err := Read(out); err != nil {
		t.Fatalf("Written before the file was in place: %v", err)
	}
	cancel()
	<-done
}
