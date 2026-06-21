// Package scanner runs the continuous-governance loop: on an interval it
// re-analyzes every stored flow, compares the result against the flow's accepted
// baseline (drift) and its last recorded health (regression), and dispatches an
// alert through the notify package for anything new.
//
// It is read-only with respect to flow data and best-effort: a flow that fails
// to parse or analyze is logged and skipped, and alerts are de-duplicated per
// (flow, event-type) so an unchanged regression is reported once, not every tick.
// Cloud-only — it needs a storage backend to enumerate flows and baselines.
package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"pad-core/analyzer"
	"pad-core/logger"
	"pad-core/models"

	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// AnalyzeFunc analyzes a parsed flow document. In production this is wired to
// AnalysisService.AnalyzeFlow (which respects rule config); tests inject a stub.
type AnalyzeFunc func(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error)

// Scanner periodically scans stored flows for governance regressions.
type Scanner struct {
	backend  storageif.StorageBackend
	analyze  AnalyzeFunc
	notifier *notify.Dispatcher
	interval time.Duration

	mu      sync.Mutex
	lastSig map[string]string // (flowID|eventType) -> last alert signature, for dedup

	stop     chan struct{}
	stopOnce sync.Once
}

// New creates a Scanner. interval <= 0 disables periodic scanning (Start is a
// no-op), but ScanOnce can still be invoked directly (e.g. from a test or an
// admin "scan now" action).
func New(backend storageif.StorageBackend, analyze AnalyzeFunc, notifier *notify.Dispatcher, interval time.Duration) *Scanner {
	return &Scanner{
		backend:  backend,
		analyze:  analyze,
		notifier: notifier,
		interval: interval,
		lastSig:  make(map[string]string),
		stop:     make(chan struct{}),
	}
}

// enabled reports whether periodic scanning should run: it needs a backend, an
// analyzer, at least one notification channel, and a positive interval.
func (s *Scanner) enabled() bool {
	return s != nil && s.backend != nil && s.analyze != nil && s.notifier.Enabled() && s.interval > 0
}

// Start launches the periodic scan loop in a background goroutine. It returns
// immediately and is a no-op when scanning is disabled. The loop runs until Stop.
func (s *Scanner) Start() {
	if !s.enabled() {
		logger.Info("scanner: disabled (needs backend, notifier, and a positive interval)")
		return
	}
	logger.Info("scanner: starting", "interval", s.interval.String())
	go s.loop()
}

func (s *Scanner) loop() {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("scanner: loop panicked", "recover", r)
		}
	}()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.interval)
			s.ScanOnce(ctx)
			cancel()
		}
	}
}

// Stop halts the scan loop. Safe to call multiple times.
func (s *Scanner) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

// ScanOnce scans every stored flow once. Errors are logged, never returned, so a
// single bad flow doesn't abort the sweep.
func (s *Scanner) ScanOnce(ctx context.Context) {
	if s.backend == nil || s.analyze == nil {
		return
	}
	flows, err := s.backend.ListFlows(ctx, storageif.FlowFilter{AllFlows: true})
	if err != nil {
		logger.Warn("scanner: list flows failed", "err", err)
		return
	}
	for _, f := range flows {
		s.scanFlow(ctx, f)
	}
}

func (s *Scanner) scanFlow(ctx context.Context, lf *storageif.FlowDocument) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("scanner: flow scan panicked", "flowId", lf.ID, "recover", r)
		}
	}()

	doc := parseStored(lf.Content)
	if doc == nil {
		return
	}
	doc.ID = lf.ID
	doc.OwnerID = lf.OwnerID
	doc.OrganizationID = lf.OrganizationID

	report, err := s.analyze(ctx, doc)
	if err != nil || report == nil {
		if err != nil {
			logger.Warn("scanner: analyze failed", "flowId", lf.ID, "err", err)
		}
		return
	}

	for _, ev := range s.events(ctx, lf, report) {
		if s.shouldAlert(lf.ID, ev) {
			s.notifier.Dispatch(ctx, ev)
		}
	}
}

// events derives the governance alerts for one freshly-analyzed flow: drift
// (new findings since the accepted baseline) and health regression (current
// health below the last recorded health).
func (s *Scanner) events(ctx context.Context, lf *storageif.FlowDocument, report *models.AnalysisReport) []notify.Event {
	var events []notify.Event
	name := lf.Name

	// Drift vs. accepted baseline. Only meaningful once a baseline exists;
	// otherwise every finding is "new" and would alert on the first scan.
	bl, err := s.backend.GetFlowBaseline(ctx, lf.ID)
	if err != nil {
		logger.Warn("scanner: load baseline failed", "flowId", lf.ID, "err", err)
	} else if bl != nil {
		keys := bl.Keys
		if keys == nil {
			keys = []string{}
		}
		drift := analyzer.ComputeDrift(lf.ID, report, keys)
		if len(drift.New) > 0 {
			events = append(events, notify.Event{
				Type:        notify.EventDrift,
				FlowID:      lf.ID,
				FlowName:    name,
				Title:       fmt.Sprintf("New findings in %s", flowLabel(lf)),
				Message:     fmt.Sprintf("%d new finding(s) since baseline (%d error, %d warning).", len(drift.New), drift.NewErrors, drift.NewWarnings),
				NewErrors:   drift.NewErrors,
				NewWarnings: drift.NewWarnings,
			})
		}
	}

	// Health regression vs. last recorded snapshot. Requires metrics (a parsed,
	// analyzable flow) and a prior snapshot to compare against.
	if report.Metrics != nil {
		prior, err := s.backend.LoadFlowHealth(ctx, lf.ID)
		if err != nil {
			logger.Warn("scanner: load health failed", "flowId", lf.ID, "err", err)
		} else if prior != nil && report.Metrics.HealthScore < prior.HealthScore {
			events = append(events, notify.Event{
				Type:        notify.EventHealthRegression,
				FlowID:      lf.ID,
				FlowName:    name,
				Title:       fmt.Sprintf("Health regressed in %s", flowLabel(lf)),
				Message:     fmt.Sprintf("Health score dropped from %d to %d.", prior.HealthScore, report.Metrics.HealthScore),
				HealthScore: report.Metrics.HealthScore,
				PrevHealth:  prior.HealthScore,
			})
		}
	}
	return events
}

// shouldAlert de-duplicates alerts: it returns true only when the event's
// signature differs from the last one sent for the same (flow, type), so a
// persistent regression is reported once rather than on every tick.
func (s *Scanner) shouldAlert(flowID string, ev notify.Event) bool {
	key := flowID + "|" + string(ev.Type)
	sig := signature(ev)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSig[key] == sig {
		return false
	}
	s.lastSig[key] = sig
	return true
}

func signature(ev notify.Event) string {
	switch ev.Type {
	case notify.EventDrift:
		return fmt.Sprintf("e%dw%d", ev.NewErrors, ev.NewWarnings)
	case notify.EventHealthRegression:
		return fmt.Sprintf("h%d<%d", ev.HealthScore, ev.PrevHealth)
	default:
		return ev.Message
	}
}

func parseStored(content json.RawMessage) *models.FlowDocument {
	if len(content) == 0 {
		return nil
	}
	var doc models.FlowDocument
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil
	}
	doc.RebuildIndexes()
	return &doc
}

func flowLabel(lf *storageif.FlowDocument) string {
	if lf.Name != "" {
		return lf.Name
	}
	return lf.ID
}
