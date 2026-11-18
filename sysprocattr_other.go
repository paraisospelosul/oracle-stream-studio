//go:build !linux

package main

import "os/exec"

func setProcessDeathSignal(_ *exec.Cmd) {}
