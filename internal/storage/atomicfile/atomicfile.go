// Package atomicfile holds the low-level file helpers the on-disk stores
// share: the filesystem backend (internal/storage/filesystem) and the Azure
// AI Foundry registry file (internal/storage, azure-models.json). It is a
// leaf package on purpose — internal/storage cannot import the filesystem
// backend, whose tests import internal/testutil, which imports
// internal/storage (a test import cycle).
package atomicfile

import (
	"os"
	"time"
)

// RenameOverOpenReader renames oldpath over newpath, tolerating the one way
// Windows departs from POSIX rename semantics: replacing a file that another
// goroutine momentarily holds open for reading (Go's os.ReadFile opens with
// FILE_SHARE_READ|WRITE and no FILE_SHARE_DELETE, so a LoadFlow racing a Save
// holds exactly such a handle) fails instead of atomically swapping the file.
// The failure surfaces as either ERROR_SHARING_VIOLATION (32) or
// ERROR_ACCESS_DENIED (5)/EACCES/EPERM depending on Windows build and
// filesystem — Go maps only the latter to ErrPermission, so errno 32 must be
// matched on the raw errno (IsSharingViolation) or it is invisible to
// errors.Is. The reader's handle closes within microseconds, so a short
// bounded retry turns the spurious denial into a success. Whether an error is
// retryable at all is platform policy (TransientSharingConflict): on non-Windows
// POSIX rename is atomic over open files, and fs.ErrPermission there means a
// genuine, permanent permission problem — retrying it just burned 20×100 ms
// while the caller (often holding flowMu) serialized every other save.
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

// ReadFileTolerant is the read-side mirror of RenameOverOpenReader: while a
// writer's rename is replacing the destination, a concurrent OPEN of the same
// path fails with the same transient sharing violation (a LoadFlow racing a
// Save — the mirror of the race the writer retry handles). The replacement
// completes within microseconds, so the same bounded retry applies. On
// non-Windows TransientSharingConflict is false and reads fail through
// immediately.
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
