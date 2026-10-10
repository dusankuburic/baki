package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestInitLogger_HonoursLevelInLocalMode (R10): outside the cloud the configured level used to be dropped (the logger
// was installed a second time at info), so PAD_LOG_LEVEL=debug logged nothing at debug on the desktop or local+db.
// The lines go to the log file and stderr; stdout stays the desktop handshake's.
func TestInitLogger_HonoursLevelInLocalMode(t *testing.T) {
	for profile, env := range map[capability.Profile][]string{
		capability.Desktop: {"PAD_MODE=local", "PAD_LOG_LEVEL=debug"},
		// Only initLogger runs: the database is never reached.
		capability.LocalDB: {"PAD_MODE=local", "PAD_LOG_LEVEL=debug", "PAD_STORAGE=database", "PAD_DATABASE_URL=postgres://localhost/unused"},
	} {
		t.Run(string(profile), func(t *testing.T) {
			rep, stderr := observabilityChild(t, env...)
			if rep.Profile != profile {
				t.Fatalf("the child's profile = %q, want %q", rep.Profile, profile)
			}
			for where, out := range map[string]string{"the log file": readLogFile(t, rep), "stderr": stderr} {
				line, ok := findLogLine(out, probeMessage)
				if !ok {
					t.Errorf("%s has no %q line at debug; it has:\n%s", where, probeMessage, out)
					continue
				}
				if line["level"] != "DEBUG" {
					t.Errorf("%s: probe line level = %v, want DEBUG", where, line["level"])
				}
			}
			if rep.Stdout != "" {
				t.Errorf("the local logger wrote to stdout (the desktop handshake's): %q", rep.Stdout)
			}
		})
	}
}

// TestInitLogger_NamesTheReplica: the instance id is on every log line, on the events forwarded to the error sink
// and in the job state's holder column.
func TestInitLogger_NamesTheReplica(t *testing.T) {
	rep, _ := observabilityChild(t, "PAD_MODE=local", "PAD_INSTANCE_ID=replica-local")
	for _, e := range rep.Errors {
		t.Error(e)
	}
	lines := logLines(t, "the log file", readLogFile(t, rep))
	if len(lines) == 0 {
		t.Error("the log file is empty")
	}
	for _, line := range lines {
		if line["instance_id"] != "replica-local" {
			t.Errorf("log line without the instance id: %v", line)
		}
	}
	if rep.JobHolder != "replica-local" {
		t.Errorf("job state holder = %q, want the instance id", rep.JobHolder)
	}
	if rep.ErrorAttrs["instance_id"] != "replica-local" || rep.ErrorAttrs["probe"] != "attr" {
		t.Errorf("error sink attrs = %v, want the call site's plus instance_id", rep.ErrorAttrs)
	}
}

// TestApp_LogsAreJSONFromTheFirstLine: the logger is installed before anything that logs is built. The invokes of
// the di modules (which build the storage, so the migrations, and the mailer) used to run before initLogger: their
// lines went to Go's default text logger on stderr, without the JSON format the log alerts parse or the instance id.
func TestApp_LogsAreJSONFromTheFirstLine(t *testing.T) {
	t.Run("desktop", func(t *testing.T) {
		rep, stderr := bootChild(t)
		for _, e := range rep.Errors {
			t.Errorf("boot: %s", e)
		}
		noTextLogger(t, stderr)
		lines := logLines(t, "stderr", stderr)
		if len(lines) == 0 {
			t.Fatal("the desktop boot logged nothing on stderr")
		}
		// The desktop takes no PAD_INSTANCE_ID (no PAD_MODE): the instance id is the host name.
		id, _ := lines[0]["instance_id"].(string)
		for _, line := range lines {
			if id == "" || line["instance_id"] != id {
				t.Errorf("log line without the instance id %q: %v", id, line)
			}
		}
	})

	t.Run("cloud", func(t *testing.T) {
		dsn := pgtest.NewDatabase(t, "") // a fresh database, so the boot migrates it; skips without DATABASE_URL
		rep, stderr := bootChild(t,
			"PAD_MODE=cloud",
			"PAD_HOST=127.0.0.1",
			"PAD_PORT=0",
			"PAD_BEHIND_PROXY=true",
			"PAD_AUTH_ENABLED=true",
			"PAD_AUTH_SECRET="+randomKey(t),
			"PAD_ENCRYPTION_KEY="+randomKey(t),
			"PAD_STORAGE=database",
			"PAD_DATABASE_URL="+dsn,
			"PAD_DB_REQUIRE_SSL=false",
			"PAD_INSTANCE_ID=replica-cloud",
		)
		for _, e := range rep.Errors {
			t.Errorf("boot: %s", e)
		}
		noTextLogger(t, stderr)
		migrations := 0
		for _, line := range logLines(t, "stdout", rep.Stdout) {
			if line["instance_id"] != "replica-cloud" {
				t.Errorf("log line without the instance id: %v", line)
			}
			if line["msg"] == "schema migration applied" {
				migrations++
			}
		}
		if migrations == 0 {
			t.Errorf("no JSON \"schema migration applied\" line on stdout:\n%s", rep.Stdout)
		}
	})
}

// TestTelemetryOptions_FromConfig (R15): the resource names the deployment from the loaded configuration (whatever
// its source: PAD_MODE used to be read from the environment, empty with a PAD_CONFIG file) and the replica by the
// configuration's instance id.
func TestTelemetryOptions_FromConfig(t *testing.T) {
	for _, tc := range []struct {
		mode    config.DeploymentMode
		backend config.StorageBackend
		want    capability.Profile
	}{
		{config.ModeLocal, config.StorageLocal, capability.Desktop},
		{config.ModeLocal, config.StorageDatabase, capability.LocalDB},
		{config.ModeCloud, config.StorageDatabase, capability.Cloud},
	} {
		cfg := config.Default()
		cfg.Mode = tc.mode
		cfg.Storage.Backend = tc.backend
		cfg.Runtime.InstanceID = "replica-9"
		cfg.Runtime.OTLPEndpoint = "http://collector:4318"
		got := telemetryOptions(cfg, Build{Version: "1.2.3", Commit: "abc"})
		if got.Environment != string(tc.want) {
			t.Errorf("%s/%s: Environment = %q, want the profile %q", tc.mode, tc.backend, got.Environment, tc.want)
		}
		if got.InstanceID != "replica-9" || got.Version != "1.2.3" || got.OTLPEndpoint != "http://collector:4318" ||
			got.ServiceName != "baki-backend" {
			t.Errorf("%s/%s: options = %+v", tc.mode, tc.backend, got)
		}
	}
}

// probeMessage is the debug line the observability child logs.
const probeMessage = "observability probe"

// observabilityChildReport names the file the observability child writes its report to; set only for the child.
const observabilityChildReport = "BAKI_TEST_OBSERVABILITY_REPORT"

// testJobInstance is the job the observability child runs to read the holder it records.
var testJobInstance = lifecycle.DeclareJob(lifecycle.JobDecl{Name: "test_app_instance", Schedule: "tests only"})

// observabilityReport is what the observability child reports to its parent.
type observabilityReport struct {
	Profile capability.Profile
	// ConfigDir is the desktop's configuration directory (the log file is under it).
	ConfigDir  string
	Stdout     string
	JobHolder  string
	ErrorAttrs map[string]any
	Errors     []string
}

// observabilityChild runs TestObservabilityChild in a child process (the logger, the job state's instance id and the
// error sink are process-wide, and the log file stays open until the process exits), with the per-user directories at
// a temporary one and env added, and returns its report and stderr.
func observabilityChild(t *testing.T, env ...string) (observabilityReport, string) {
	t.Helper()
	home := t.TempDir()
	reportPath := filepath.Join(home, "report.json")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestObservabilityChild$", "-test.count=1")
	cmd.Env = hermeticEnv(home, append(env, observabilityChildReport+"="+reportPath)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v\nstderr:\n%s", err, stderr.String())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("child report: %v\nstderr:\n%s", err, stderr.String())
	}
	var rep observabilityReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("child report: %v", err)
	}
	return rep, stderr.String()
}

// TestObservabilityChild is the child half of the initLogger tests: it loads the configuration, runs initLogger as
// the boot does, logs a debug line, runs a job and reports an error, and writes what it saw to the report file.
func TestObservabilityChild(t *testing.T) {
	reportPath := os.Getenv(observabilityChildReport)
	if reportPath == "" {
		t.Skip("the child half of the initLogger tests")
	}
	stdout, err := os.CreateTemp("", "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdout.Close() }()
	saved := os.Stdout
	os.Stdout = stdout
	defer func() { os.Stdout = saved }()

	var rep observabilityReport
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	rep.Profile = capability.Of(cfg).Profile()
	if rep.ConfigDir, err = storage.ConfigDir(); err != nil {
		t.Fatal(err)
	}
	initLogger(cfg)
	slog.Debug(probeMessage)

	state := &holderState{}
	job := (&lifecycle.Group{}).NewSingleton(lifecycle.Job{Spec: testJobInstance, Run: func(context.Context) error { return nil }}, nil, state)
	if err := job.RunOnce(context.Background()); err != nil {
		rep.Errors = append(rep.Errors, "job: "+err.Error())
	}
	job.Stop()
	rep.JobHolder = state.holder

	sink := &attrsSink{}
	errreport.Register(sink)
	errreport.CaptureError(context.Background(), "probe", errors.New("probe error"), errreport.Attrs{"probe": "attr"})
	errreport.Register(nil)
	rep.ErrorAttrs = sink.attrs

	b, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	rep.Stdout = string(b)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readLogFile returns the desktop log file the child wrote.
func readLogFile(t *testing.T, rep observabilityReport) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rep.ConfigDir, "logs", "pad-analyzer.log"))
	if err != nil {
		t.Fatalf("log file: %v", err)
	}
	return string(b)
}

// textLoggerLine matches a line of Go's default logger, which slog writes to until a handler is installed.
var textLoggerLine = regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (DEBUG|INFO|WARN|ERROR) .*$`)

// noTextLogger fails for every line of out that Go's default text logger wrote.
func noTextLogger(t *testing.T, out string) {
	t.Helper()
	for _, line := range textLoggerLine.FindAllString(out, -1) {
		t.Errorf("logged before the logger was installed: %s", line)
	}
}

// logLines parses out as JSON log lines, failing for a line that is not one.
func logLines(t *testing.T, where, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Errorf("%s: not a JSON log line: %q", where, raw)
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// findLogLine returns the first JSON line of out whose msg is msg.
func findLogLine(out, msg string) (map[string]any, bool) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) == nil && line["msg"] == msg {
			return line, true
		}
	}
	return nil, false
}

// holderState is a lifecycle.JobState recording the holder of the last run started.
type holderState struct{ holder string }

func (s *holderState) RecordJobStart(_ context.Context, _, holder string, _ time.Time) error {
	s.holder = holder
	return nil
}

func (*holderState) RecordJobEnd(context.Context, string, time.Time, string) error { return nil }

func (*holderState) JobLastSuccess(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

// attrsSink is an errreport.Sink recording the attributes of the last event.
type attrsSink struct{ attrs errreport.Attrs }

func (s *attrsSink) CaptureError(_ context.Context, _ error, attrs errreport.Attrs) { s.attrs = attrs }

func (s *attrsSink) CapturePanic(_ context.Context, _ any, _ []byte, attrs errreport.Attrs) {
	s.attrs = attrs
}
