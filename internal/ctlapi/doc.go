// Package ctlapi is the supervisor's local control endpoint (LLD 2.11) and its client: a loopback
// HTTP server whose address and secret are in <root>/_state/ctl.addr and ctl.secret, with every
// handler a cli.Action (rule R2). See README.md.
package ctlapi
