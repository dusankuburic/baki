package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"pad-analyzer/internal/testutil/pgtest"
)

// TestApp_StopOrder pins the order the stop hooks of the real graph run in. fx runs them in reverse registration
// order, so the order of the Invoke list (and of the providers the invokes pull in) is load-bearing: the jobs stop
// before the server drains the background runners, the audit trail drains after the server, the pool closes after
// everything that queries. A move of a provider or an invoke that changes it fails here.
//
// fx runs the observability module's invokes before the application's list, so initTelemetry registers its hook
// before what the list constructs (the router, the audit recorder, the auth manager, the storage): telemetry starts
// first and flushes last, after the audit trail drains and the pool closes.
//
// Each boot runs in a child process (TestBootChild): the server's stop closes every lifecycle.Bounded runner of the
// process, and the boot installs the process-wide logger, so an in-process boot would break the tests after it.
func TestApp_StopOrder(t *testing.T) {
	t.Run("desktop", func(t *testing.T) {
		rep, stderr := bootChild(t)
		// The Tauri handshake: the CONFIG line is on stdout once the graph is built (the listener is bound at Invoke
		// time, before any start hook), and nothing else is ever written there (the logs go to a file and stderr).
		if !strings.HasPrefix(rep.StdoutAtNew, "CONFIG:{") || strings.Count(rep.StdoutAtNew, "\n") != 1 {
			t.Errorf("stdout once the graph was built = %q, want the CONFIG line alone", rep.StdoutAtNew)
		}
		if rep.Stdout != rep.StdoutAtNew {
			t.Errorf("stdout after start and stop = %q, want only the CONFIG line", rep.Stdout)
		}
		// The desktop boot never reaches the OS keychain (the child's keyring fails every call, which the keyring
		// store reports with this warning), so running the binary in a test touches no developer's keychain.
		if strings.Contains(stderr, "OS keychain unavailable") {
			t.Error("the desktop boot or stop reached the OS keychain")
		}
		checkStops(t, rep, []string{
			"registerDrain",
			"initScanner",
			"startServer",
			"api.NewRouter",
			"audit.Provide",
			"initTelemetry",
		})
	})

	t.Run("cloud", func(t *testing.T) {
		dsn := pgtest.NewDatabase(t, "") // skips without DATABASE_URL
		rep, stderr := bootChild(t,
			"PAD_MODE=cloud",
			"PAD_HOST=127.0.0.1",
			"PAD_PORT=0",
			"PAD_BEHIND_PROXY=true", // cloud auth refuses plain HTTP without a TLS-terminating proxy in front
			"PAD_AUTH_ENABLED=true",
			"PAD_AUTH_SECRET="+randomKey(t),
			"PAD_ENCRYPTION_KEY="+randomKey(t),
			// A rotation under way, so the re-seal pass is scheduled too.
			"PAD_ENCRYPTION_KEY_PREVIOUS="+randomKey(t),
			"PAD_STORAGE=database",
			"PAD_DATABASE_URL="+dsn,
			"PAD_DB_REQUIRE_SSL=false",
		)
		if strings.Contains(rep.StdoutAtNew, "CONFIG:") {
			t.Errorf("a cloud boot printed the desktop handshake: %q", rep.StdoutAtNew)
		}
		if strings.Contains(stderr+rep.Stdout, "OS keychain unavailable") {
			t.Error("the cloud boot or stop reached the OS keychain")
		}
		checkStops(t, rep, []string{
			"registerDrain",
			"scheduleSecretReseal",
			"scheduleFieldEncryptionBackfill",
			"audit.ScheduleOrgBackfill",
			"initScanner",
			"startServer",
			"initRetentionPurge",
			"api.NewRouter",
			"provideAuthManager",
			"audit.Provide",
			"provideStorageBackend",
			"initTelemetry",
		})
	})
}

func checkStops(t *testing.T, rep bootReport, want []string) {
	t.Helper()
	for _, e := range rep.Errors {
		t.Errorf("boot: %s", e)
	}
	if !slices.Equal(rep.Stops, want) {
		t.Errorf("stop hooks ran in this order (by the function that registered each):\n  %s\nwant:\n  %s",
			strings.Join(rep.Stops, "\n  "), strings.Join(want, "\n  "))
	}
}

// bootReport is what a child boot reports to its parent.
type bootReport struct {
	// Stops names, in the order they ran, the function that registered each stop hook (hookCaller).
	Stops  []string
	Errors []string
	// StdoutAtNew is what building the graph printed on stdout: every invoke had run, no start hook yet.
	StdoutAtNew string
	// Stdout is everything printed on stdout until the app had stopped.
	Stdout string
}

// bootChildReport names the file a child boot writes its report to; set only for the child.
const bootChildReport = "BAKI_TEST_BOOT_REPORT"

var errKeyringTouched = errors.New("the test boot reached the OS keychain")

// bootChild runs TestBootChild in a child process of this test binary, with the per-user directories at a temporary
// one and env added, and returns its report and stderr.
func bootChild(t *testing.T, env ...string) (bootReport, string) {
	t.Helper()
	home := t.TempDir()
	reportPath := filepath.Join(home, "report.json")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBootChild$", "-test.count=1")
	cmd.Env = hermeticEnv(home, append(env, bootChildReport+"="+reportPath)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child boot: %v\nstderr:\n%s", err, stderr.String())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("child boot report: %v\nstderr:\n%s", err, stderr.String())
	}
	var rep bootReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("child boot report: %v", err)
	}
	return rep, stderr.String()
}

// TestBootChild is the child half of TestApp_StopOrder: it builds the graph main runs (Options), starts it and stops
// it within fx's own budgets (as fx.App.Run does), and writes what it saw to the report file.
func TestBootChild(t *testing.T) {
	reportPath := os.Getenv(bootChildReport)
	if reportPath == "" {
		t.Skip("the child half of TestApp_StopOrder")
	}
	// Every keychain call fails (and the keyring store logs it) instead of reaching the developer's keychain.
	keyring.MockInitWithError(errKeyringTouched)

	stdout, err := os.CreateTemp("", "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdout.Close() }()
	saved := os.Stdout
	os.Stdout = stdout
	defer func() { os.Stdout = saved }()
	readStdout := func() string {
		b, err := os.ReadFile(stdout.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	rec := &stopRecorder{}
	var rep bootReport
	app := fx.New(append(Options(Build{}), fx.WithLogger(func() fxevent.Logger { return rec }))...)
	rep.StdoutAtNew = readStdout()
	if err := app.Err(); err != nil {
		rep.Errors = append(rep.Errors, "build: "+err.Error())
	} else {
		startCtx, cancelStart := context.WithTimeout(context.Background(), app.StartTimeout())
		defer cancelStart()
		if err := app.Start(startCtx); err != nil {
			rep.Errors = append(rep.Errors, "start: "+err.Error())
		}
		stopCtx, cancelStop := context.WithTimeout(context.Background(), app.StopTimeout())
		defer cancelStop()
		if err := app.Stop(stopCtx); err != nil {
			rep.Errors = append(rep.Errors, "stop: "+err.Error())
		}
	}
	rep.Stdout = readStdout()
	rep.Stops, rep.Errors = rec.result(rep.Errors)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stopRecorder is an fx event logger recording the stop hooks as they run and every hook error.
type stopRecorder struct {
	mu    sync.Mutex
	stops []string
	errs  []string
}

func (r *stopRecorder) LogEvent(e fxevent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e := e.(type) {
	case *fxevent.OnStopExecuting:
		r.stops = append(r.stops, hookCaller(e.CallerName))
	case *fxevent.OnStopExecuted:
		if e.Err != nil {
			r.errs = append(r.errs, "stop hook of "+hookCaller(e.CallerName)+": "+e.Err.Error())
		}
	case *fxevent.OnStartExecuted:
		if e.Err != nil {
			r.errs = append(r.errs, "start hook of "+hookCaller(e.CallerName)+": "+e.Err.Error())
		}
	}
}

func (r *stopRecorder) result(errs []string) ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.stops), append(errs, r.errs...)
}

// hookCaller names a function without its module path, and the application's own functions without their package
// too (the names read as they did when the wiring was package main's).
func hookCaller(fn string) string {
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		fn = fn[i+1:]
	}
	if rest, ok := strings.CutPrefix(fn, "app."); ok {
		return rest
	}
	return fn
}

// randomKey is a secret strong enough for cloud mode, random so no key sits in the source.
func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// hermeticEnv is this process's environment for a child boot, with the per-user directories (config, cache, home,
// temp) at home, so the boot never reads or writes the developer's %APPDATA%\pad-analyzer, and without the PAD_* and
// OTEL_* settings of the developer's shell, plus extra.
func hermeticEnv(home string, extra ...string) []string {
	redirected := []string{"APPDATA", "LOCALAPPDATA", "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMP", "TEMP", "TMPDIR"}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		k = strings.ToUpper(k)
		if strings.HasPrefix(k, "PAD_") || strings.HasPrefix(k, "OTEL_") || slices.Contains(redirected, k) {
			continue
		}
		env = append(env, kv)
	}
	for _, k := range redirected {
		env = append(env, k+"="+home)
	}
	return append(env, extra...)
}
