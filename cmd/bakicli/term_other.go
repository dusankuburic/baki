//go:build !windows

package main

import "os"

// isTerminal reports whether f is a character device other than the null
// device: a terminal, for every purpose bakicli has.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}
