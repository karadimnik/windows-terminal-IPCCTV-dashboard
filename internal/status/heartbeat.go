package status

import (
	"context"
	"time"
)

// Heartbeater writes a status snapshot every interval on the goroutine that runs
// it, so the heartbeat does not wait for relay or ffmpeg work done elsewhere. It
// logs nothing: errors go to OnError (a failed write skips the cycle; the next
// tick tries again).
type Heartbeater struct {
	// Writer receives each snapshot.
	Writer *Writer
	// Snapshot returns the document to write; it must be quick and must not
	// block on slow work (return a copy of state the supervisor keeps current).
	// nil skips the cycle.
	Snapshot func() *Document
	// Interval between writes (StatusIntervalSeconds).
	Interval time.Duration
	// OnError is called with each write error; may be nil.
	OnError func(error)
	// NewTicker is a test seam; nil uses time.NewTicker.
	NewTicker func(d time.Duration) (ticks <-chan time.Time, stop func())
}

// Run writes once at once (the first write is not delayed by an interval), then
// on every tick, and returns when ctx is done. No write is in flight after Run
// returns.
func (h *Heartbeater) Run(ctx context.Context) {
	newTicker := h.NewTicker
	if newTicker == nil {
		newTicker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	ticks, stop := newTicker(h.Interval)
	defer stop()
	for {
		if ctx.Err() != nil {
			return
		}
		h.once()
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (h *Heartbeater) once() {
	doc := h.Snapshot()
	if doc == nil {
		return
	}
	if err := h.Writer.Write(doc); err != nil && h.OnError != nil {
		h.OnError(err)
	}
}
