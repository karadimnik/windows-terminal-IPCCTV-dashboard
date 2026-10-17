package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"holocron/internal/logx"
)

// ErrUnknownAction is returned by Do for a name that is not registered.
var ErrUnknownAction = errors.New("unknown action")

// LogSource hands out the logger of a component; *logx.Logger implements it.
type LogSource interface {
	For(component string) *slog.Logger
}

// Runner executes actions and is the only thing that should: it writes the records.
type Runner struct {
	Registry *Registry        // default: Default
	Logs     LogSource        // required
	Now      func() time.Time // default time.Now (a fake clock in tests)
}

// NewRunner returns a Runner over a registry (nil means Default).
func NewRunner(reg *Registry, logs LogSource) *Runner {
	if reg == nil {
		reg = Default
	}
	return &Runner{Registry: reg, Logs: logs, Now: time.Now}
}

// NewActionID is 8 random bytes in hex.
func NewActionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Do runs the named action. The action id is taken from in.ActionID, else from ctx, else generated,
// and is passed on in the ctx the handler receives. It logs action.begin at DEBUG and exactly one
// action.result: INFO for the success of a Mutating action, DEBUG for the success of a read-only
// one, WARN for a refusal (errors.Is ErrRefused), ERROR for any other failure (a panic included).
func (r *Runner) Do(ctx context.Context, name string, in Input) (out Output, err error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	id := in.ActionID
	if id == "" {
		id = ActionIDFrom(ctx)
	}
	if id == "" {
		id = NewActionID()
	}
	ctx = WithActionID(ctx, id)
	actor := ActorFrom(ctx)

	act, ok := r.Registry.Lookup(name)
	if !ok {
		r.Logs.For("cli").LogAttrs(ctx, logx.LevelError, "action.result",
			logx.Action(name), logx.Actor(actor), logx.Target(in.Target), logx.Result(logx.ResultFail),
			logx.DurationMS(0), logx.Err(ErrUnknownAction), logx.ActionID(id), logx.PID(os.Getpid()))
		return Output{}, fmt.Errorf("%w: %s", ErrUnknownAction, name)
	}

	log := r.Logs.For(act.Component)
	base := []slog.Attr{logx.Action(name), logx.Actor(actor), logx.Target(in.Target), logx.ActionID(id), logx.PID(os.Getpid())}
	if act.Audit {
		base = append(base, logx.Audit())
	}
	log.LogAttrs(ctx, logx.LevelDebug, "action.begin", base...)

	start := now()
	var stack string
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic in action %s: %v", name, p)
				stack = string(debug.Stack())
				if len(stack) > 4096 {
					stack = stack[:4096]
				}
			}
		}()
		out, err = act.Run(ctx, in)
	}()
	dur := now().Sub(start)

	target := in.Target
	if out.Target != "" {
		target = out.Target
	}
	attrs := []slog.Attr{logx.Action(name), logx.Actor(actor), logx.Target(target), logx.DurationMS(dur), logx.ActionID(id), logx.PID(os.Getpid())}
	level, result := logx.LevelDebug, logx.ResultOK
	switch {
	case err == nil:
		if act.Mutating {
			level = logx.LevelInfo
		}
	case errors.Is(err, ErrRefused):
		level, result = logx.LevelWarn, logx.ResultRefused
	default:
		level, result = logx.LevelError, logx.ResultFail
	}
	attrs = append(attrs, logx.Result(result), logx.Err(err))
	if stack != "" {
		attrs = append(attrs, slog.String("stack", stack))
	}
	if act.Audit {
		attrs = append(attrs, logx.Audit())
	}
	log.LogAttrs(ctx, level, "action.result", attrs...)
	return out, err
}
