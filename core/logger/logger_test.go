package logger

import (
	"bytes"
	"context"
	"encoding/json"
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

// ---- newHandler: level and base attributes ----------------------------------

// installHandler makes newHandler(opts) the default logger for the test, writing to the returned buffer.
func installHandler(t *testing.T, opts Options) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(newHandler(&buf, opts)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestNewHandler_BaseAttrsOnEveryLine: the base attributes (the replica's instance id) are on every line, however it
// is logged, so two replicas' lines can be told apart in one log stream; once, not repeated by derived loggers.
func TestNewHandler_BaseAttrsOnEveryLine(t *testing.T) {
	buf := installHandler(t, Options{Level: "debug", BaseAttrs: []slog.Attr{slog.String("instance_id", "replica-7")}})
	setExtractor(t, func(ctx context.Context) []slog.Attr {
		if id, _ := ctx.Value(requestKey{}).(string); id != "" {
			return []slog.Attr{slog.String("request_id", id)}
		}
		return nil
	})

	Info("helper")
	Debug("debug helper")
	slog.Warn("direct", "k", "v")
	slog.Default().With("component", "scanner").Info("derived")
	slog.Default().WithGroup("g").Info("grouped", "k", "v")
	FromContext(context.WithValue(context.Background(), requestKey{}, "req-1")).Error("request")
	logPanic("op", "boom")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 7 {
		t.Fatalf("got %d lines, want 7:\n%s", len(lines), buf.String())
	}
	for _, line := range lines {
		if n := bytes.Count(line, []byte(`"instance_id":`)); n != 1 {
			t.Errorf("instance_id appears %d times in %s", n, line)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		if m["instance_id"] != "replica-7" {
			t.Errorf("instance_id = %v in %s", m["instance_id"], line)
		}
	}
	if m := lastLine(t, buf); m["msg"] != "panic recovered" {
		t.Errorf("last line = %v", m)
	}
}

func TestNewHandler_HonoursLevel(t *testing.T) {
	for level, want := range map[string][]string{
		"debug": {"d", "i", "w", "e"},
		"":      {"i", "w", "e"},
		"info":  {"i", "w", "e"},
		"WARN":  {"w", "e"},
		"error": {"e"},
	} {
		buf := installHandler(t, Options{Level: level})
		Debug("d")
		Info("i")
		Warn("w")
		Error("e")
		var got []string
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatalf("level %q: decode %s: %v", level, line, err)
			}
			got = append(got, m["msg"].(string))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("level %q logged %v, want %v", level, got, want)
		}
	}
}

// TestInitWith_FileSinkFailureKeepsLevel (R10, the core half): when the log file cannot be opened, InitWith used to
// return without installing anything, so the process logged through slog's default text logger at info, whatever
// the configured level. It now writes JSON to stderr at that level and still reports the error.
func TestInitWith_FileSinkFailureKeepsLevel(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := InitWith(Options{Level: "debug", BaseAttrs: []slog.Attr{slog.String("instance_id", "replica-7")}}) // no AppDataDir
	if err == nil {
		t.Fatal("InitWith without AppDataDir reported no error")
	}
	h := slog.Default().Handler()
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("the configured debug level was dropped")
	}
	if _, ok := h.(*slog.JSONHandler); !ok {
		t.Errorf("handler = %T, want the JSON handler", h)
	}
}
