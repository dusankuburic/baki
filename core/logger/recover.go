package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// Guard recovers from a panic, logs it with a stack trace, and sets *err.
// Use as: defer logger.Guard("App.Method", &err)
func Guard(operation string, err *error) {
	if r := recover(); r != nil {
		stack := make([]byte, 8192)
		n := runtime.Stack(stack, false)
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack[:n]),
		)
		*err = fmt.Errorf("panic in %s: %v", operation, r)
	}
}

// GuardRecover recovers from a panic and logs it, without setting an error return.
// Use as: defer logger.GuardRecover("App.Method")
func GuardRecover(operation string) {
	if r := recover(); r != nil {
		stack := make([]byte, 8192)
		n := runtime.Stack(stack, false)
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack[:n]),
		)
	}
}

func RecoverPanic(operation string) {
	if r := recover(); r != nil {
		stack := make([]byte, 8192)
		n := runtime.Stack(stack, false)
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack[:n]),
		)
	}
}

func RecoverPanicWithError(operation string) error {
	if r := recover(); r != nil {
		stack := make([]byte, 8192)
		n := runtime.Stack(stack, false)
		slog.Error("panic recovered",
			"operation", operation,
			"panic", r,
			"stack", string(stack[:n]),
		)
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
