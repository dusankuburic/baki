package config

import "testing"

// PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP moved here from the webhook handler with its meaning unchanged: exactly "1"
// turns it on, and any other value is ignored rather than refusing to boot.
func TestApplyEnvVars_CIWebhookAllowNoTimestamp(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "": false, "0": false, "true": false, "yes": false} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv("PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP", value)
			cfg := Default()
			if err := applyEnvVars(cfg); err != nil {
				t.Fatalf("applyEnvVars: %v", err)
			}
			if got := cfg.Governance.InboundWebhookAllowNoTimestamp; got != want {
				t.Errorf("InboundWebhookAllowNoTimestamp = %v, want %v", got, want)
			}
		})
	}
}
