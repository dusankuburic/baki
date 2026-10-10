package config

import (
	"strings"
	"testing"
)

// PAD_NOTIFY_ALLOWED_HOSTS restricts the hosts an organisation's channels may point at. Its entries follow
// PAD_AZURE_FOUNDRY_ALLOWED_HOSTS' rules: a glob over a public suffix or a multi-tenant hosting zone would let any
// tenant admin stand up a receiver under it, so the list would restrict nothing; a malformed entry would never match.
func TestValidate_NotifyAllowedHosts(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		wantErr string
	}{
		{"unset ok", nil, ""},
		{"exact hosts ok", []string{"hooks.slack.com", "contoso.webhook.office.com"}, ""},
		{"own glob ok", []string{"*.hooks.contoso.com"}, ""},
		{"workflows globs ok", []string{"*.logic.azure.com", "*.environment.api.powerplatform.com"}, ""},
		{"blank entry skipped", []string{" ", "hooks.slack.com"}, ""},
		{"rejects url", []string{"https://hooks.slack.com"}, "no scheme, port, path"},
		{"rejects port", []string{"hooks.slack.com:443"}, "no scheme, port, path"},
		{"rejects bare wildcard", []string{"*"}, "wildcard is only allowed"},
		{"rejects tld glob", []string{"*.com"}, "at least two DNS labels"},
		{"rejects single label", []string{"localhost"}, "at least two DNS labels"},
		{"rejects ip literal", []string{"203.0.113.10"}, "IP literals"},
		{"rejects public suffix glob", []string{"*.co.uk"}, "public suffix"},
		{"rejects multi-tenant zone glob", []string{"*.azurewebsites.net"}, "multi-tenant zone"},
		{"exact host in a multi-tenant zone ok", []string{"contoso-hooks.azurewebsites.net"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloudCfg()
			cfg.Governance.NotifyAllowedHosts = tc.entries
			err := Validate(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("entries=%v: unexpected error: %v", tc.entries, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "PAD_NOTIFY_ALLOWED_HOSTS") {
				t.Errorf("entries=%v: error = %v, want one naming the variable and %q", tc.entries, err, tc.wantErr)
			}
		})
	}
}

func TestApplyEnvVars_NotifyAllowedHostsAndTeamsWorkflow(t *testing.T) {
	t.Setenv("PAD_NOTIFY_ALLOWED_HOSTS", " Hooks.Slack.com , *.Logic.Azure.com ")
	t.Setenv("PAD_NOTIFY_TEAMS_WORKFLOW_URL", "https://prod-00.westeurope.logic.azure.com:443/workflows/x")
	cfg := Default()
	if err := applyEnvVars(cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Governance.NotifyAllowedHosts, ","); got != "hooks.slack.com,*.logic.azure.com" {
		t.Errorf("hosts = %q", got)
	}
	if got := cfg.Governance.NotifyTeamsWorkflowURL.Reveal(); got != "https://prod-00.westeurope.logic.azure.com:443/workflows/x" {
		t.Errorf("teams workflow url = %q", got)
	}
}
