//go:build !windows

package atomicfile

// IsSharingViolation is the non-Windows arm of the retry predicate in
// RenameOverOpenReader: POSIX rename atomically replaces a destination that
// other processes/goroutines hold open, so there is no sharing violation to
// detect and the answer is always false. Implemented (rather than dropped from
// the shared call site) so the predicate compiles identically on every
// platform and stays exhaustively testable.
func IsSharingViolation(err error) bool {
	return false
}

// TransientSharingConflict is the non-Windows arm of RenameOverOpenReader's retry
// predicate, and it is unconditionally false: POSIX rename over an open
// destination succeeds outright, so the only errors left are genuine,
// permanent failures — fs.ErrPermission on POSIX means the caller really lacks
// permission (there is no transient Windows-style ERROR_ACCESS_DENIED shape),
// and retrying it burned 20×100 ms while the caller (often holding flowMu)
// serialized every other flow save behind a hopeless retry.
func TransientSharingConflict(err error) bool {
	return false
}
