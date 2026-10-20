//go:build !windows

package alerts

import "os/exec"

func hideWindow(*exec.Cmd) {}
