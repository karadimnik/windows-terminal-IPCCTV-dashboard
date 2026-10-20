//go:build !linux && !windows

package main

// The spike only measured Windows and Linux; other systems report "unknown".
func heldOpen(p string) bool { return false }

func birth(p string) string { return "unsupported" }
