package alerts

import (
	"context"
	"log/slog"

	"holocron/internal/logx"
)

// logChannel is the "log" channel: always on, the structured record of the alert. It is the
// durable local trace (the PowerShell product's "ALERT ..." line) and carries audit=true.
type logChannel struct{ log *slog.Logger }

func newLogChannel(l *slog.Logger) *logChannel { return &logChannel{log: l} }

func (*logChannel) Name() string  { return "log" }
func (*logChannel) Enabled() bool { return true }

func (c *logChannel) Send(ctx context.Context, ev Event) error {
	level := slog.LevelWarn
	switch ev.Severity {
	case Error:
		level = slog.LevelError
	case Info:
		level = slog.LevelInfo
	}
	attrs := []slog.Attr{logx.Action("alert.deliver"), logx.Target("log"), logx.Result(logx.ResultOK),
		slog.Int("id", ev.ID), slog.String("severity", ev.Severity.String()), logx.Audit()}
	if ev.Camera != "" {
		attrs = append(attrs, slog.String("camera", ev.Camera))
	}
	c.log.LogAttrs(ctx, level, "ALERT "+ev.text(), attrs...)
	return nil
}
