// Package shadow compares two archives of segment files, the Operator's existing archive and the
// shadow archive Holocron records beside it (HLD phase 2, W13), and says whether they are equal
// within stated tolerances: names, clock boundaries, sizes, ffprobe durations, stream layout, audio
// and gaps. It only reads both roots (directory listings, file sizes and times, ffprobe on closed
// segments); it never writes, renames or deletes anything.
package shadow
