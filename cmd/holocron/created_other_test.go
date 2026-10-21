//go:build !windows

package main

import "time"

// setCreated: the pruner reads no creation time outside Windows.
func setCreated(string, time.Time) error { return nil }
