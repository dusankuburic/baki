//go:build windows

package atomicfile

import (
	"errors"
	"io/fs"
	"syscall"
)

// ErrSharingViolation is winerror ERROR_SHARING_VIOLATION. Go's syscall package
// exports only the codes its own error mapping needs (this one not), so it is
// pinned here next to its only use.
const ErrSharingViolation syscall.Errno = 32

// IsSharingViolation reports whether err is (or wraps) a Windows
// ERROR_SHARING_VIOLATION (32). Go maps only ERROR_ACCESS_DENIED(5)/EACCES/EPERM
// to os.ErrPermission, so a rename over a destination held open without
// FILE_SHARE_DELETE must be matched on the raw errno or it is invisible to
// errors.Is.
func IsSharingViolation(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == ErrSharingViolation
}

// TransientSharingConflict is the Windows arm of the retry predicates in
// RenameOverOpenReader and ReadFileTolerant: both transient shapes qualify —
// the raw ERROR_SHARING_VIOLATION (32) errors.Is cannot see, and
// ERROR_ACCESS_DENIED(5), which Windows surfaces for the very same
// no-FILE_SHARE_DELETE race but Go maps onto fs.ErrPermission.
func TransientSharingConflict(err error) bool {
	return IsSharingViolation(err) || errors.Is(err, fs.ErrPermission)
}
