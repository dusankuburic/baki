package service

import (
	"context"
	"fmt"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/notify"
	"pad-core/models"
)

// WebhookNotifier posts analysis summaries to chat channels (Slack/Discord/
// Mattermost/Teams-via-Slack-connector) and the generic webhook/Teams channels — a
// thin facade over notify.Dispatcher (no duplicated HTTP/retry/HMAC delivery logic)
// translating an analysis report into a notify.Event. The dispatcher is nil-safe:
// with no channel configured, Enabled() is false and NotifyAnalysis is a no-op.
type WebhookNotifier struct {
	dispatcher *notify.Dispatcher
	// deliveries caps the dispatches in flight: one runs per analysis and
	// walks every channel in turn (each bounded by the dispatcher's timeout),
	// so a slow channel used to pile up one goroutine per analyze request.
	deliveries *lifecycle.Bounded
}

// maxConcurrentAnalysisDispatches matches the scanner's cap on concurrent
// governance-alert deliveries (maxConcurrentDeliveries).
const maxConcurrentAnalysisDispatches = 32

// NewWebhookNotifier wraps a notify.Dispatcher. The dispatcher is provided by
// internal/app's provideNotifier and already knows about every configured channel.
func NewWebhookNotifier(dispatcher *notify.Dispatcher) *WebhookNotifier {
	return &WebhookNotifier{
		dispatcher: dispatcher,
		deliveries: lifecycle.NewBounded("analysis_webhook", maxConcurrentAnalysisDispatches),
	}
}

// Drain waits for in-flight dispatches (see lifecycle.Bounded.Drain) and
// stops new ones.
func (w *WebhookNotifier) Drain(ctx context.Context) error {
	return w.deliveries.Drain(ctx)
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
		// No stored row raised it: a random id, and the time of the analysis.
		ID:       notify.NewEventID(),
		At:       time.Now().UTC(),
		Type:     notify.EventAnalysisComplete,
		FlowName: flowName,
		Title:    fmt.Sprintf("PAD Flow Analysis: %s (%s)", flowName, severity),
		Message: fmt.Sprintf("%d error(s), %d warning(s), %d info finding(s)",
			report.Stats.Errors, report.Stats.Warnings, report.Stats.Info),
		Analysis: summary,
	}

	// Dispatch on a detached context — the analysis HTTP request may return (and
	// its ctx cancel) before delivery completes. The dispatcher applies its own
	// per-channel timeout; the runner caps how many dispatches run at once
	// (past the cap this summary is dropped and counted — it is best-effort).
	w.deliveries.Go(func() { w.dispatcher.Dispatch(context.Background(), ev) })
}
