//go:build !windows

package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// TestRetryableRenameError_NonWindowsFailsFast pins the POSIX arm of the
// retry policy: renameOverOpenReader must NOT retry anything here. POSIX
// rename over an open destination succeeds outright, so a permission error is
// a genuine, permanent denial — retrying it burned 20×100 ms while the caller
// (often holding flowMu) serialized every other flow save. ErrPermission is
// asserted explicitly because it is exactly the shape the Windows arm DOES
// retry; the two build-tagged predicates must stay opposite on it.
func TestRetryableRenameError_NonWindowsFailsFast(t *testing.T) {
	if transientSharingConflict(fs.ErrPermission) {
		t.Error("transientSharingConflict(fs.ErrPermission) = true, want false on POSIX (permanent denial, fail fast)")
	}
	eacces := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EACCES}
	if transientSharingConflict(eacces) {
		t.Errorf("transientSharingConflict(%v) = true, want false on POSIX", eacces)
	}
	if transientSharingConflict(errors.New("plain error")) {
		t.Error("transientSharingConflict(plain error) = true, want false")
	}
	if transientSharingConflict(nil) {
		t.Error("transientSharingConflict(nil) = true, want false")
	}
}
