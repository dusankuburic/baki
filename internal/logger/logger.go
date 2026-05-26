package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

func Init(appDataDir string, debug bool) error {
	logDir := filepath.Join(appDataDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}

	rotateLogs(logDir)

	logFile := filepath.Join(logDir, "pad-analyzer.log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	handler := slog.NewJSONHandler(io.MultiWriter(f, os.Stderr), &slog.HandlerOptions{
		Level:     level,
		AddSource: debug,
	})
	defaultLogger := slog.New(handler)
	slog.SetDefault(defaultLogger)
	return nil
}

func Info(msg string, args ...any)  { slog.Info(msg, args...) }
func Error(msg string, args ...any) { slog.Error(msg, args...) }
func Debug(msg string, args ...any) { slog.Debug(msg, args...) }
func Warn(msg string, args ...any)  { slog.Warn(msg, args...) }
