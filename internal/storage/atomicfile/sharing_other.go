//go:build !windows

package atomicfile

// IsSharingViolation is the non-Windows arm of the retry predicate in
// RenameOverOpenReader: POSIX rename atomically replaces a destination other
// processes hold open, so there is nothing to detect and the answer is always
// false. Implemented so the predicate compiles identically on every platform.
func IsSharingViolation(err error) bool {
	return false
}

// TransientSharingConflict is the non-Windows arm of the retry predicate, and
// it is unconditionally false: POSIX rename over an open destination succeeds
// outright, so the only errors left are genuine, permanent failures —
// fs.ErrPermission on POSIX really means no permission, and retrying it burned
// 20×100 ms while the caller (often holding a store lock) serialized every
// other save behind a hopeless retry.
func TransientSharingConflict(err error) bool {
	return false
}
