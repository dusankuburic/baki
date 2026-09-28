//go:build windows

package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// openLikeGoReadFile opens path with the exact handle shape Go's os.ReadFile
// (syscall.Open) uses: FILE_SHARE_READ|FILE_SHARE_WRITE and no
// FILE_SHARE_DELETE — the handle a LoadFlow racing a Save holds on the
// destination when renameOverOpenReader's retry matters.
func openLikeGoReadFile(t *testing.T, path string) syscall.Handle {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("path %s: %v", path, err)
	}
	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ,
		uint32(syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE), // no FILE_SHARE_DELETE
		nil,
		syscall.OPEN_EXISTING,
		uint32(syscall.FILE_ATTRIBUTE_NORMAL),
		0)
	if err != nil {
		t.Fatalf("CreateFile %s: %v", path, err)
	}
	return h
}

// TestRenameRetryPredicate_MatchesDestinationHeldOpen induces the exact race
// renameOverOpenReader exists for, against the real filesystem: a destination
// held open the way LoadFlow holds it makes replacing it fail. Which errno the
// OS surfaces varies by Windows build and filesystem (observed: NTFS on
// Windows 11 returns ERROR_ACCESS_DENIED(5), which Go maps to ErrPermission;
// SMB/FAT configurations report ERROR_SHARING_VIOLATION(32)) — so the
// assertion is on the retry PREDICATE as a whole: at least one arm must match,
// or the retry is dead code for the live race on that platform.
func TestRenameRetryPredicate_MatchesDestinationHeldOpen(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dest.json")
	src := filepath.Join(dir, "src.json")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	h := openLikeGoReadFile(t, dst)
	defer syscall.CloseHandle(h) //nolint:errcheck // best-effort test cleanup

	renameErr := os.Rename(src, dst)
	if renameErr == nil {
		t.Skip("rename over an unshared-open destination unexpectedly succeeded")
	}
	if !isSharingViolation(renameErr) && !errors.Is(renameErr, fs.ErrPermission) {
		t.Fatalf("os.Rename over a no-FILE_SHARE_DELETE destination returned %v, which neither retry arm matches", renameErr)
	}
}

// TestIsSharingViolation_RenameOfUnsharedOpenFile proves the helper against a
// REAL os.Rename ERROR_SHARING_VIOLATION. Renaming a file that is itself held
// open without FILE_SHARE_DELETE deterministically yields errno 32 (unlike the
// destination-replace case, whose errno varies) — and errno 32 is exactly the
// shape the ErrPermission arm cannot see, since Go maps only
// ERROR_ACCESS_DENIED(5)/EACCES/EPERM to os.ErrPermission.
func TestIsSharingViolation_RenameOfUnsharedOpenFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	dst := filepath.Join(dir, "moved.json")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("seed src: %v", err)
	}

	h := openLikeGoReadFile(t, src)
	defer syscall.CloseHandle(h) //nolint:errcheck // best-effort test cleanup

	renameErr := os.Rename(src, dst)
	if renameErr == nil {
		t.Skip("renaming an unshared-open file unexpectedly succeeded")
	}
	var errno syscall.Errno
	if !errors.As(renameErr, &errno) || errno != errSharingViolation {
		t.Fatalf("expected a genuine ERROR_SHARING_VIOLATION from os.Rename, got %v (errno %d)", renameErr, errno)
	}
	if !isSharingViolation(renameErr) {
		t.Fatalf("isSharingViolation(%v) = false, want true for a real ERROR_SHARING_VIOLATION", renameErr)
	}
	if errors.Is(renameErr, fs.ErrPermission) {
		t.Fatalf("sanity: %v unexpectedly satisfies ErrPermission — the ErrPermission arm would mask the sharing-violation test above", renameErr)
	}
}

// TestRetryableRenameError_Windows pins the platform policy
// renameOverOpenReader retries under: both transient shapes of the
// open-destination race (raw errno 32, and errno 5 via fs.ErrPermission)
// qualify; anything else — a plain error, nil — must fail immediately.
func TestRetryableRenameError_Windows(t *testing.T) {
	sharing := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errSharingViolation}
	if !transientSharingConflict(sharing) {
		t.Errorf("transientSharingConflict(errno 32) = false, want true")
	}
	denied := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.ERROR_ACCESS_DENIED}
	if !transientSharingConflict(denied) {
		t.Errorf("transientSharingConflict(errno 5 / ErrPermission) = false, want true")
	}
	if transientSharingConflict(errors.New("plain error")) {
		t.Error("transientSharingConflict(plain error) = true, want false")
	}
	if transientSharingConflict(nil) {
		t.Error("transientSharingConflict(nil) = true, want false")
	}
}

// TestIsSharingViolation_WrappedErrno pins the classification the retry
// predicate depends on: errno 32 wrapped in os.Rename's *LinkError matches,
// while errno 5 (handled by the ErrPermission arm) and non-errno errors do
// not. os.Rename returns *LinkError{Err: syscall.Errno(...)} on Windows, so
// this is the exact error shape reaching isSharingViolation in production.
func TestIsSharingViolation_WrappedErrno(t *testing.T) {
	sharing := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errSharingViolation}
	if !isSharingViolation(sharing) {
		t.Errorf("isSharingViolation(%v) = false, want true for ERROR_SHARING_VIOLATION", sharing)
	}

	// errno 5 is the shape Go maps to ErrPermission — it must be answered by
	// the OTHER arm of the retry predicate, not this one.
	denied := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.ERROR_ACCESS_DENIED}
	if isSharingViolation(denied) {
		t.Errorf("isSharingViolation(%v) = true, want false (errno 5 belongs to the ErrPermission branch)", denied)
	}
	if !errors.Is(denied, fs.ErrPermission) {
		t.Errorf("sanity: %v should satisfy errors.Is(fs.ErrPermission)", denied)
	}

	if isSharingViolation(errors.New("plain error")) {
		t.Error("isSharingViolation(plain error) = true, want false")
	}
	if isSharingViolation(nil) {
		t.Error("isSharingViolation(nil) = true, want false")
	}
	// Double wrapping must still be seen through (errors.As, not a shallow
	// type assertion).
	if !isSharingViolation(fmt.Errorf("wrap: %w", sharing)) {
		t.Errorf("isSharingViolation(wrapped errno 32) = false, want true")
	}
}
