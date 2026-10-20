//go:build !windows

package prune

import "time"

// setCreated: no creation time to set outside Windows (the planner does not read one there).
func setCreated(string, time.Time) error { return nil }
