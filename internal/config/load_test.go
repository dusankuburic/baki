package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// clearLoadEnv unsets the variables that choose Load's source, so a test starts from the desktop defaults whatever
// the developer's shell exports.
func clearLoadEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PAD_CONFIG", "PAD_MODE", "PAD_PORT"} {
		t.Setenv(k, "")
	}
}

// writeConfigFile writes a PAD_CONFIG file and points PAD_CONFIG at it.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	clearLoadEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_CONFIG", path)
	return path
}

// TestValidate_RequestTimeoutMustBePositive (R8): the default deadline of every request. 0 (or less) was accepted
// and timed out every request at once.
func TestValidate_RequestTimeoutMustBePositive(t *testing.T) {
	for _, v := range []string{"0s", "-5s"} {
		t.Run(v, func(t *testing.T) {
			clearLoadEnv(t)
			t.Setenv("PAD_MODE", "local")
			t.Setenv("PAD_REQUEST_TIMEOUT", v)
			cfg, err := LoadFromEnvRaw()
			if err == nil {
				err = Validate(cfg)
			}
			if err == nil || !strings.Contains(err.Error(), "PAD_REQUEST_TIMEOUT") {
				t.Fatalf("PAD_REQUEST_TIMEOUT=%s: err = %v, want a refusal naming the variable", v, err)
			}
		})
	}
}

// TestLoad_ConfigFileWithInvalidDurationFails: a duration in a PAD_CONFIG file is held to the rules of its PAD_*
// variable. A bad one used to boot with scanning or retention silently off, the AI resilience silently at its
// defaults, or (R8) every request timing out at once.
func TestLoad_ConfigFileWithInvalidDurationFails(t *testing.T) {
	for _, tc := range []struct{ body, field string }{
		{`{"Governance":{"ScanInterval":"every hour"}}`, "ScanInterval"},
		{`{"Governance":{"ScanInterval":"-1h"}}`, "ScanInterval"},
		{`{"Governance":{"RetentionPurgeInterval":"daily"}}`, "RetentionPurgeInterval"},
		{`{"Governance":{"RetentionPurgeInterval":"-24h"}}`, "RetentionPurgeInterval"},
		{`{"Storage":{"DBConnMaxLifetime":"-1m"}}`, "DBConnMaxLifetime"},
		{`{"Storage":{"DBConnMaxIdleTime":"5 minutes"}}`, "DBConnMaxIdleTime"},
		{`{"Storage":{"DBStatementTimeout":"-30s"}}`, "DBStatementTimeout"},
		{`{"Runtime":{"CircuitBreakerOpenDur":"0s"}}`, "CircuitBreakerOpenDur"},
		{`{"Runtime":{"RetryBaseDelay":"fast"}}`, "RetryBaseDelay"},
		{`{"Runtime":{"RequestTimeout":"0s"}}`, "RequestTimeout"},
		{`{"Runtime":{"RequestTimeout":"soon"}}`, "RequestTimeout"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			writeConfigFile(t, tc.body)
			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load booted with %s (request timeout %v, scan interval %v)", tc.body, cfg.Runtime.RequestTimeout, cfg.Governance.ScanInterval)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("Load: %v; want the error to name %s", err, tc.field)
			}
		})
	}
}

// TestLoad_ConfigFileOfTodaysFormatKeepsWorking: testdata/config_pre5a.json is what encoding/json wrote for the
// struct before the durations were typed (every field, durations as strings, "" for the ones left unset). It loads
// as it did: the same values, and the same defaults where it said "".
func TestLoad_ConfigFileOfTodaysFormatKeepsWorking(t *testing.T) {
	clearLoadEnv(t)
	path, err := filepath.Abs(filepath.Join("testdata", "config_pre5a.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"Governance.ScanInterval", cfg.Governance.ScanInterval.Std(), time.Hour},
		{"Governance.RetentionPurgeInterval", cfg.Governance.RetentionPurgeInterval.Std(), 12 * time.Hour},
		{`Storage.DBConnMaxLifetime ("" = the default)`, cfg.Storage.DBConnMaxLifetime.Std(), time.Hour},
		{"Storage.DBConnMaxIdleTime", cfg.Storage.DBConnMaxIdleTime.Std(), 10 * time.Minute},
		{`Storage.DBStatementTimeout ("0s" = off)`, cfg.Storage.DBStatementTimeout.Std(), 0},
		{"Runtime.CircuitBreakerOpenDur", cfg.Runtime.CircuitBreakerOpenDur.Std(), 45 * time.Second},
		{`Runtime.RetryBaseDelay ("" = the default)`, cfg.Runtime.RetryBaseDelay.Std(), 500 * time.Millisecond},
		{"Runtime.RequestTimeout", cfg.Runtime.RequestTimeout.Std(), 90 * time.Second},
		{"Runtime.ShutdownDrainDelay (absent: the cloud default)", cfg.Runtime.ShutdownDrainDelay.Std(), 3 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if cfg.Mode != ModeCloud || cfg.Server.Port != 8080 || cfg.Storage.DBMaxOpenConns != 15 || cfg.Log.Level != "warn" ||
		cfg.Governance.AuditRetentionDays != 90 || len(cfg.Server.AllowedOrigins) != 1 || cfg.Auth.SecretEphemeral {
		t.Errorf("the other settings did not load as written: %+v", cfg.Server)
	}
	if cfg.Runtime.InstanceID == "" {
		t.Error("no instance id resolved")
	}
	// The file's credentials are plain strings under the same names; they arrive as secret.Value.
	if got := cfg.Auth.Secret.Reveal(); got != "a-long-random-cloud-signing-secret-of-a-fixture" {
		t.Errorf("Auth.Secret = %q, want the file's", got)
	}
	if got := cfg.Storage.DatabaseURL.Reveal(); got != "postgres://baki_app@db.example.com:5432/baki?sslmode=require" {
		t.Errorf("Storage.DatabaseURL = %q, want the file's", got)
	}
	if cfg.Auth.EncryptionKey.IsSet() || cfg.Auth.SSO.ClientSecret.IsSet() || cfg.Email.Password.IsSet() || cfg.Redis.URL.IsSet() {
		t.Error(`the file's "" credentials loaded as set`)
	}
}

// TestLoad_InstanceIDResolution: PAD_INSTANCE_ID (or a config file's Runtime.InstanceID), else the replica name
// Azure Container Apps sets, else the host name, else a random id. Never empty.
func TestLoad_InstanceIDResolution(t *testing.T) {
	load := func(t *testing.T) string {
		t.Helper()
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg.Runtime.InstanceID
	}
	t.Run("PAD_INSTANCE_ID", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_MODE", "local")
		t.Setenv("PAD_INSTANCE_ID", " replica-7 ")
		t.Setenv("CONTAINER_APP_REPLICA_NAME", "aca-replica")
		if got := load(t); got != "replica-7" {
			t.Errorf("instance id = %q, want replica-7", got)
		}
	})
	t.Run("a config file's", func(t *testing.T) {
		writeConfigFile(t, `{"Runtime":{"InstanceID":"from-file"}}`)
		t.Setenv("CONTAINER_APP_REPLICA_NAME", "aca-replica")
		if got := load(t); got != "from-file" {
			t.Errorf("instance id = %q, want from-file", got)
		}
	})
	t.Run("the ACA replica name", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_INSTANCE_ID", "")
		t.Setenv("CONTAINER_APP_REPLICA_NAME", "baki-app--0000012-7d9c5b8f4-x2x7k")
		if got := load(t); got != "baki-app--0000012-7d9c5b8f4-x2x7k" {
			t.Errorf("instance id = %q, want the replica name", got)
		}
	})
	t.Run("the host name", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_INSTANCE_ID", "")
		t.Setenv("CONTAINER_APP_REPLICA_NAME", "")
		want, err := os.Hostname()
		if err != nil || want == "" {
			t.Skip("no host name here")
		}
		if got := load(t); got != want {
			t.Errorf("instance id = %q, want the host name %q", got, want)
		}
	})
	t.Run("random when nothing names it", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_INSTANCE_ID", "")
		t.Setenv("CONTAINER_APP_REPLICA_NAME", "")
		prev := hostname
		hostname = func() (string, error) { return "", os.ErrNotExist }
		t.Cleanup(func() { hostname = prev })
		a, b := load(t), load(t)
		if a == "" || a == b {
			t.Errorf("instance ids %q, %q: want a random one per process", a, b)
		}
	})
}

// TestLoad_ShutdownDrainDelay: PAD_SHUTDOWN_DRAIN_DELAY, the pause between "draining" and ending the streams (ADR
// 0024): 3 s by default in the cloud profile, 0 for local and desktop; an explicit value wins, 0 included, on both
// load paths; at most 10 s, so the stop budget holds.
func TestLoad_ShutdownDrainDelay(t *testing.T) {
	cloudEnv := func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_MODE", "cloud")
		t.Setenv("PAD_AUTH_ENABLED", "true")
		t.Setenv("PAD_AUTH_SECRET", "a-long-random-cloud-signing-secret-0123456789")
		t.Setenv("PAD_STORAGE", "database")
		t.Setenv("PAD_DATABASE_URL", "postgres://localhost/test")
		t.Setenv("PAD_BEHIND_PROXY", "true")
	}
	const cloudFile = `{"Mode":"cloud","Server":{"BehindProxy":true},"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test"},"Auth":{"Enabled":true,"Secret":"a-long-random-cloud-signing-secret-0123456789"}`
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  time.Duration
	}{
		{"desktop defaults", clearLoadEnv, 0},
		{"local env", func(t *testing.T) { clearLoadEnv(t); t.Setenv("PAD_MODE", "local") }, 0},
		{"cloud env", cloudEnv, 3 * time.Second},
		{"cloud env, explicit 0s", func(t *testing.T) { cloudEnv(t); t.Setenv("PAD_SHUTDOWN_DRAIN_DELAY", "0s") }, 0},
		{"cloud env, explicit 10s", func(t *testing.T) { cloudEnv(t); t.Setenv("PAD_SHUTDOWN_DRAIN_DELAY", "10s") }, 10 * time.Second},
		{"local env, explicit 2s", func(t *testing.T) {
			clearLoadEnv(t)
			t.Setenv("PAD_MODE", "local")
			t.Setenv("PAD_SHUTDOWN_DRAIN_DELAY", "2s")
		}, 2 * time.Second},
		{"cloud file", func(t *testing.T) { writeConfigFile(t, cloudFile+`}`) }, 3 * time.Second},
		{"cloud file, explicit 0s", func(t *testing.T) {
			writeConfigFile(t, cloudFile+`,"Runtime":{"ShutdownDrainDelay":"0s"}}`)
		}, 0},
		{"local file", func(t *testing.T) { writeConfigFile(t, `{"Mode":"local"}`) }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Runtime.ShutdownDrainDelay.Std(); got != tc.want {
				t.Errorf("drain delay = %v, want %v", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"11s", "-1s"} {
		t.Run(bad+" refused", func(t *testing.T) {
			cloudEnv(t)
			t.Setenv("PAD_SHUTDOWN_DRAIN_DELAY", bad)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAD_SHUTDOWN_DRAIN_DELAY") {
				t.Errorf("PAD_SHUTDOWN_DRAIN_DELAY=%s: err = %v, want a refusal naming it", bad, err)
			}
		})
	}
}

// TestLoad_SlicesAreDetached: the configuration hands out every slice with its own backing array and no spare
// capacity, so a reader that appends to one (it may not write to the configuration itself: archtest) copies
// instead of writing where another reader can see.
func TestLoad_SlicesAreDetached(t *testing.T) {
	cfg := Default()
	var spare []reflect.Value
	var fill func(v reflect.Value)
	fill = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			for i := range v.NumField() {
				fill(v.Field(i))
			}
		case reflect.Slice:
			s := reflect.MakeSlice(v.Type(), 1, 8)
			v.Set(s)
			spare = append(spare, s)
		}
	}
	fill(reflect.ValueOf(cfg).Elem())
	if len(spare) < 5 {
		t.Fatalf("found %d slice fields, want at least the five lists", len(spare))
	}
	detachSlices(cfg)
	i := 0
	var check func(v reflect.Value, path string)
	check = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Struct:
			for f := range v.NumField() {
				check(v.Field(f), path+"."+v.Type().Field(f).Name)
			}
		case reflect.Slice:
			if v.Cap() != v.Len() {
				t.Errorf("%s: cap %d, len %d; want no spare capacity", path, v.Cap(), v.Len())
			}
			if v.Pointer() == spare[i].Pointer() {
				t.Errorf("%s shares its backing array with the slice it was loaded from", path)
			}
			i++
		}
	}
	check(reflect.ValueOf(cfg).Elem(), "Config")

	clearLoadEnv(t)
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_ALLOWED_ORIGINS", "https://a.example.com,https://b.example.com,https://c.example.com")
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if o := loaded.Server.AllowedOrigins; len(o) != 3 || cap(o) != 3 {
		t.Errorf("AllowedOrigins len %d cap %d, want 3 and 3", len(o), cap(o))
	}
}

// TestHealthcheckPort (R14): the healthcheck probes the port the server listens on: a PAD_CONFIG file's, else
// PAD_PORT, else 8080. It reads only that, so a setting the probe does not use (a cloud file that would fail
// validation, a missing secret) never fails it.
func TestHealthcheckPort(t *testing.T) {
	t.Run("PAD_PORT", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_PORT", "9090")
		if p, err := HealthcheckPort(); err != nil || p != 9090 {
			t.Errorf("port = %d, %v; want 9090", p, err)
		}
	})
	t.Run("default", func(t *testing.T) {
		clearLoadEnv(t)
		if p, err := HealthcheckPort(); err != nil || p != 8080 {
			t.Errorf("port = %d, %v; want 8080", p, err)
		}
	})
	t.Run("a config file's port wins over PAD_PORT, as it does for the server", func(t *testing.T) {
		writeConfigFile(t, `{"Mode":"cloud","Server":{"Port":7070}}`) // would not validate: no auth, no database
		t.Setenv("PAD_PORT", "9090")
		if p, err := HealthcheckPort(); err != nil || p != 7070 {
			t.Errorf("port = %d, %v; want the file's 7070", p, err)
		}
	})
	t.Run("a config file without a port", func(t *testing.T) {
		writeConfigFile(t, `{"Mode":"local"}`)
		if p, err := HealthcheckPort(); err != nil || p != 8080 {
			t.Errorf("port = %d, %v; want 8080", p, err)
		}
	})
	t.Run("a bad PAD_PORT", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_PORT", "http")
		if _, err := HealthcheckPort(); err == nil {
			t.Error("PAD_PORT=http accepted")
		}
	})
}

// TestLoad_ReplicaAndServerConnectionKnobs: PAD_MAX_REPLICAS and PAD_DB_SERVER_MAX_CONNS, which infra/main.bicep
// emits for the database boot check (docs/architecture/operations.md). 0 means unknown; negatives are refused.
func TestLoad_ReplicaAndServerConnectionKnobs(t *testing.T) {
	clearLoadEnv(t)
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_MAX_REPLICAS", "3")
	t.Setenv("PAD_DB_SERVER_MAX_CONNS", "50")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.MaxReplicas != 3 || cfg.Storage.DBServerMaxConns != 50 {
		t.Errorf("MaxReplicas %d, DBServerMaxConns %d; want 3 and 50", cfg.Storage.MaxReplicas, cfg.Storage.DBServerMaxConns)
	}
	for _, k := range []string{"PAD_MAX_REPLICAS", "PAD_DB_SERVER_MAX_CONNS"} {
		t.Run(k+" negative", func(t *testing.T) {
			t.Setenv(k, "-1")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), k) {
				t.Errorf("%s=-1: err = %v, want a refusal naming it", k, err)
			}
		})
	}
}

// TestValidate_DurationRanges: the range of every duration setting, checked by Validate on every load path.
func TestValidate_DurationRanges(t *testing.T) {
	for _, tc := range []struct {
		env     string
		set     func(*Config, Duration)
		ok, bad []time.Duration
	}{
		{"PAD_REQUEST_TIMEOUT", func(c *Config, d Duration) { c.Runtime.RequestTimeout = d },
			[]time.Duration{time.Millisecond, time.Hour}, []time.Duration{0, -time.Second}},
		{"PAD_SCAN_INTERVAL", func(c *Config, d Duration) { c.Governance.ScanInterval = d },
			[]time.Duration{0, time.Hour}, []time.Duration{-time.Second}},
		{"PAD_RETENTION_PURGE_INTERVAL", func(c *Config, d Duration) { c.Governance.RetentionPurgeInterval = d },
			[]time.Duration{0, 24 * time.Hour}, []time.Duration{-time.Second}},
		{"PAD_DB_CONN_MAX_LIFETIME", func(c *Config, d Duration) { c.Storage.DBConnMaxLifetime = d },
			[]time.Duration{0, time.Hour}, []time.Duration{-time.Second}},
		{"PAD_DB_CONN_MAX_IDLE_TIME", func(c *Config, d Duration) { c.Storage.DBConnMaxIdleTime = d },
			[]time.Duration{0, time.Minute}, []time.Duration{-time.Second}},
		{"PAD_DB_STATEMENT_TIMEOUT", func(c *Config, d Duration) { c.Storage.DBStatementTimeout = d },
			[]time.Duration{0, time.Minute}, []time.Duration{-time.Second}},
		{"PAD_CB_OPEN_DURATION", func(c *Config, d Duration) { c.Runtime.CircuitBreakerOpenDur = d },
			[]time.Duration{time.Second}, []time.Duration{0, -time.Second}},
		{"PAD_RETRY_BASE_DELAY", func(c *Config, d Duration) { c.Runtime.RetryBaseDelay = d },
			[]time.Duration{time.Millisecond}, []time.Duration{0, -time.Second}},
		{"PAD_SHUTDOWN_DRAIN_DELAY", func(c *Config, d Duration) { c.Runtime.ShutdownDrainDelay = d },
			[]time.Duration{0, 10 * time.Second}, []time.Duration{-time.Second, 10*time.Second + 1}},
	} {
		for _, d := range tc.ok {
			cfg := Default()
			tc.set(cfg, Duration(d))
			if err := Validate(cfg); err != nil {
				t.Errorf("%s=%v refused: %v", tc.env, d, err)
			}
		}
		for _, d := range tc.bad {
			cfg := Default()
			tc.set(cfg, Duration(d))
			if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), tc.env) {
				t.Errorf("%s=%v: Validate = %v, want a refusal naming %s", tc.env, d, err, tc.env)
			}
		}
	}
}
