//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func prepareInterrupt(*exec.Cmd) {}

// interrupt sends SIGTERM, as the Tauri shell does on Unix; fx stops the app on it.
func interrupt(p *os.Process) bool {
	return p.Signal(syscall.SIGTERM) == nil
}
