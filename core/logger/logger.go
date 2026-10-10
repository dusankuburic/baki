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
	// StdoutOnly disables the file sink and writes JSON to stdout only — the
	// right choice for containers, where the orchestrator collects stdout and
	// the pod's ephemeral filesystem loses a file-based log on restart.
	StdoutOnly bool
	// BaseAttrs are written on every line, before the line's own attributes:
	// what identifies the process (the server sets its replica's instance id)
	// so the lines of several replicas in one stream can be told apart.
	BaseAttrs []slog.Attr
}

// Init initialises logging with the legacy defaults: structured JSON to a file
// under <appDataDir>/logs/ and to stderr, at info level (debug if the flag is
// true); used by desktop/Tauri builds. Cloud deployments should call InitWith
// with StdoutOnly=true.
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
// When the log file cannot be opened it still installs the logger, on stderr
// alone, so the level and the JSON format hold, and returns the error.
func InitWith(opts Options) error {
	w, err := sink(opts)
	if err != nil {
		w = os.Stderr
	}
	slog.SetDefault(slog.New(newHandler(w, opts)))
	return err
}

// sink opens where InitWith writes: stdout, or the log file plus stderr.
func sink(opts Options) (io.Writer, error) {
	if opts.StdoutOnly {
		return os.Stdout, nil
	}
	if opts.AppDataDir == "" {
		return nil, fmt.Errorf("logger: AppDataDir required when not StdoutOnly")
	}
	logDir := filepath.Join(opts.AppDataDir, "logs")
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return nil, err
	}
	rotateLogs(logDir)

	logFile := filepath.Join(logDir, "pad-analyzer.log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) // #nosec G304 -- log path under the app's own data dir
	if err != nil {
		return nil, err
	}
	return io.MultiWriter(f, os.Stderr), nil
}

// newHandler is the JSON handler InitWith installs, writing to w.
func newHandler(w io.Writer, opts Options) slog.Handler {
	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     parseLevel(opts.Level),
		AddSource: opts.AddSource,
	})
	if len(opts.BaseAttrs) > 0 {
		h = h.WithAttrs(opts.BaseAttrs)
	}
	return h
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
