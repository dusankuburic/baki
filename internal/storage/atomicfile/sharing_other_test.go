//go:build !windows

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// TestRetryableRenameError_NonWindowsFailsFast pins the POSIX arm of the
// retry policy: RenameOverOpenReader must NOT retry anything here. POSIX
// rename over an open destination succeeds outright, so a permission error is
// a genuine, permanent denial — retrying it burned 20×100 ms while the caller
// (often holding a store lock) serialized every other save. ErrPermission is
// asserted explicitly because it is exactly the shape the Windows arm DOES
// retry; the two build-tagged predicates must stay opposite on it.
func TestRetryableRenameError_NonWindowsFailsFast(t *testing.T) {
	if TransientSharingConflict(fs.ErrPermission) {
		t.Error("TransientSharingConflict(fs.ErrPermission) = true, want false on POSIX (permanent denial, fail fast)")
	}
	eacces := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EACCES}
	if TransientSharingConflict(eacces) {
		t.Errorf("TransientSharingConflict(%v) = true, want false on POSIX", eacces)
	}
	if TransientSharingConflict(errors.New("plain error")) {
		t.Error("TransientSharingConflict(plain error) = true, want false")
	}
	if TransientSharingConflict(nil) {
		t.Error("TransientSharingConflict(nil) = true, want false")
	}
}
