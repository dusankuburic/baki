package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Options configures the logger. Pass via InitWith; Init keeps the original
// (desktop/Tauri-friendly) defaults for backwards compatibility.
type Options struct {
	// AppDataDir is the root for the on-disk log file. Ignored when
	// StdoutOnly is true.
	AppDataDir string
	// Level is one of "debug", "info", "warn", "error" (case-insensitive).
	// Empty or unknown values fall back to "info".
	Level string
	// AddSource adds file:line to every log line. Useful in debug builds.
	AddSource bool
	// StdoutOnly disables the file sink and writes JSON to stdout only.
	// This is the right choice for containerized deployments: the
	// orchestrator collects stdout/stderr, and the pod's filesystem is
	// ephemeral so a file-based log doesn't survive a restart.
	StdoutOnly bool
}

// Init initialises logging with the legacy defaults: structured JSON to
// both a file under <appDataDir>/logs/ and stderr, at info level (debug
// if the flag is true). Used by desktop/Tauri builds. Cloud deployments
// should call InitWith with StdoutOnly=true.
func Init(appDataDir string, debug bool) error {
	level := "info"
	if debug {
		level = "debug"
	}
	return InitWith(Options{
		AppDataDir: appDataDir,
		Level:      level,
		AddSource:  debug,
	})
}

// InitWith installs a logger configured by opts. See the field comments.
func InitWith(opts Options) error {
	var w io.Writer = os.Stdout
	if !opts.StdoutOnly {
		if opts.AppDataDir == "" {
			return fmt.Errorf("logger: AppDataDir required when not StdoutOnly")
		}
		logDir := filepath.Join(opts.AppDataDir, "logs")
		if err := os.MkdirAll(logDir, 0750); err != nil {
			return err
		}
		rotateLogs(logDir)

		logFile := filepath.Join(logDir, "pad-analyzer.log")
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) // #nosec G304 -- log path under the app's own data dir
		if err != nil {
			return err
		}
		w = io.MultiWriter(f, os.Stderr)
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     parseLevel(opts.Level),
		AddSource: opts.AddSource,
	})
	slog.SetDefault(slog.New(handler))
	return nil
}

// parseLevel maps a string level to slog.Level. Unknown / empty inputs
// fall back to info (the safe production default — debug logs are noisy
// and may include sensitive context).
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func Info(msg string, args ...any)  { slog.Info(msg, args...) }
func Error(msg string, args ...any) { slog.Error(msg, args...) }
func Debug(msg string, args ...any) { slog.Debug(msg, args...) }
func Warn(msg string, args ...any)  { slog.Warn(msg, args...) }
