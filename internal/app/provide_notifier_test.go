package app

import (
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
)

// TestProvideNotifier_DeploymentChannelsIgnoreTheAllowList: PAD_NOTIFY_ALLOWED_HOSTS restricts what organisation
// admins configure; the deployment's own channels (PAD_NOTIFY_*) are the operator's and are built whatever it lists.
func TestProvideNotifier_DeploymentChannelsIgnoreTheAllowList(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeCloud
	cfg.Auth.Enabled = true
	cfg.Governance.NotifyAllowedHosts = []string{"hooks.slack.com"}
	cfg.Governance.NotifyTeamsWorkflowURL = secret.New("https://203.0.113.10:443/workflows/x/triggers/manual/paths/invoke?sig=abc")
	d, err := provideNotifier(cfg, nil)
	if err != nil {
		t.Fatalf("provideNotifier: %v", err)
	}
	if !d.Enabled() {
		t.Error("the deployment's Teams Workflows channel was left out")
	}
}
