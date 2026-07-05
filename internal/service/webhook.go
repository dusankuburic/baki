package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"pad-core/models"
)

// WebhookNotifier posts analysis summaries to a Slack-compatible incoming
// webhook URL when configured via the PAD_WEBHOOK_URL env var. If the env var
// is unset, all methods are no-ops. The payload format matches Slack's incoming
// webhook API, which is also compatible with Discord, Mattermost, and Microsoft
// Teams (via the Slack-compatible webhook connector).
type WebhookNotifier struct {
	url    string
	client *http.Client
}

// NewWebhookNotifier reads the webhook URL from PAD_WEBHOOK_URL. Returns a
// no-op notifier if the env var is unset.
func NewWebhookNotifier() *WebhookNotifier {
	url := strings.TrimSpace(os.Getenv("PAD_WEBHOOK_URL"))
	if url == "" {
		return &WebhookNotifier{}
	}
	return &WebhookNotifier{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (w *WebhookNotifier) Enabled() bool { return w.url != "" }

// NotifyAnalysis posts a summary of the analysis report to the webhook. It's
// non-blocking (runs in a goroutine) and best-effort (failures are silently
// dropped — a notification failure must never break analysis).
func (w *WebhookNotifier) NotifyAnalysis(flowName string, report *models.AnalysisReport) {
	if !w.Enabled() || report == nil {
		return
	}

	color := "good"
	if report.Stats.Errors > 0 {
		color = "danger"
	} else if report.Stats.Warnings > 0 {
		color = "warning"
	}

	healthStr := "n/a"
	if report.Metrics != nil {
		healthStr = fmt.Sprintf("%d/100", report.Metrics.HealthScore)
	}

	payload := map[string]interface{}{
		"text": fmt.Sprintf("PAD Flow Analysis: *%s*", flowName),
		"attachments": []map[string]interface{}{{
			"color": color,
			"fields": []map[string]interface{}{
				{"title": "Errors", "value": fmt.Sprintf("%d", report.Stats.Errors), "short": true},
				{"title": "Warnings", "value": fmt.Sprintf("%d", report.Stats.Warnings), "short": true},
				{"title": "Info", "value": fmt.Sprintf("%d", report.Stats.Info), "short": true},
				{"title": "Health", "value": healthStr, "short": true},
			},
			"footer": "Baki PAD Flow Analyzer",
			"ts":     time.Now().Unix(),
		}},
	}

	go w.post(payload)
}

func (w *WebhookNotifier) post(payload map[string]interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
