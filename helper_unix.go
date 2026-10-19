//go:build darwin || linux

package main

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the helper in its own process group. When launchd stops mydevices it
// kills the whole group outright; the helper instead sees its stdin close and ends its
// capture session cleanly, so macOS doesn't leave its recording indicator behind.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
