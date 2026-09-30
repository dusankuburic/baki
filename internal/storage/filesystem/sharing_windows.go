//go:build windows

package filesystem

import "pad-analyzer/internal/storage/atomicfile"

// errSharingViolation is winerror ERROR_SHARING_VIOLATION (32). The retry
// predicates and their rationale live in internal/storage/atomicfile, shared
// with the Azure AI Foundry registry file; these names stay so this
// package's tests pin the implementation renameOverOpenReader and
// readFileTolerant actually run.
const errSharingViolation = atomicfile.ErrSharingViolation

// isSharingViolation is atomicfile.IsSharingViolation: err is (or wraps) a
// Windows ERROR_SHARING_VIOLATION, which errors.Is(err, fs.ErrPermission)
// cannot see.
func isSharingViolation(err error) bool {
	return atomicfile.IsSharingViolation(err)
}

// transientSharingConflict is atomicfile.TransientSharingConflict, the
// Windows retry predicate: ERROR_SHARING_VIOLATION or ERROR_ACCESS_DENIED
// (fs.ErrPermission) from a reader's handle on the destination.
func transientSharingConflict(err error) bool {
	return atomicfile.TransientSharingConflict(err)
}
