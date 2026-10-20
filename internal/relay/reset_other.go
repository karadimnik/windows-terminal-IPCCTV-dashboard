//go:build !windows

package relay

func resetACL(string) error { return nil }
