package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
)

// Option adjusts a Dispatcher.
type Option func(*Dispatcher)

// WithClock injects the clock used for dedupe, retry waits and durations (tests use clock.Fake).
func WithClock(c clock.Clock) Option { return func(d *Dispatcher) { d.clk = c } }

// WithRedactor masks secrets in Title, Message and Camera before an event reaches any channel,
// as the PowerShell product did (Get-CamRedacted). Pass logger.Redactor().String.
func WithRedactor(f func(string) string) Option { return func(d *Dispatcher) { d.redact = f } }

// Dispatcher accepts events without ever blocking the caller and delivers each to every enabled
// channel on that channel's own worker.
type Dispatcher struct {
	cfg    resolved
	log    *slog.Logger
	clk    clock.Clock
	redact func(string) string

	base    context.Context
	cancel  context.CancelFunc
	closing chan struct{}
	once    sync.Once
	wg      sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	last    map[string]clock.Mono
	workers []*worker
	all     []Channel
}

// New builds the dispatcher and starts one worker per enabled channel. The "log" channel is always
// added (first) unless channels already contains one named "log". logger should be the "alerts"
// component logger (logx.Logger.For("alerts")); nil discards.
func New(cfg Config, channels []Channel, logger *slog.Logger) *Dispatcher {
	return NewWith(cfg, channels, logger)
}

// NewWith is New with options.
func NewWith(cfg Config, channels []Channel, logger *slog.Logger, opts ...Option) *Dispatcher {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	d := &Dispatcher{cfg: cfg.resolve(), log: logger, clk: clock.Real(), redact: func(s string) string { return s },
		closing: make(chan struct{}), last: map[string]clock.Mono{}}
	for _, o := range opts {
		o(d)
	}
	d.base, d.cancel = context.WithCancel(context.Background())

	hasLog := false
	for _, c := range channels {
		if c != nil && c.Name() == "log" {
			hasLog = true
		}
	}
	if !hasLog {
		d.all = append(d.all, newLogChannel(logger))
	}
	for _, c := range channels {
		if c != nil {
			d.all = append(d.all, c)
		}
	}
	for _, c := range d.all {
		if !safeEnabled(c) {
			d.log.Debug("alert channel disabled", logx.Action("alert.channel"), logx.Target(c.Name()), logx.Result("disabled"))
			continue
		}
		w := &worker{d: d, ch: c, wake: make(chan struct{}, 1)}
		if n, ok := c.(Networked); ok && n.Network() {
			w.retry = true
		}
		d.workers = append(d.workers, w)
		d.wg.Add(1)
		go w.run()
	}
	return d
}

func safeEnabled(c Channel) (on bool) {
	defer func() {
		if recover() != nil {
			on = false
		}
	}()
	return c.Enabled()
}

// Raise queues ev for every enabled channel and returns at once. It never blocks, panics or
// reports an error: a full queue drops the oldest event of that channel, a repeated Key within
// MinRepeat is dropped, a closed dispatcher ignores the call.
func (d *Dispatcher) Raise(ev Event) {
	defer func() { _ = recover() }()
	ev.Title, ev.Message, ev.Camera = d.redact(ev.Title), d.redact(ev.Message), d.redact(ev.Camera)
	if ev.At.IsZero() {
		ev.At = d.clk.Now()
	}
	attrs := func(result string) []any {
		a := []any{logx.Action("alert.raise"), logx.Result(result), slog.Int("id", ev.ID), slog.String("severity", ev.Severity.String())}
		if ev.Camera != "" {
			a = append(a, slog.String("camera", ev.Camera))
		}
		if ev.Key != "" {
			a = append(a, slog.String("key", d.redact(ev.Key)))
		}
		return a
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		d.log.Debug("alert ignored: dispatcher closed", attrs("closed")...)
		return
	}
	if ev.Key != "" {
		now := d.clk.Mono()
		if t, ok := d.last[ev.Key]; ok && now.Sub(t) < d.cfg.minRepeat {
			d.mu.Unlock()
			d.log.Debug("alert not repeated within MinRepeat", attrs("deduped")...)
			return
		}
		d.last[ev.Key] = now
		if len(d.last) > 512 {
			for k, t := range d.last {
				if now.Sub(t) >= d.cfg.minRepeat {
					delete(d.last, k)
				}
			}
		}
	}
	workers := d.workers
	d.mu.Unlock()

	d.log.Info("alert raised: "+ev.text(), attrs(logx.ResultOK)...)
	for _, w := range workers {
		w.push(ev)
	}
}

// RaiseFrom raises the event of a producer type that implements Eventer.
func (d *Dispatcher) RaiseFrom(e Eventer) { d.Raise(e.AlertEvent()) }

// Result is the outcome of Test for one channel.
type Result struct {
	Channel  string
	Enabled  bool
	Err      error // nil: delivered
	Duration time.Duration
}

// Test sends a TEST event straight to one channel (name) or to all (name == ""), bypassing the
// queue, the dedupe and the retries, and returns one Result per channel. Disabled channels are
// reported, not skipped silently; an unknown name yields one Result with an error.
func (d *Dispatcher) Test(ctx context.Context, name string) []Result {
	ev := Event{ID: IDTest, Severity: Info, Title: "TEST - Holocron alert test, ignore",
		Message: "This is a TEST alert sent by 'holocron alert test'. Nothing is wrong.", At: d.clk.Now()}
	var out []Result
	for _, c := range d.all {
		if name != "" && c.Name() != name {
			continue
		}
		r := Result{Channel: c.Name(), Enabled: safeEnabled(c)}
		if !r.Enabled {
			r.Err = fmt.Errorf("channel %s is disabled", c.Name())
		} else {
			start := d.clk.Mono()
			r.Err = d.attempt(ctx, c, ev)
			r.Duration = d.clk.Since(start)
			if _, self := c.(*logChannel); !self {
				d.logDelivery(c.Name(), r.Err, r.Duration, true, false)
			}
		}
		out = append(out, r)
	}
	if name != "" && len(out) == 0 {
		out = append(out, Result{Channel: name, Err: fmt.Errorf("unknown channel %q", name)})
	}
	return out
}

// attempt runs one Send with the per-channel timeout. Send runs on its own goroutine so a channel
// that ignores its context cannot hold the worker; a panic becomes an error.
func (d *Dispatcher) attempt(parent context.Context, c Channel, ev Event) error {
	ctx, cancel := context.WithTimeout(parent, d.cfg.timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("channel panicked: %v", r)
			}
		}()
		done <- c.Send(ctx, ev)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		select {
		case err := <-done:
			return err
		default:
		}
		if parent.Err() != nil {
			return fmt.Errorf("delivery abandoned: %w", parent.Err())
		}
		return fmt.Errorf("timed out after %s", d.cfg.timeout)
	}
}

func (d *Dispatcher) logDelivery(channel string, err error, dur time.Duration, test, final bool) {
	a := []any{logx.Action("alert.deliver"), logx.Target(channel), logx.DurationMS(dur)}
	if test {
		a = append(a, slog.Bool("test", true))
	}
	switch {
	case err == nil:
		d.log.Info("alert delivered", append(a, logx.Result(logx.ResultOK))...)
	case final && !test:
		d.log.Error("alert delivery failed, giving up", append(a, logx.Result(logx.ResultFail), logx.Err(err))...)
	default:
		d.log.Warn("alert delivery failed", append(a, logx.Result(logx.ResultFail), logx.Err(err))...)
	}
}

// Close stops accepting events and flushes the queues. Queued events get one attempt each (retry
// waits are skipped). When ctx ends first, in-flight deliveries are cancelled, the undelivered
// events are dropped with a WARN, and ctx.Err() is returned. Close is idempotent.
func (d *Dispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.once.Do(func() { close(d.closing) })

	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		<-done
		return ctx.Err()
	}
}

// worker owns the queue and the delivery loop of one channel.
type worker struct {
	d     *Dispatcher
	ch    Channel
	retry bool
	wake  chan struct{}

	mu      sync.Mutex
	q       []Event
	dropped int
	noticed bool
}

func (w *worker) push(ev Event) {
	w.mu.Lock()
	notice := false
	if len(w.q) >= w.d.cfg.queue {
		w.q = w.q[1:]
		w.dropped++
		if !w.noticed {
			w.noticed, notice = true, true
		}
	}
	w.q = append(w.q, ev)
	w.mu.Unlock()
	if notice {
		w.d.log.Warn("alert queue full: dropping the oldest events until it drains",
			logx.Action("alert.queue"), logx.Target(w.ch.Name()), logx.Result("dropped"), slog.Int("queue_size", w.d.cfg.queue))
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// next blocks until an event is queued; false means the worker should end.
func (w *worker) next() (Event, bool) {
	for {
		w.mu.Lock()
		if w.d.base.Err() != nil {
			n := len(w.q) + w.dropped
			w.q, w.dropped = nil, 0
			w.mu.Unlock()
			if n > 0 {
				w.d.log.Warn("alerts not delivered before the shutdown deadline",
					logx.Action("alert.deliver"), logx.Target(w.ch.Name()), logx.Result("dropped"), slog.Int("count", n))
			}
			return Event{}, false
		}
		if len(w.q) > 0 {
			ev := w.q[0]
			w.q = w.q[1:]
			w.mu.Unlock()
			return ev, true
		}
		n := w.dropped
		w.dropped, w.noticed = 0, false
		w.mu.Unlock()
		if n > 0 {
			w.d.log.Info("alert queue drained", logx.Action("alert.queue"), logx.Target(w.ch.Name()),
				logx.Result("dropped"), slog.Int("dropped", n))
		}
		select {
		case <-w.wake:
		case <-w.d.closing:
			w.mu.Lock()
			empty := len(w.q) == 0
			w.mu.Unlock()
			if empty {
				return Event{}, false
			}
		case <-w.d.base.Done():
		}
	}
}

func (w *worker) run() {
	defer w.d.wg.Done()
	for {
		ev, ok := w.next()
		if !ok {
			return
		}
		w.deliver(ev)
	}
}

func (w *worker) deliver(ev Event) {
	d := w.d
	name := w.ch.Name()
	_, self := w.ch.(*logChannel)
	if !safeEnabled(w.ch) {
		d.log.Debug("alert skipped: channel disabled", logx.Action("alert.deliver"), logx.Target(name), logx.Result("disabled"))
		return
	}
	var backoff []time.Duration
	if w.retry {
		backoff = d.cfg.backoff
	}
	for i := 0; ; i++ {
		start := d.clk.Mono()
		err := d.attempt(d.base, w.ch, ev)
		dur := d.clk.Since(start)
		if err == nil {
			if !self {
				d.logDelivery(name, nil, dur, false, false)
			}
			return
		}
		closing := false
		select {
		case <-d.closing:
			closing = true
		default:
		}
		if i >= len(backoff) || closing || d.base.Err() != nil {
			d.logDelivery(name, err, dur, false, w.retry)
			return
		}
		d.logDelivery(name, err, dur, false, false)
		select {
		case <-d.clk.After(backoff[i]):
		case <-d.closing:
		case <-d.base.Done():
		}
	}
}
