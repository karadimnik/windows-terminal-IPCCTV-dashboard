// Package logx is the Holocron logging layer (rule R2 of
// tasks/portable-nvr/os-independent-hld.md, section 2.13 of the LLD): levelled,
// structured, per-component logging on log/slog with run-time level changes,
// redaction below every level, and file writers that cannot fail their caller.
package logx
