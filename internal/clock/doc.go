// Package clock is the one place the program reads time.
//
// Persisted and compared instants are UTC wall readings (Clock.Now). Intervals - heartbeat age,
// stall, backoff, alert repeat, "is it due" - use the monotonic reading (Clock.Mono and Clock.Since),
// so a +/- 3600 s step of the wall clock (daylight saving read as local arithmetic, an NTP or
// manual step) can neither fire nor suppress them. The PowerShell product measures intervals on
// the UTC wall clock: that absorbs daylight saving but not a step of the UTC clock itself (IR
// section 7.3, "known limits"); the monotonic reading is a deliberate improvement over it.
// Comparisons against file times (mtime) still use the wall clock and are unavoidably affected by a step.
//
// Display forms keep the local offset: FormatTimestamp writes the PowerShell round-trip shape.
package clock
