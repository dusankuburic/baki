package di

import (
	"context"
	"slices"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// TestProvideChannelService_HonoursTheNotifyAllowList: PAD_NOTIFY_ALLOWED_HOSTS reaches the organisation channels'
// service, so a save to a host off the list is refused on url, and PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE decides whether
// the channel form is shown the list.
func TestProvideChannelService_HonoursTheNotifyAllowList(t *testing.T) {
	cfg := config.Default()
	cfg.Governance.NotifyAllowedHosts = []string{"hooks.slack.com"}
	svc := ProvideChannelService(testutil.NewFakeBackend(), cfg, nil)
	_, err := svc.Save(context.Background(), "o", service.ChannelSave{Kind: "webhook", URL: "https://203.0.113.10/hook"})
	if ae, ok := apperr.As(err); !ok || ae.Code != "INVALID_CHANNEL" || ae.Details["field"] != "url" {
		t.Errorf("Save off the list = %v, want 400 INVALID_CHANNEL on url", err)
	}
	if c := svc.Choices(); !c.HostsRestricted || c.AllowedHosts != nil {
		t.Errorf("choices = %+v, want the list applied and not shown", c)
	}
	cfg.Governance.NotifyAllowedHostsVisible = true
	if c := ProvideChannelService(testutil.NewFakeBackend(), cfg, nil).Choices(); !slices.Equal(c.AllowedHosts, []string{"hooks.slack.com"}) {
		t.Errorf("choices with the list visible = %+v", c)
	}
}
