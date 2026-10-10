package config

import (
	"reflect"
	"strings"
	"testing"

	"pad-analyzer/internal/platform/secret"
)

func TestDeploymentChannels(t *testing.T) {
	g := GovernanceConfig{
		NotifyWebhookURL: secret.New("https://hooks.example.com/w"), NotifyWebhookSecret: secret.New("ws"),
		NotifyTeamsURL:         secret.New("https://acme.webhook.office.com/t"),
		NotifyTeamsWorkflowURL: secret.New("https://prod-00.westeurope.logic.azure.com:443/workflows/w"),
		NotifySlackURL:         secret.New("https://hooks.slack.com/s"),
		NotifyEmailTo:          "ops@example.com",
		// Three of Jira's four: left out.
		NotifyJiraBaseURL: "https://acme.atlassian.net", NotifyJiraEmail: "bot@example.com", NotifyJiraProject: "PAD",
	}
	want := []DeploymentChannel{
		{Kind: "webhook", Values: map[string]string{"url": "https://hooks.example.com/w", "secret": "ws"}},
		{Kind: "teams", Values: map[string]string{"url": "https://acme.webhook.office.com/t"}},
		{Kind: "teams-workflow", Values: map[string]string{"url": "https://prod-00.westeurope.logic.azure.com:443/workflows/w"}},
		{Kind: "slack", Values: map[string]string{"url": "https://hooks.slack.com/s"}},
		{Kind: "email", Values: map[string]string{"to": "ops@example.com"}},
	}
	if got := g.DeploymentChannels(); !reflect.DeepEqual(got, want) {
		t.Errorf("DeploymentChannels =\n%+v\nwant\n%+v", got, want)
	}
	g.NotifyJiraAPIToken = secret.New("tok")
	got := g.DeploymentChannels()
	if last := got[len(got)-1]; last.Kind != "jira" || last.Values["token"] != "tok" || last.Values["project"] != "PAD" {
		t.Errorf("jira channel = %+v", last)
	}
	if got := (GovernanceConfig{}).DeploymentChannels(); len(got) != 0 {
		t.Errorf("unconfigured: %+v", got)
	}
}

// TestResolveLegacyAliases: PAD_WEBHOOK_URL/_SECRET configure the Slack channel only when PAD_NOTIFY_SLACK_URL does
// not.
func TestResolveLegacyAliases(t *testing.T) {
	t.Setenv("PAD_WEBHOOK_URL", "https://hooks.slack.com/legacy")
	t.Setenv("PAD_WEBHOOK_SECRET", "legacy-secret")
	cfg := Default()
	if err := ResolveLegacyAliases(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.NotifySlackURL.Reveal() != "https://hooks.slack.com/legacy" || cfg.Governance.NotifySlackSecret.Reveal() != "legacy-secret" {
		t.Errorf("alias not applied: %q %q", cfg.Governance.NotifySlackURL.Reveal(), cfg.Governance.NotifySlackSecret.Reveal())
	}
	cfg = Default()
	cfg.Governance.NotifySlackURL = secret.New("https://hooks.slack.com/new")
	if err := ResolveLegacyAliases(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Governance.NotifySlackURL.Reveal() != "https://hooks.slack.com/new" || cfg.Governance.NotifySlackSecret.IsSet() {
		t.Errorf("alias overrode PAD_NOTIFY_SLACK_URL: %q %q", cfg.Governance.NotifySlackURL.Reveal(), cfg.Governance.NotifySlackSecret.Reveal())
	}

	// A redacted dump pasted back as the alias variables is refused, as the PAD_NOTIFY_* rows refuse it.
	t.Setenv("PAD_WEBHOOK_SECRET", secret.Redacted)
	if err := ResolveLegacyAliases(Default()); err == nil || !strings.Contains(err.Error(), "PAD_WEBHOOK_SECRET") {
		t.Errorf("err = %v, want a refusal naming PAD_WEBHOOK_SECRET", err)
	}
}
