# clock

    type Mono time.Duration                      // monotonic reading; (Mono).Sub(Mono) time.Duration
    type Clock interface {
        Now() time.Time                          // wall clock, UTC: persisted instants, comparison with mtimes
        Mono() Mono                              // monotonic: intervals, stall, backoff, alert repeat
        Since(m Mono) time.Duration
        Sleep(d time.Duration)
        After(d time.Duration) <-chan time.Time
    }
    func Real() Clock
    func Due(c Clock, last Mono, interval time.Duration) bool
    func NewFake(start time.Time) *Fake          // Advance(d) wall+mono, StepWall(d) wall only, Sleepers() int
    const TimestampLayout, LogStampLayout
    func FormatTimestamp(t time.Time) string     // 2006-01-02T15:04:05.0000000-07:00, offset of t's own location; "never" = null, written by callers
    func FormatLogStamp(t time.Time) string      // 2006-01-02 15:04:05 +03:00
    func ParseTimestamp(s string) (time.Time, error) // instant in UTC; exact shape first, then RFC 3339
    func Location(name string) (*time.Location, error) // IANA, Windows name via the alias table, or ""/"Local"
    func WindowsZoneToIANA(name string) (string, bool)

Deviation from LLD 2.1: `Since` takes a `Mono`, not a `time.Time`, because a fake clock cannot forge the monotonic reading of a
`time.Time`. Importing this package embeds `time/tzdata` (about 450 KB) so Windows hosts without zoneinfo files can load zones.
A +/- 3600 s wall step does not change a `Mono` interval (improvement over PowerShell, which measures on the UTC wall clock).
