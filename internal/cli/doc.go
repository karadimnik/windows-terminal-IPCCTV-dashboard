// Package cli is the action contract (rule R2, LLD 2.13): every command, control-API handler and
// web handler is an Action in a Registry, and a Runner is the only way to execute one. The Runner,
// not the handler, writes the begin and result records, so an action cannot forget to log.
package cli
