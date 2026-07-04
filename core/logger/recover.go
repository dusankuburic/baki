package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// PanicHook is invoked by the recover helpers after a panic is logged. The
// server registers a hook that forwards to its error-aggregation sink
// (internal/errreport); core cannot import that package, so the indirection
// keeps the dependency one-way (server → core). Implementations must not panic.
// Arguments: operation is the call-site label; recovered is the panic value;
// stack is the goroutine stack trace.
type PanicHook func(operation string, recovered any, stack []byte)

var (
	hookMu sync.RWMutex
	hook   PanicHook
)

// SetPanicHook installs the hook called when a Guard/Recover helper catches a
// panic. Intended to be called once at startup. Passing nil removes it.
func SetPanicHook(fn PanicHook) {
	hookMu.Lock()
	hook = fn
	hookMu.Unlock()
}

// captureStack returns a goroutine stack trace of (at most) 8 KiB. Shared by
// every recover helper so the hook always receives []byte.
func captureStack() []byte {
	buf := make([]byte, 8192)
	n := runtime.Stack(buf, false)
	return buf[:n]
}

func notifyHook(operation string, r any, stack []byte) {
	hookMu.RLock()
	fn := hook
	hookMu.RUnlock()
	if fn == nil {
		return
	}
	defer func() { _ = recover() }() // a hook must never break recovery
	fn(operation, r, stack)
}

// Guard recovers from a panic, logs it with a stack trace, and sets *err.
// Use as: defer logger.Guard("App.Method", &err)
func Guard(operation string, err *error) {
	if r := recover(); r != nil {
		stack := captureStack()
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack),
		)
		notifyHook(operation, r, stack)
		*err = fmt.Errorf("panic in %s: %v", operation, r)
	}
}

// GuardRecover recovers from a panic and logs it, without setting an error return.
// Use as: defer logger.GuardRecover("App.Method")
func GuardRecover(operation string) {
	if r := recover(); r != nil {
		stack := captureStack()
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack),
		)
		notifyHook(operation, r, stack)
	}
}

func RecoverPanic(operation string) {
	if r := recover(); r != nil {
		stack := captureStack()
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack),
		)
		notifyHook(operation, r, stack)
	}
}

func RecoverPanicWithError(operation string) error {
	if r := recover(); r != nil {
		stack := captureStack()
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack),
		)
		notifyHook(operation, r, stack)
		return fmt.Errorf("panic in %s: %v", operation, r)
	}
	return nil
}

func rotateLogs(logDir string) {
	mainLog := filepath.Join(logDir, "pad-analyzer.log")
	info, err := os.Stat(mainLog)
	if err != nil || info.Size() <= 10<<20 {
		return
	}

	if err := os.Remove(filepath.Join(logDir, "pad-analyzer.log.5")); err != nil && !os.IsNotExist(err) {
		slog.Warn("log rotation: failed to remove .5", "error", err)
	}

	for i := 4; i >= 1; i-- {
		oldPath := filepath.Join(logDir, fmt.Sprintf("pad-analyzer.log.%d", i))
		newPath := filepath.Join(logDir, fmt.Sprintf("pad-analyzer.log.%d", i+1))
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			slog.Warn("log rotation: rename failed", "from", oldPath, "to", newPath, "error", err)
		}
	}
	if err := os.Rename(mainLog, filepath.Join(logDir, "pad-analyzer.log.1")); err != nil {
		slog.Warn("log rotation: main rename failed", "error", err)
	}
}
