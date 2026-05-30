package service

import (
	"errors"
	"strings"
)

// ErrInvalidPath is returned by validateUserPath when a path is rejected by
// the basic safety filters.
var ErrInvalidPath = errors.New("invalid path")

// validateUserPath enforces a small set of must-not-happen checks on a
// user-supplied filesystem path before it is passed to os.ReadFile, os.Stat,
// or filepath.Join. It does NOT confine the path to a root — every endpoint
// that accepts a path is already gated by the cloud-mode `jwtEnabled` guard
// in the HTTP handler layer, so traversal across users isn't reachable in
// cloud mode and the desktop client legitimately needs to open files anywhere
// the OS user has access to. This is defense-in-depth: if the cloud-mode
// gate is ever lifted, these checks still reject the most obvious abuse
// patterns, and they also catch malformed input from a misbehaving frontend.
//
// Rejected: empty, null-byte-containing, or whitespace-only paths.
func validateUserPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return ErrInvalidPath
	}
	if strings.ContainsRune(p, 0) {
		return ErrInvalidPath
	}
	return nil
}
