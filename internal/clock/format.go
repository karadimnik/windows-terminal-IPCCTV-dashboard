package clock

import (
	"fmt"
	"time"
)

// TimestampLayout is the PowerShell round-trip ('o') shape with a numeric offset: exactly seven
// fractional digits and the offset in force, never "Z".
const TimestampLayout = "2006-01-02T15:04:05.0000000-07:00"

// LogStampLayout is the supervisor log stamp (`yyyy-MM-dd HH:mm:ss zzz`).
const LogStampLayout = "2006-01-02 15:04:05 -07:00"

// FormatTimestamp writes t with the offset of t's own location, so convert first
// (t.In(loc)) to get the offset in force at that instant. "never" is the literal null, written by callers.
func FormatTimestamp(t time.Time) string { return t.Format(TimestampLayout) }

// FormatLogStamp writes the log stamp of t in its own location.
func FormatLogStamp(t time.Time) string { return t.Format(LogStampLayout) }

// ParseTimestamp reads a stored timestamp back as an instant in UTC, never as a local clock reading.
// The exact PowerShell shape is read first; RFC 3339 with "Z" or any fraction is accepted too, like
// DateTimeOffset.Parse in Get-CamDateTimeOrNull.
func ParseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(TimestampLayout, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a timestamp: %q", s)
	}
	return t.UTC(), nil
}
