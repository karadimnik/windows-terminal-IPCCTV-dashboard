// Package alerts is the alert path: the event model, a dispatcher whose Raise never blocks the
// caller, and the delivery channels (log, webhook, push, desktop, eventlog). See README.md and
// tasks/portable-nvr/os-independent-lld.md section 2.7.
//
// Rule: nothing here may block, fail or panic into the recorder. An alert that cannot be delivered
// is logged and dropped.
package alerts
