//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

func setProcessDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
}
