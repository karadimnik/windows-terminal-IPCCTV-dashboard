package logx

import (
	"log/slog"
	"time"
)

// Values of the result attribute.
const (
	ResultOK      = "ok"
	ResultFail    = "fail"
	ResultRefused = "refused"
)

// Standard attribute helpers for action records.

func Action(name string) slog.Attr         { return slog.String("action", name) }
func Actor(actor string) slog.Attr         { return slog.String("actor", actor) }
func Target(target string) slog.Attr       { return slog.String("target", target) }
func Result(result string) slog.Attr       { return slog.String("result", result) }
func ActionID(id string) slog.Attr         { return slog.String("action_id", id) }
func PID(pid int) slog.Attr                { return slog.Int("pid", pid) }
func DurationMS(d time.Duration) slog.Attr { return slog.Int64("duration_ms", d.Milliseconds()) }

// Audit marks a record for the audit stream.
func Audit() slog.Attr { return slog.Bool("audit", true) }

// Err is the err attribute; a nil error yields an empty attribute, which slog drops.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.Any("err", err)
}
