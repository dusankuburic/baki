package config

import (
	"strings"
	"testing"
	"time"
)

// dbDefaultsCloudEnv is a valid cloud configuration in the environment, for the default checks below.
func dbDefaultsCloudEnv(t *testing.T) {
	t.Helper()
	clearLoadEnv(t)
	t.Setenv("PAD_MODE", "cloud")
	t.Setenv("PAD_AUTH_ENABLED", "true")
	t.Setenv("PAD_AUTH_SECRET", "a-long-random-cloud-signing-secret-0123456789")
	t.Setenv("PAD_STORAGE", "database")
	t.Setenv("PAD_DATABASE_URL", "postgres://localhost/test")
	t.Setenv("PAD_BEHIND_PROXY", "true")
	for _, k := range []string{"PAD_AUDIT_RETENTION_DAYS", "PAD_REQUEST_TIMEOUT", "PAD_DB_IDLE_TX_TIMEOUT", "PAD_DB_ALLOW_BYPASSRLS"} {
		t.Setenv(k, "")
	}
}

// dbDefaultsCloudFile is a valid cloud configuration file, without its closing brace so a case can add sections.
const dbDefaultsCloudFile = `{"Mode":"cloud","Server":{"BehindProxy":true},"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test"},"Auth":{"Enabled":true,"Secret":"a-long-random-cloud-signing-secret-0123456789"}`

// TestLoad_AuditRetentionDefault: PAD_AUDIT_RETENTION_DAYS defaults to DefaultCloudAuditRetentionDays in the cloud
// profile, so the audit trail, analysis history, usage, alerts and rule notices stop growing without bound; local
// and the desktop keep 0 (forever). An explicit value wins on both load paths, 0 included: an operator's 0 still
// keeps everything, and an empty variable is unset, as for every PAD_* variable.
func TestLoad_AuditRetentionDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  int
	}{
		{"desktop defaults", clearLoadEnv, 0},
		{"local env", func(t *testing.T) { clearLoadEnv(t); t.Setenv("PAD_MODE", "local") }, 0},
		{"local env with a database and accounts", func(t *testing.T) {
			clearLoadEnv(t)
			t.Setenv("PAD_MODE", "local")
			t.Setenv("PAD_STORAGE", "database")
			t.Setenv("PAD_DATABASE_URL", "postgres://localhost/test")
			t.Setenv("PAD_AUTH_ENABLED", "true")
			t.Setenv("PAD_AUTH_SECRET", "local-mode-signing-secret-0123456789abcdef")
		}, 0},
		{"cloud env", dbDefaultsCloudEnv, 400},
		{"cloud env, explicit 0 keeps forever", func(t *testing.T) { dbDefaultsCloudEnv(t); t.Setenv("PAD_AUDIT_RETENTION_DAYS", "0") }, 0},
		{"cloud env, explicit 30", func(t *testing.T) { dbDefaultsCloudEnv(t); t.Setenv("PAD_AUDIT_RETENTION_DAYS", "30") }, 30},
		{"local env, explicit 90", func(t *testing.T) {
			clearLoadEnv(t)
			t.Setenv("PAD_MODE", "local")
			t.Setenv("PAD_AUDIT_RETENTION_DAYS", "90")
		}, 90},
		{"cloud file", func(t *testing.T) { writeConfigFile(t, dbDefaultsCloudFile+`}`) }, 400},
		{"cloud file, explicit 0 keeps forever", func(t *testing.T) {
			writeConfigFile(t, dbDefaultsCloudFile+`,"Governance":{"AuditRetentionDays":0}}`)
		}, 0},
		{"local file", func(t *testing.T) { writeConfigFile(t, `{"Mode":"local"}`) }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Governance.AuditRetentionDays; got != tc.want {
				t.Errorf("AuditRetentionDays = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLoad_DBIdleTxTimeout: PAD_DB_IDLE_TX_TIMEOUT, the server's idle_in_transaction_session_timeout. Unset it is
// the request timeout plus DBIdleTxTimeoutMargin, derived when the configuration loads from whichever request
// timeout it ends up with, so only a handler already past its deadline is cut; an explicit value wins, "0s" turning
// it off, on both load paths.
func TestLoad_DBIdleTxTimeout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  time.Duration
	}{
		{"desktop defaults", clearLoadEnv, 30*time.Second + DBIdleTxTimeoutMargin},
		{"cloud env", dbDefaultsCloudEnv, 30*time.Second + DBIdleTxTimeoutMargin},
		{"cloud env follows PAD_REQUEST_TIMEOUT", func(t *testing.T) {
			dbDefaultsCloudEnv(t)
			t.Setenv("PAD_REQUEST_TIMEOUT", "2m")
		}, 2*time.Minute + DBIdleTxTimeoutMargin},
		{"cloud env, explicit 0s is off", func(t *testing.T) { dbDefaultsCloudEnv(t); t.Setenv("PAD_DB_IDLE_TX_TIMEOUT", "0s") }, 0},
		{"cloud env, explicit 5m", func(t *testing.T) { dbDefaultsCloudEnv(t); t.Setenv("PAD_DB_IDLE_TX_TIMEOUT", "5m") }, 5 * time.Minute},
		{"cloud file", func(t *testing.T) { writeConfigFile(t, dbDefaultsCloudFile+`}`) }, 30*time.Second + DBIdleTxTimeoutMargin},
		{"cloud file follows its request timeout", func(t *testing.T) {
			writeConfigFile(t, dbDefaultsCloudFile+`,"Runtime":{"RequestTimeout":"40s"}}`)
		}, 40*time.Second + DBIdleTxTimeoutMargin},
		{`cloud file, "" is unset`, func(t *testing.T) {
			writeConfigFile(t, dbDefaultsCloudFile+`,"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test","DBIdleTxTimeout":""}}`)
		}, 30*time.Second + DBIdleTxTimeoutMargin},
		{`cloud file, explicit "0s" is off`, func(t *testing.T) {
			writeConfigFile(t, dbDefaultsCloudFile+`,"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test","DBIdleTxTimeout":"0s"}}`)
		}, 0},
		{"cloud file, explicit 3m", func(t *testing.T) {
			writeConfigFile(t, dbDefaultsCloudFile+`,"Storage":{"Backend":"database","DatabaseURL":"postgres://localhost/test","DBIdleTxTimeout":"3m"}}`)
		}, 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.EffectiveDBIdleTxTimeout(); got != tc.want {
				t.Errorf("EffectiveDBIdleTxTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidate_DBIdleTxTimeoutOutlastsTheRequest: an explicit idle-transaction timeout must be longer than the request
// timeout, or it would cut a request transaction whose handler is still inside its deadline (the transaction idles
// while its handler calls out or computes between statements). 0 (off) is always accepted, a negative value never,
// and the derived default always fits, whatever the request timeout.
func TestValidate_DBIdleTxTimeoutOutlastsTheRequest(t *testing.T) {
	explicit := func(d time.Duration) OptionalDuration { return ExplicitDuration(d) }
	for _, tc := range []struct {
		name    string
		request time.Duration
		idle    OptionalDuration
		wantErr bool
	}{
		{"derived, default request timeout", 30 * time.Second, OptionalDuration{}, false},
		{"derived, an hour's request timeout", time.Hour, OptionalDuration{}, false},
		{"off", time.Hour, explicit(0), false},
		{"longer", 30 * time.Second, explicit(31 * time.Second), false},
		{"equal", 30 * time.Second, explicit(30 * time.Second), true},
		{"shorter", time.Minute, explicit(10 * time.Second), true},
		{"negative", 30 * time.Second, explicit(-time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Runtime.RequestTimeout = Duration(tc.request)
			cfg.Storage.DBIdleTxTimeout = tc.idle
			err := Validate(cfg)
			switch {
			case tc.wantErr && (err == nil || !strings.Contains(err.Error(), "PAD_DB_IDLE_TX_TIMEOUT")):
				t.Errorf("Validate = %v, want a refusal naming PAD_DB_IDLE_TX_TIMEOUT", err)
			case !tc.wantErr && err != nil:
				t.Errorf("Validate = %v, want none", err)
			}
		})
	}
	t.Run("the refusal names the request timeout", func(t *testing.T) {
		dbDefaultsCloudEnv(t)
		t.Setenv("PAD_REQUEST_TIMEOUT", "45s")
		t.Setenv("PAD_DB_IDLE_TX_TIMEOUT", "40s")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAD_DB_IDLE_TX_TIMEOUT") || !strings.Contains(err.Error(), "PAD_REQUEST_TIMEOUT") {
			t.Errorf("Load = %v, want a refusal naming both variables", err)
		}
	})
	t.Run("a bad value names the variable", func(t *testing.T) {
		dbDefaultsCloudEnv(t)
		t.Setenv("PAD_DB_IDLE_TX_TIMEOUT", "soon")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAD_DB_IDLE_TX_TIMEOUT") {
			t.Errorf("Load = %v, want a refusal naming PAD_DB_IDLE_TX_TIMEOUT", err)
		}
	})
}

// TestLoad_DBAllowBypassRLS: PAD_DB_ALLOW_BYPASSRLS, the cloud profile's escape hatch from refusing a database role
// that bypasses row-level security. Off unless set; a value that is not a boolean refuses to boot.
func TestLoad_DBAllowBypassRLS(t *testing.T) {
	dbDefaultsCloudEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.DBAllowBypassRLS {
		t.Error("DBAllowBypassRLS is on by default")
	}
	t.Setenv("PAD_DB_ALLOW_BYPASSRLS", "true")
	if cfg, err = Load(); err != nil || !cfg.Storage.DBAllowBypassRLS {
		t.Errorf("PAD_DB_ALLOW_BYPASSRLS=true: Load = %v, DBAllowBypassRLS = %v; want it on", err, cfg != nil && cfg.Storage.DBAllowBypassRLS)
	}
	t.Setenv("PAD_DB_ALLOW_BYPASSRLS", "sure")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAD_DB_ALLOW_BYPASSRLS") {
		t.Errorf("PAD_DB_ALLOW_BYPASSRLS=sure: Load = %v, want a refusal naming it", err)
	}
}

// TestLoad_MultiReplicaCloudRedisNeedsPassword: the revocation/room/SSE fanout
// channels are published to Redis without a MAC, so a multi-replica cloud
// backplane reachable without a password must refuse to boot. Local mode (the
// dev replicas stack, on a private compose network) stays allowed.
func TestLoad_MultiReplicaCloudRedisNeedsPassword(t *testing.T) {
	cloud := func(t *testing.T, redisURL string) error {
		t.Helper()
		clearLoadEnv(t)
		t.Setenv("PAD_MODE", "cloud")
		t.Setenv("PAD_STORAGE", "database")
		t.Setenv("PAD_DATABASE_URL", "postgres://localhost/test")
		t.Setenv("PAD_AUTH_ENABLED", "true")
		t.Setenv("PAD_AUTH_SECRET", "a-long-random-cloud-signing-secret-0123456789")
		t.Setenv("PAD_BEHIND_PROXY", "true")
		t.Setenv("PAD_MAX_REPLICAS", "2")
		if redisURL != "" {
			t.Setenv("PAD_REDIS_URL", redisURL)
		}
		_, err := Load()
		return err
	}

	if err := cloud(t, "redis://cache.internal:6379/0"); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Errorf("passwordless redis: err = %v, want the no-password boot refusal", err)
	}
	if err := cloud(t, "redis://:sekret@cache.internal:6379/0"); err != nil {
		t.Errorf("redis with a password must boot: %v", err)
	}

	// Local mode is deliberately exempt (dev stacks run on a compose network).
	clearLoadEnv(t)
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_MAX_REPLICAS", "2")
	t.Setenv("PAD_REDIS_URL", "redis://redis:6379")
	if _, err := Load(); err != nil {
		t.Errorf("local-mode dev stack must not need a redis password: %v", err)
	}
}
