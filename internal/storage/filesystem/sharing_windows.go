//go:build windows

package filesystem

import (
	"errors"
	"io/fs"
	"syscall"
)

// errSharingViolation is winerror ERROR_SHARING_VIOLATION. Go's syscall
// package exports only the handful of codes its own error mapping needs
// (ERROR_ACCESS_DENIED yes, this one no), so it is pinned here next to its
// only use.
const errSharingViolation syscall.Errno = 32

// isSharingViolation reports whether err is (or wraps) a Windows
// ERROR_SHARING_VIOLATION (32). Go's Windows error mapping links only
// ERROR_ACCESS_DENIED(5)/EACCES/EPERM to os.ErrPermission, so a rename over a
// destination held open without FILE_SHARE_DELETE — exactly the handle shape
// Go's own os.ReadFile opens with, i.e. LoadFlow racing a Save — must be
// matched on the raw errno or it is invisible to errors.Is.
func isSharingViolation(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == errSharingViolation
}

// transientSharingConflict is the Windows arm of the retry predicates in
// renameOverOpenReader and readFileTolerant: both transient shapes observed
// on Windows qualify — the raw ERROR_SHARING_VIOLATION (32) that errors.Is
// cannot see, and ERROR_ACCESS_DENIED(5), which Windows surfaces for the
// very same no-FILE_SHARE_DELETE race (opening a path mid-replace fails the
// same way) but Go maps onto fs.ErrPermission.
func transientSharingConflict(err error) bool {
	return isSharingViolation(err) || errors.Is(err, fs.ErrPermission)
}
