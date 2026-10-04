package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---- Init ------------------------------------------------------------------

func TestInit_CreatesLogFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slog keeps log file open; TempDir cleanup fails on Windows")
	}
	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatalf("Init: %v", err)
	}
	logPath := filepath.Join(dir, "logs", "pad-analyzer.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected log file at %s, got: %v", logPath, err)
	}
}

func TestInit_DebugMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slog keeps log file open; TempDir cleanup fails on Windows")
	}
	dir := t.TempDir()
	if err := Init(dir, true); err != nil {
		t.Fatalf("Init debug: %v", err)
	}
}

// Reset to a discard handler so future tests use a known-safe logger.
func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ---- Guard -----------------------------------------------------------------

func callGuard(err *error) {
	defer Guard("test.op", err)
	panic("boom")
}

func TestGuard_SetsErrOnPanic(t *testing.T) {
	var err error
	callGuard(&err)
	if err == nil {
		t.Fatal("Guard should set *err on panic")
	}
	if !strings.Contains(err.Error(), "test.op") {
		t.Errorf("err should mention operation, got: %v", err)
	}
}

func TestGuard_NoPanic_ErrIsNil(t *testing.T) {
	var err error
	func() {
		defer Guard("no.panic", &err)
		// no panic
	}()
	if err != nil {
		t.Errorf("expected nil err when no panic, got: %v", err)
	}
}

// ---- GuardRecover ----------------------------------------------------------

func callGuardRecover() {
	defer GuardRecover("guard.recover.op")
	panic("silent boom")
}

func TestGuardRecover_DoesNotPropagatePanic(t *testing.T) {
	callGuardRecover()
}

// ---- RecoverPanic ----------------------------------------------------------

func callRecoverPanic() {
	defer RecoverPanic("recover.panic.op")
	panic("rp boom")
}

func TestRecoverPanic_DoesNotPropagatePanic(t *testing.T) {
	callRecoverPanic()
}

// ---- RecoverPanicWithError -------------------------------------------------

// RecoverPanicWithError must be the directly-deferred function for recover()
// to intercept a panic. Wrap it in a named function so we can defer it directly
// and have it cover the panic path.
func callRecoverPanicWithErrorDirect() {
	defer RecoverPanicWithError("direct.op")
	panic("direct test panic")
}

func TestRecoverPanicWithError_PanicPath_DoesNotPropagate(t *testing.T) {
	// If RecoverPanicWithError doesn't catch the panic, it will propagate to
	// here and fail the test via the testing framework.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("RecoverPanicWithError did not handle the panic: %v", r)
		}
	}()
	callRecoverPanicWithErrorDirect()
}

func TestRecoverPanicWithError_ReturnsNilWhenNoPanic(t *testing.T) {
	// Called outside of a panicking goroutine → recover() returns nil → error is nil.
	err := RecoverPanicWithError("no.panic.op")
	if err != nil {
		t.Errorf("expected nil when no panic, got: %v", err)
	}
}

// ---- rotateLogs ------------------------------------------------------------

func TestRotateLogs_SkipsSmallFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "pad-analyzer.log")

	if err := os.WriteFile(logPath, []byte("small log"), 0644); err != nil {
		t.Fatal(err)
	}

	rotateLogs(dir) // should be a no-op

	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("small log file should not be rotated: %v", err)
	}
}

func TestRotateLogs_RotatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "pad-analyzer.log")

	data := make([]byte, 11<<20) // 11 MB, above the 10 MB threshold
	if err := os.WriteFile(logPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	rotateLogs(dir)

	// Original file should have been renamed to .1.
	if _, err := os.Stat(logPath); err == nil {
		t.Error("expected original log to be rotated away")
	}
	rotated := filepath.Join(dir, "pad-analyzer.log.1")
	if _, err := os.Stat(rotated); err != nil {
		t.Errorf("expected rotated file at .1, got: %v", err)
	}
}

func TestRotateLogs_NonExistentFile_Noop(t *testing.T) {
	// No log file in the dir → os.Stat fails → early return; must not panic.
	dir := t.TempDir()
	rotateLogs(dir)
}
