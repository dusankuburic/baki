package main

import (
	"os"
	"os/exec"
	"syscall"
)

// prepareInterrupt starts the backend in a process group of its own, so a CTRL_BREAK reaches it alone.
func prepareInterrupt(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// interrupt sends CTRL_BREAK to the backend's process group; Go delivers it as os.Interrupt, on which fx stops the
// app. That needs a console the test shares with the backend; without one it kills the backend and reports false.
func interrupt(p *os.Process) bool {
	ctrl := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")
	if ok, _, _ := ctrl.Call(syscall.CTRL_BREAK_EVENT, uintptr(p.Pid)); ok == 0 {
		_ = p.Kill()
		return false
	}
	return true
}
