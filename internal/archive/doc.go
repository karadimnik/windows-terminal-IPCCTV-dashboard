// Package archive holds the naming, scanning and root-safety rules of the recording archive:
// camera folder names (SafeName), segment file names (pattern, parse, strict allowlist), the
// per-camera directory scan with the "open segment" definition of HLD section 6, the archive
// Layout every reader lists and resolves segments through (spool and day folders, decision D20;
// with its incremental Cache), the
// archive-root guards (CheckRoot) and the ownership marker. The pruner planner is a later task (W3) and builds
// on these. See README.md in this folder for the API and the deliberate differences from PowerShell.
package archive
