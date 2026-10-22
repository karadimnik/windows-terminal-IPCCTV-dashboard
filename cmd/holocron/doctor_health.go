package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"holocron/internal/web"
)

// The Admin tab's Health section runs the doctor table above inside the web role (webadmin.go),
// through buildDoctor with inWeb set, and returns the very document `doctor --json` prints. Nothing
// of the checks or their thresholds is repeated for it.

const (
	// healthBudget bounds one run: the growth sample's own window (growthTimeout) is the longest check.
	healthBudget = growthTimeout
	// healthTTL: a run younger than this answers the next request, so a refresh button and the
	// auto refresh cannot stack runs (each one samples the recordings for 2 to 10 s).
	healthTTL = 5 * time.Second
)

// runHealth is one Health run on e (the web role's env, configuration read again): the doctor
// table inside healthBudget, with the in-web-role differences of drun.inWeb. ctx ends the checks.
func runHealth(ctx context.Context, e *env, sel map[string]bool) doctorReport {
	ctx, cancel := context.WithTimeout(ctx, healthBudget)
	defer cancel()
	return buildDoctor(ctx, e, sel, false, true)
}

// healthSelection turns the requested group names (none: all) into the run's selection and the
// cache key of that selection.
func healthSelection(groups []string) (map[string]bool, string, error) {
	sel := map[string]bool{}
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if !contains(doctorGroups, g) {
			return nil, "", web.BadInput("group takes %s, got %q", strings.Join(doctorGroups, ", "), g)
		}
		sel[g] = true
	}
	if len(sel) == 0 {
		for _, g := range doctorGroups {
			sel[g] = true
		}
	}
	names := make([]string, 0, len(sel))
	for g := range sel {
		names = append(names, g)
	}
	sort.Strings(names)
	return sel, strings.Join(names, ","), nil
}

// healthCache shares one run between concurrent requests and keeps the last result for healthTTL.
// The run belongs to its waiters, not to the first request: it ends early only when the last
// waiter goes away (every browser disconnected).
type healthCache struct {
	now func() time.Time
	mu  sync.Mutex
	// the last finished run
	lastKey string
	last    doctorReport
	lastAt  time.Time
	have    bool
	flight  *healthFlight
}

type healthFlight struct {
	key      string
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	canceled bool
	rep      doctorReport
	at       time.Time
}

func newHealthCache() *healthCache { return &healthCache{now: time.Now} }

// get returns the report for key: the cached one if it is younger than healthTTL, else the result
// of the run in progress, else of a new run(ctx) that starts now. It returns ctx.Err() when the
// caller's ctx ends first.
func (h *healthCache) get(ctx context.Context, key string, run func(context.Context) doctorReport) (doctorReport, time.Time, error) {
	for {
		h.mu.Lock()
		if h.have && h.lastKey == key && h.now().Sub(h.lastAt) < healthTTL {
			rep, at := h.last, h.lastAt
			h.mu.Unlock()
			return rep, at, nil
		}
		f := h.flight
		if f == nil {
			rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			f = &healthFlight{key: key, done: make(chan struct{}), cancel: cancel}
			h.flight = f
			go func() {
				rep := run(rctx)
				h.mu.Lock()
				f.rep, f.at = rep, h.now()
				if !f.canceled {
					h.last, h.lastKey, h.lastAt, h.have = rep, f.key, f.at, true
				}
				h.flight = nil
				h.mu.Unlock()
				cancel()
				close(f.done)
			}()
		}
		f.waiters++
		h.mu.Unlock()
		select {
		case <-f.done:
			if f.canceled || f.key != key {
				if err := ctx.Err(); err != nil {
					return doctorReport{}, time.Time{}, err
				}
				continue // that run was for other groups, or ended with its last waiter: run again
			}
			return f.rep, f.at, nil
		case <-ctx.Done():
			h.mu.Lock()
			if f.waiters--; f.waiters == 0 && h.flight == f {
				f.canceled = true
				f.cancel()
			}
			h.mu.Unlock()
			return doctorReport{}, time.Time{}, ctx.Err()
		}
	}
}
