package service

import (
	"context"
	"fmt"

	"pad-analyzer/internal/notify"
	"pad-core/models"
)

// WebhookNotifier posts analysis summaries to chat channels (Slack/Discord/
// Mattermost/Teams-via-Slack-connector) and the generic webhook/Teams
// channels. It is a thin facade over notify.Dispatcher: rather than duplicating
// the HTTP/retry/HMAC delivery logic, it translates an analysis report into a
// notify.Event and hands it to the shared dispatcher. This consolidates the two
// historically-separate notification paths (governance alerts from the scanner
// and analysis-complete summaries from this handler) onto one delivery stack.
//
// The dispatcher is nil-safe: when no channel is configured, Enabled() is false
// and NotifyAnalysis is a no-op.
type WebhookNotifier struct {
	dispatcher *notify.Dispatcher
}

// NewWebhookNotifier wraps a notify.Dispatcher. The dispatcher is provided by
// main.go's provideNotifier and already knows about every configured channel.
func NewWebhookNotifier(dispatcher *notify.Dispatcher) *WebhookNotifier {
	return &WebhookNotifier{dispatcher: dispatcher}
}

func (w *WebhookNotifier) Enabled() bool { return w.dispatcher != nil && w.dispatcher.Enabled() }

// NotifyAnalysis posts a summary of the analysis report to every configured
// channel. It's non-blocking (dispatches on a goroutine) and best-effort
// (per-channel failures are logged inside the dispatcher, never propagated — a
// notification failure must never break analysis).
func (w *WebhookNotifier) NotifyAnalysis(flowName string, report *models.AnalysisReport) {
	if !w.Enabled() || report == nil {
		return
	}

	summary := &notify.AnalysisSummary{
		Errors:   report.Stats.Errors,
		Warnings: report.Stats.Warnings,
		Info:     report.Stats.Info,
	}
	if report.Metrics != nil {
		summary.HealthScore = report.Metrics.HealthScore
	}

	severity := "clean"
	switch {
	case report.Stats.Errors > 0:
		severity = "errors"
	case report.Stats.Warnings > 0:
		severity = "warnings"
	}

	ev := notify.Event{
		Type:     notify.EventAnalysisComplete,
		FlowName: flowName,
		Title:    fmt.Sprintf("PAD Flow Analysis: %s (%s)", flowName, severity),
		Message: fmt.Sprintf("%d error(s), %d warning(s), %d info finding(s)",
			report.Stats.Errors, report.Stats.Warnings, report.Stats.Info),
		Analysis: summary,
	}

	// Dispatch on a detached context — the analysis HTTP request may return (and
	// its ctx cancel) before delivery completes. The dispatcher applies its own
	// per-channel timeout.
	go w.dispatcher.Dispatch(context.Background(), ev)
}
