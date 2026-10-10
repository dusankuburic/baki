//go:build windows

package main

import (
	"os"
	"syscall"
)

// isTerminal reports whether f is a console; NUL and pipes are not.
func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
