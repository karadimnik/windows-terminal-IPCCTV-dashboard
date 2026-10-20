// Package supervise holds the recorder supervisor (W7b: the loop, heartbeat, relay and live-view
// MediaMTX children, pruner schedule and ordered stop) and the per-camera child lifecycle manager it
// drives (W7a: backoff, stall watchdog, relay-or-direct input choice, camera-down alert 110 and the
// status rows). See README.md and tasks/portable-nvr/os-independent-lld.md sections 2.4 and 3.
package supervise
