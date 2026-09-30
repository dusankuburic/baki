//go:build !windows

package filesystem

import "pad-analyzer/internal/storage/atomicfile"

// transientSharingConflict is atomicfile.TransientSharingConflict,
// unconditionally false off Windows: POSIX rename atomically replaces a
// destination that other processes/goroutines hold open, and
// fs.ErrPermission on POSIX is a genuine, permanent denial, never worth a
// retry. The predicates and their rationale live in
// internal/storage/atomicfile, shared with the Azure AI Foundry registry
// file; the name stays so this package's tests pin the implementation
// renameOverOpenReader and readFileTolerant actually run. (The Windows-only
// isSharingViolation has no caller here, so it has no non-Windows twin;
// atomicfile.IsSharingViolation keeps one for every platform.)
func transientSharingConflict(err error) bool {
	return atomicfile.TransientSharingConflict(err)
}
