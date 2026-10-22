package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/web"
)

func TestHealthSelectionNamesTheValidGroupsAndKeysTheSet(t *testing.T) {
	if _, _, err := healthSelection([]string{"setup", "nope"}); err == nil || !web.IsInputError(err) || !strings.Contains(err.Error(), strings.Join(doctorGroups, ", ")) {
		t.Errorf("unknown group: %v", err)
	}
	all, key, err := healthSelection(nil)
	if err != nil || len(all) != len(doctorGroups) || key != "connections,live,recording,setup,storage,system,web" {
		t.Errorf("all: %v %q %v", all, key, err)
	}
	if _, k, _ := healthSelection([]string{"web", "setup"}); k != "setup,web" {
		t.Errorf("key %q", k)
	}
}

// slowRun counts its runs and holds each until release is closed.
type slowRun struct {
	runs    atomic.Int32
	started chan struct{}
	release chan struct{}
}

func newSlowRun() *slowRun {
	return &slowRun{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (s *slowRun) run(ctx context.Context) doctorReport {
	n := s.runs.Add(1)
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return doctorReport{Schema: -1}
	}
	return doctorReport{Schema: 1, Root: string(rune('a' + n))}
}

func TestHealthConcurrentRequestsShareOneRun(t *testing.T) {
	h := newHealthCache()
	s := newSlowRun()
	var wg sync.WaitGroup
	reps := make([]doctorReport, 3)
	for i := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, _, err := h.get(context.Background(), "k", s.run)
			if err != nil {
				t.Error(err)
			}
			reps[i] = rep
		}()
	}
	<-s.started
	time.Sleep(50 * time.Millisecond) // let the other two join the run in progress
	close(s.release)
	wg.Wait()
	if n := s.runs.Load(); n != 1 {
		t.Errorf("%d runs for 3 concurrent requests", n)
	}
	for _, r := range reps {
		if r.Root != reps[0].Root || r.Schema != 1 {
			t.Errorf("answers differ: %+v", reps)
		}
	}
}

func TestHealthCacheAnswersFor5SecondsPerGroupSet(t *testing.T) {
	h := newHealthCache()
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return clock }
	s := newSlowRun()
	close(s.release)
	get := func(key string) {
		t.Helper()
		if _, _, err := h.get(context.Background(), key, s.run); err != nil {
			t.Fatal(err)
		}
	}
	get("k")
	clock = clock.Add(healthTTL - time.Second)
	get("k")
	if n := s.runs.Load(); n != 1 {
		t.Fatalf("%d runs within the TTL", n)
	}
	get("other") // another group set is not answered by it
	if n := s.runs.Load(); n != 2 {
		t.Fatalf("%d runs after another key", n)
	}
	clock = clock.Add(healthTTL)
	get("other")
	if n := s.runs.Load(); n != 3 {
		t.Fatalf("%d runs after the TTL", n)
	}
	if _, at, _ := h.get(context.Background(), "other", s.run); !at.Equal(clock) {
		t.Errorf("the run time %v, want %v", at, clock)
	}
}

func TestHealthRunEndsWhenTheLastWaiterGoesAway(t *testing.T) {
	h := newHealthCache()
	s := newSlowRun()
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	errs := make(chan error, 2)
	for _, ctx := range []context.Context{ctx1, ctx2} {
		go func() { _, _, err := h.get(ctx, "k", s.run); errs <- err }()
	}
	<-s.started
	time.Sleep(50 * time.Millisecond)
	cancel1() // one browser left: the run goes on for the other
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("first: %v", err)
	}
	select {
	case err := <-errs:
		t.Fatalf("the second request ended too: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel2() // the last one: the checks are cancelled
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("second: %v", err)
	}
	// the cancelled run is not cached: the next request runs again
	close(s.release)
	rep, _, err := h.get(context.Background(), "k", s.run)
	if err != nil || rep.Schema != 1 || s.runs.Load() != 2 {
		t.Errorf("after the cancel: %+v %v, %d runs", rep, err, s.runs.Load())
	}
}

func TestWebRoleEnvironmentIsInfoAndNamesNoValue(t *testing.T) {
	none := sysWebEnv(func(string) string { return "" })
	if len(none) != 1 || none[0].Level != lvInfo || none[0].Title != "environment" || !strings.HasPrefix(none[0].Detail, "environment of the web role: no ") {
		t.Errorf("%+v", none)
	}
	set := sysWebEnv(func(k string) string {
		if k == app.SandboxEnv || k == app.EnvCameraUser {
			return "secret-value"
		}
		return ""
	})
	if len(set) != 1 || set[0].Level != lvInfo || set[0].Fix != "" || strings.Contains(set[0].Detail, "secret-value") ||
		!strings.Contains(set[0].Detail, app.SandboxEnv+" and "+app.EnvCameraUser+" set") {
		t.Errorf("%+v", set)
	}
}
