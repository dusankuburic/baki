// Package atomicfile holds the low-level file helpers the on-disk stores share
// (the filesystem backend and the Azure registry file). It is a leaf package on
// purpose — internal/storage cannot import the filesystem backend, whose tests
// import internal/testutil, which imports internal/storage (a test import cycle).
package atomicfile

import (
	"os"
	"time"
)

// RenameOverOpenReader renames oldpath over newpath, tolerating the one way
// Windows departs from POSIX rename semantics: replacing a file another
// goroutine momentarily holds open for reading (Go's os.ReadFile opens with no
// FILE_SHARE_DELETE — a LoadFlow racing a Save) fails with either
// ERROR_SHARING_VIOLATION (32) or ERROR_ACCESS_DENIED (5)/EACCES/EPERM; Go maps
// only the latter to ErrPermission, so errno 32 must be matched raw
// (IsSharingViolation) or it is invisible to errors.Is. The reader's handle
// closes within microseconds, so a short bounded retry succeeds. Retryability
// itself is platform policy (TransientSharingConflict): on non-Windows POSIX
// rename is atomic over open files and fs.ErrPermission is a genuine, permanent
// problem — retrying it just burned 20×100 ms while the caller (often holding
// flowMu) serialized every other save.
func RenameOverOpenReader(oldpath, newpath string) error {
	const attempts = 20 // ~2 s of 100 ms waits at worst
	var err error
	for range attempts {
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
		if !TransientSharingConflict(err) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// ReadFileTolerant is the read-side mirror of RenameOverOpenReader: opening a
// path while a writer's rename is replacing it fails with the same transient
// sharing violation, so the same bounded retry applies. On non-Windows
// TransientSharingConflict is false and reads fail through immediately.
func ReadFileTolerant(path string) ([]byte, error) {
	const attempts = 20 // ~2 s of 100 ms waits at worst; matches the writer
	var (
		data []byte
		err  error
	)
	for range attempts {
		data, err = os.ReadFile(path) // #nosec G304 -- every caller passes a path it owns and validated (see the #nosec notes at the call sites)
		if err == nil {
			return data, nil
		}
		if !TransientSharingConflict(err) {
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}
