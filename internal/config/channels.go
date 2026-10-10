package config

import (
	"fmt"
	"os"

	"pad-analyzer/internal/platform/secret"
)

// DeploymentChannel is one notification channel the operator configures in the environment: a kind of the notify
// registry (notify.Kind) and its field values, revealed: this is the boundary where the notify layer takes them
// (it seals the organisations' own in the database and signs with them).
type DeploymentChannel struct {
	Kind   string
	Values map[string]string
}

// deploymentChannels is the table of the deployment's channels: each kind, its fields, the PAD_NOTIFY_* variables
// (envBindings) that set them, and whether it is configured. The order is the delivery order.
var deploymentChannels = []struct {
	kind   string
	values func(g GovernanceConfig) map[string]string
	set    func(g GovernanceConfig) bool
}{
	{"webhook", func(g GovernanceConfig) map[string]string {
		return map[string]string{"url": g.NotifyWebhookURL.Reveal(), "secret": g.NotifyWebhookSecret.Reveal()}
	}, func(g GovernanceConfig) bool { return g.NotifyWebhookURL.IsSet() }},
	{"teams", func(g GovernanceConfig) map[string]string {
		return map[string]string{"url": g.NotifyTeamsURL.Reveal()}
	}, func(g GovernanceConfig) bool { return g.NotifyTeamsURL.IsSet() }},
	{"teams-workflow", func(g GovernanceConfig) map[string]string {
		return map[string]string{"url": g.NotifyTeamsWorkflowURL.Reveal()}
	}, func(g GovernanceConfig) bool { return g.NotifyTeamsWorkflowURL.IsSet() }},
	{"slack", func(g GovernanceConfig) map[string]string {
		return map[string]string{"url": g.NotifySlackURL.Reveal(), "secret": g.NotifySlackSecret.Reveal()}
	}, func(g GovernanceConfig) bool { return g.NotifySlackURL.IsSet() }},
	{"email", func(g GovernanceConfig) map[string]string {
		return map[string]string{"to": g.NotifyEmailTo}
	}, func(g GovernanceConfig) bool { return g.NotifyEmailTo != "" }},
	// Jira needs all four; a partial configuration is left out, as it always was.
	{"jira", func(g GovernanceConfig) map[string]string {
		return map[string]string{"url": g.NotifyJiraBaseURL, "email": g.NotifyJiraEmail, "token": g.NotifyJiraAPIToken.Reveal(), "project": g.NotifyJiraProject}
	}, func(g GovernanceConfig) bool {
		return g.NotifyJiraBaseURL != "" && g.NotifyJiraEmail != "" && g.NotifyJiraAPIToken.IsSet() && g.NotifyJiraProject != ""
	}},
}

// DeploymentChannels lists the configured deployment channels, in delivery order. They receive every tenant's
// governance events (docs/webhooks.md).
func (g GovernanceConfig) DeploymentChannels() []DeploymentChannel {
	var out []DeploymentChannel
	for _, ch := range deploymentChannels {
		if !ch.set(g) {
			continue
		}
		values := map[string]string{}
		for k, v := range ch.values(g) {
			if v != "" {
				values[k] = v
			}
		}
		out = append(out, DeploymentChannel{Kind: ch.kind, Values: values})
	}
	return out
}

// ResolveLegacyAliases applies the deprecated aliases, once at load time and on every load path (a config file, the
// environment, the defaults), as they always applied: PAD_WEBHOOK_URL and PAD_WEBHOOK_SECRET, which sent
// Slack-format payloads, configure the Slack channel when PAD_NOTIFY_SLACK_URL is not set. The error is a
// variable holding the redaction marker instead of a value.
func ResolveLegacyAliases(cfg *Config) error {
	if cfg.Governance.NotifySlackURL.IsSet() {
		return nil
	}
	for _, a := range []struct {
		key string
		dst *secret.Value
	}{
		{"PAD_WEBHOOK_URL", &cfg.Governance.NotifySlackURL},
		{"PAD_WEBHOOK_SECRET", &cfg.Governance.NotifySlackSecret},
	} {
		if err := a.dst.UnmarshalText([]byte(os.Getenv(a.key))); err != nil {
			return fmt.Errorf("config: %s: %w", a.key, err)
		}
	}
	return nil
}
