package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"pad-analyzer/internal/platform/secret"
)

// secretEnvRows is every environment variable whose value is a credential, or carries one (a DSN's password, a
// webhook URL's token), with the config field it fills. TestConfig_SecretFieldsAreSecretValues keeps it complete.
var secretEnvRows = []struct{ env, field string }{
	{"PAD_METRICS_TOKEN", "Server.MetricsToken"},
	{"PAD_DATABASE_URL", "Storage.DatabaseURL"},
	{"PAD_AZURE_BLOB_CONNECTION_STRING", "Storage.AzureBlobConnectionString"},
	{"PAD_AUTH_SECRET", "Auth.Secret"},
	{"PAD_ENCRYPTION_KEY", "Auth.EncryptionKey"},
	{"PAD_ENCRYPTION_KEY_PREVIOUS", "Auth.EncryptionKeyPrevious"},
	{"PAD_SSO_CLIENT_SECRET", "Auth.SSO.ClientSecret"},
	{"PAD_NOTIFY_WEBHOOK_URL", "Governance.NotifyWebhookURL"},
	{"PAD_NOTIFY_WEBHOOK_SECRET", "Governance.NotifyWebhookSecret"},
	{"PAD_NOTIFY_TEAMS_URL", "Governance.NotifyTeamsURL"},
	{"PAD_NOTIFY_TEAMS_WORKFLOW_URL", "Governance.NotifyTeamsWorkflowURL"},
	{"PAD_NOTIFY_SLACK_URL", "Governance.NotifySlackURL"},
	{"PAD_NOTIFY_SLACK_SECRET", "Governance.NotifySlackSecret"},
	{"PAD_CI_WEBHOOK_SECRET", "Governance.InboundWebhookSecret"},
	{"PAD_NOTIFY_JIRA_API_TOKEN", "Governance.NotifyJiraAPIToken"},
	{"PAD_SMTP_PASSWORD", "Email.Password"},
	{"PAD_REDIS_URL", "Redis.URL"},
}

// sentinelFor is the value a test plants in a secret field: unique per field, and never a substring of a redaction
// marker, a field name or a default.
func sentinelFor(field string) string {
	return "S3NT1NEL-" + strings.ReplaceAll(field, ".", "-") + "-hunter2"
}

// setSecretEnv fills every secret variable with its sentinel and the rest of the environment with what Validate
// needs beside them (local mode, so no strength rule stands in the way of a short sentinel).
func setSecretEnv(t *testing.T) map[string]string {
	t.Helper()
	clearLoadEnv(t)
	t.Setenv("PAD_MODE", "local")
	t.Setenv("PAD_AUTH_ENABLED", "true")
	t.Setenv("PAD_SSO_ISSUER", "https://idp.example.com")
	t.Setenv("PAD_SSO_CLIENT_ID", "client")
	t.Setenv("PAD_SSO_REDIRECT_URL", "https://app.example.com/cb")
	t.Setenv("PAD_AZURE_STORAGE_CONTAINER", "flows")
	t.Setenv("PAD_WEBHOOK_URL", "")
	t.Setenv("PAD_WEBHOOK_SECRET", "")
	sentinels := map[string]string{}
	for _, r := range secretEnvRows {
		s := sentinelFor(r.field)
		if r.env == "PAD_ENCRYPTION_KEY_PREVIOUS" {
			s = sentinelFor(r.field+"-1") + ", " + sentinelFor(r.field+"-2")
			sentinels[r.field+"-1"] = sentinelFor(r.field + "-1")
			sentinels[r.field+"-2"] = sentinelFor(r.field + "-2")
		} else {
			sentinels[r.field] = s
		}
		t.Setenv(r.env, s)
	}
	return sentinels
}

// assertNoSentinel fails when any sentinel appears in text.
func assertNoSentinel(t *testing.T, what, text string, sentinels map[string]string) {
	t.Helper()
	for field, s := range sentinels {
		if strings.Contains(text, s) {
			t.Errorf("%s shows %s:\n%s", what, field, text)
		}
	}
}

// renderings is every way a configuration could end up in a log, an error or a response, with its text.
func renderings(t *testing.T, cfg *Config) map[string]string {
	t.Helper()
	out := map[string]string{
		"%v":                              fmt.Sprintf("%v", cfg),
		"%+v":                             fmt.Sprintf("%+v", cfg),
		"%#v":                             fmt.Sprintf("%#v", cfg),
		"%v of the value":                 fmt.Sprintf("%v", *cfg),
		"%+v of the value":                fmt.Sprintf("%+v", *cfg),
		"%#v of the value":                fmt.Sprintf("%#v", *cfg),
		"%+v of a section":                fmt.Sprintf("%+v %+v %+v %+v %+v %+v", cfg.Auth, cfg.Storage, cfg.Governance, cfg.Email, cfg.Redis, cfg.Server),
		"%q of a field":                   fmt.Sprintf("%q %q", cfg.Auth.Secret, cfg.Auth.EncryptionKeyPrevious),
		"error wrapping %v":               fmt.Errorf("boot: %v", cfg).Error(),
		"error wrapping %+v of a section": fmt.Errorf("boot: %+v", cfg.Auth).Error(),
		"errors.Join":                     errors.Join(fmt.Errorf("a: %+v", cfg.Redis), fmt.Errorf("b: %v", cfg.Storage)).Error(),
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out["json.Marshal"] = string(b)
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("boot", "config", cfg, "auth", cfg.Auth, "dsn", cfg.Storage.DatabaseURL, "previous", cfg.Auth.EncryptionKeyPrevious)
	out["slog JSON"] = buf.String()
	buf.Reset()
	slog.New(slog.NewTextHandler(&buf, nil)).Info("boot", "config", cfg, "sso", cfg.Auth.SSO)
	out["slog text"] = buf.String()
	return out
}

// TestConfig_FormatAndJSONNeverShowSecrets: a configuration printed by accident (a %+v in a log line, json.Marshal
// of the struct, a wrapped error, an slog attribute) never shows a credential. Written against string fields, it
// fails for every rendering; secret.Value fields pass.
func TestConfig_FormatAndJSONNeverShowSecrets(t *testing.T) {
	t.Run("from the environment", func(t *testing.T) {
		sentinels := setSecretEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		for what, text := range renderings(t, cfg) {
			assertNoSentinel(t, what, text, sentinels)
		}
	})
	t.Run("from a config file", func(t *testing.T) {
		sentinels := map[string]string{}
		for _, r := range secretEnvRows {
			if r.env != "PAD_ENCRYPTION_KEY_PREVIOUS" {
				sentinels[r.field] = sentinelFor(r.field)
			}
		}
		sentinels["prev-1"], sentinels["prev-2"] = sentinelFor("prev-1"), sentinelFor("prev-2")
		body := fmt.Sprintf(`{
			"Mode": "local",
			"Server": {"MetricsToken": %q},
			"Storage": {"DatabaseURL": %q, "AzureBlobConnectionString": %q, "AzureStorageContainer": "flows"},
			"Auth": {"Enabled": true, "Secret": %q, "EncryptionKey": %q, "EncryptionKeyPrevious": [%q, %q],
				"SSO": {"IssuerURL": "https://idp.example.com", "ClientID": "c", "RedirectURL": "https://app.example.com/cb", "ClientSecret": %q}},
			"Governance": {"NotifyWebhookURL": %q, "NotifyWebhookSecret": %q, "NotifyTeamsURL": %q, "NotifyTeamsWorkflowURL": %q,
				"NotifySlackURL": %q, "NotifySlackSecret": %q, "InboundWebhookSecret": %q, "NotifyJiraAPIToken": %q},
			"Email": {"Password": %q},
			"Redis": {"URL": %q}
		}`,
			sentinels["Server.MetricsToken"],
			sentinels["Storage.DatabaseURL"], sentinels["Storage.AzureBlobConnectionString"],
			sentinels["Auth.Secret"], sentinels["Auth.EncryptionKey"], sentinels["prev-1"], sentinels["prev-2"],
			sentinels["Auth.SSO.ClientSecret"],
			sentinels["Governance.NotifyWebhookURL"], sentinels["Governance.NotifyWebhookSecret"], sentinels["Governance.NotifyTeamsURL"], sentinels["Governance.NotifyTeamsWorkflowURL"],
			sentinels["Governance.NotifySlackURL"], sentinels["Governance.NotifySlackSecret"], sentinels["Governance.InboundWebhookSecret"], sentinels["Governance.NotifyJiraAPIToken"],
			sentinels["Email.Password"],
			sentinels["Redis.URL"])
		writeConfigFile(t, body)
		t.Setenv("PAD_WEBHOOK_URL", "")
		t.Setenv("PAD_WEBHOOK_SECRET", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		for what, text := range renderings(t, cfg) {
			assertNoSentinel(t, what, text, sentinels)
		}
		// The file's values did arrive: the redaction is of the printing, not of the loading.
		if got := cfg.Storage.DatabaseURL.Reveal(); got != sentinels["Storage.DatabaseURL"] {
			t.Errorf("DatabaseURL = %q, want the file's value", got)
		}
		if got := secret.RevealAll(cfg.Auth.EncryptionKeyPrevious); len(got) != 2 || got[0] != sentinels["prev-1"] || got[1] != sentinels["prev-2"] {
			t.Errorf("EncryptionKeyPrevious = %q, want the file's two keys", got)
		}
	})
}

// TestConfig_RedactedDumpIsRefused: a configuration printed with json.Marshal shows "[redacted]" for every set
// secret; saved as a config file or pasted into the environment, that marker must not become the secret.
func TestConfig_RedactedDumpIsRefused(t *testing.T) {
	t.Run("config file", func(t *testing.T) {
		writeConfigFile(t, `{"Mode": "local", "Auth": {"Secret": "`+secret.Redacted+`"}}`)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "redaction marker") {
			t.Errorf("Load = %v, want a refusal of the redaction marker", err)
		}
	})
	t.Run("environment", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_MODE", "local")
		t.Setenv("PAD_DATABASE_URL", secret.Redacted)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "PAD_DATABASE_URL") || !strings.Contains(err.Error(), "redaction marker") {
			t.Errorf("Load = %v, want a refusal naming PAD_DATABASE_URL", err)
		}
	})
	t.Run("previous keys", func(t *testing.T) {
		clearLoadEnv(t)
		t.Setenv("PAD_MODE", "local")
		t.Setenv("PAD_ENCRYPTION_KEY", "a-local-key")
		t.Setenv("PAD_ENCRYPTION_KEY_PREVIOUS", "an-old-key, "+secret.Redacted)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "PAD_ENCRYPTION_KEY_PREVIOUS entry 2") {
			t.Errorf("Load = %v, want a refusal naming entry 2", err)
		}
	})
}

// TestConfig_SecretFieldsAreSecretValues: every field secretEnvRows names is a secret.Value (or a slice of them),
// and every secret.Value in the configuration is in secretEnvRows, so the proof test above covers a field added
// later.
func TestConfig_SecretFieldsAreSecretValues(t *testing.T) {
	secretType := reflect.TypeOf(secret.Value{})
	found := map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(rt reflect.Type, prefix string) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			name := prefix + f.Name
			switch {
			case f.Type == secretType, f.Type.Kind() == reflect.Slice && f.Type.Elem() == secretType:
				found[name] = true
			case f.Type.Kind() == reflect.Struct:
				walk(f.Type, name+".")
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	for _, r := range secretEnvRows {
		if !found[r.field] {
			t.Errorf("%s (%s) is not a secret.Value", r.field, r.env)
		}
		delete(found, r.field)
	}
	for name := range found {
		t.Errorf("%s is a secret.Value that secretEnvRows does not list: add it, with its PAD_* variable", name)
	}
}

// TestConfig_SecretEphemeralCannotComeFromAFile: Auth.SecretEphemeral says the process minted its own auth secret,
// so the keystore must not key stored data with it. A config file could set it beside a real secret and make the
// keystore refuse that secret; the field is not a setting.
func TestConfig_SecretEphemeralCannotComeFromAFile(t *testing.T) {
	writeConfigFile(t, `{"Mode": "local", "Auth": {"Enabled": true, "Secret": "an-operator-secret", "SecretEphemeral": true}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.SecretEphemeral {
		t.Error("a config file set Auth.SecretEphemeral; it must only be set by ResolveRuntimeSecrets")
	}
	if got := cfg.Auth.Secret.Reveal(); got != "an-operator-secret" {
		t.Errorf("Auth.Secret = %q, want the file's", got)
	}
}
