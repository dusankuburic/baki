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
	"net/http"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/metrics"
	"pad-core/analyzer"
	"pad-core/logger"
	"pad-core/models"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// perFlowScanTimeout bounds the analysis of a single flow within a tick. The
// tick-level context already bounds the whole sweep; this ensures one slow flow
// (or a provider hang) can't monopolize the tick or delay shutdown — the scan
// moves on to the next flow and, because each fctx derives from the tick ctx,
// a shutdown that cancels the tick is observed before starting further flows.
const perFlowScanTimeout = 30 * time.Second

// AnalyzeFunc analyzes a parsed flow document. In production this is wired to
// AnalysisService.AnalyzeFlow (which respects rule config); tests inject a stub.
type AnalyzeFunc func(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error)

// SSENotifier pushes real-time events to connected frontend clients. The api
// package's EventManager implements it. A nil notifier (the default) disables
// real-time SSE push — alerts are still persisted + dispatched to external
// channels; clients fall back to the periodic unread-count poll. Defined here
// rather than importing service.EventNotifier so the scanner stays decoupled
// from the service layer (same rationale as AnalyzeFunc above).
type SSENotifier interface {
	EmitTo(userID, name string, data any)
}

// Scanner periodically scans stored flows for governance regressions.
type Scanner struct {
	backend  storageif.StorageBackend
	analyze  AnalyzeFunc
	notifier *notify.Dispatcher
	interval time.Duration
	sse      SSENotifier // optional real-time SSE push; nil = disabled

	mu      sync.Mutex
	lastSig map[string]string // (flowID|eventType) -> last alert signature, for dedup

	// orgNotifiers caches the BUILT notifiers per org for the current sweep
	// (R2-3); orgChMu guards it. Caching the notifier rather than the channel row
	// matters: BuildChannelNotifier clones http.DefaultTransport, so building one
	// per event per channel meant a fresh connection pool every time — no keep-
	// alive reuse, and every one of those transports leaving idle connections
	// behind with nothing to call CloseIdleConnections on. See dispatchOrgChannels.
	orgChMu      sync.Mutex
	orgNotifiers map[string][]notify.AlertNotifier

	// notifyPolicy decides whether org-channel deliveries may reach loopback /
	// private addresses. Set from the deployment mode at construction; the zero
	// value blocks them, so a caller that forgets gets the safe behaviour.
	notifyPolicy notify.TargetPolicy
	// channelClient is the one guarded HTTP client every org-channel notifier
	// shares (D-25), so deliveries reuse a single connection pool.
	channelClient *http.Client

	// deliverySem bounds concurrent org-channel deliveries and deliveryWG lets
	// Stop wait for them.
	//
	// Deliveries used to be bare `go` statements detached with
	// context.WithoutCancel: a sweep where 5000 flows alert across 3 channels
	// each launched 15000 goroutines, 15000 transports and 15000 DNS lookups at
	// once, and Stop neither waited for nor cancelled any of them — so shutdown
	// silently dropped whatever was in flight. Blocking on a full semaphore
	// applies backpressure to the sweep, which is the correct trade: a slow sweep
	// is better than an unbounded fan-out.
	deliverySem chan struct{}
	deliveryWG  sync.WaitGroup

	loop lifecycle.TickerLoop
}

// New creates a Scanner. interval <= 0 disables periodic scanning (Start is a
// no-op), but ScanOnce can still be invoked directly (e.g. from a test or an
// admin "scan now" action).
func New(backend storageif.StorageBackend, analyze AnalyzeFunc, notifier *notify.Dispatcher, interval time.Duration, notifyPolicy notify.TargetPolicy) *Scanner {
	return &Scanner{
		backend:       backend,
		analyze:       analyze,
		notifier:      notifier,
		interval:      interval,
		lastSig:       make(map[string]string),
		notifyPolicy:  notifyPolicy,
		channelClient: notify.NewChannelClient(notifyPolicy),
		deliverySem:   make(chan struct{}, maxConcurrentDeliveries),
	}
}

// maxConcurrentDeliveries bounds in-flight org-channel POSTs across the whole
// scanner. High enough that a normal sweep never blocks, low enough that a
// pathological one cannot exhaust file descriptors.
const maxConcurrentDeliveries = 32

// deliveryDrainBudget bounds how long Stop waits for in-flight deliveries,
// mirroring lifecycle.stopDrainBudget so the two together stay inside the app's
// 25s fx.StopTimeout.
const deliveryDrainBudget = 5 * time.Second

// SetEventNotifier injects an SSE push sink so newly-detected alerts reach
// connected clients in real time (the notifications bell updates instantly
// instead of on the next poll). Optional: a nil sink leaves push disabled.
func (s *Scanner) SetEventNotifier(n SSENotifier) { s.sse = n }

// enabled reports whether periodic scanning should run: it needs a backend, an
// analyzer and a positive interval. A deployment-level notification channel
// (PAD_NOTIFY_*) is NOT required: the sweep also records in-app governance
// alerts and delivers to each org's own channels, both of which are configured
// without any env-level channel. Requiring one here left the org channel UI
// and the alerts inbox silently inert on every deployment that used only them.
func (s *Scanner) enabled() bool {
	return s != nil && s.backend != nil && s.analyze != nil && s.interval > 0
}

// Start launches the periodic scan loop in a background goroutine. It returns
// immediately and is a no-op when scanning is disabled. The loop runs until
// Stop. Scanner does not sweep immediately on
// Start — it waits for the first tick.
func (s *Scanner) Start() {
	if !s.enabled() {
		logger.Info("scanner: disabled (needs a storage backend and a positive PAD_SCAN_INTERVAL)")
		return
	}
	logger.Info("scanner: starting", "interval", s.interval.String())
	s.loop.Start(s.interval, false, func(ctx context.Context) {
		// Derive the tick ctx from the loop's root ctx so Stop cancels an
		// in-flight sweep (not just the next tick): ScanOnce checks ctx.Err()
		// between flows, so cancelling the root makes the current sweep exit
		// at the next flow boundary rather than running for up to
		// PAD_SCAN_INTERVAL.
		tickCtx, cancel := context.WithTimeout(ctx, s.interval)
		defer cancel()
		s.ScanOnce(tickCtx)
	}, func(r any) {
		logger.Error("scanner: loop panicked", "recover", r)
	})
}

// Stop halts the scan loop and cancels any in-flight sweep. Safe to call
// multiple times, and safe to call even if Start was never invoked (e.g. a
// disabled scanner).
func (s *Scanner) Stop() {
	s.loop.Stop()

	// Then wait for deliveries the last sweep spawned: they are deliberately
	// detached from the sweep context (cancelling mid-POST would abandon a
	// half-sent alert), so without this they were simply dropped on shutdown.
	done := make(chan struct{})
	go func() {
		s.deliveryWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(deliveryDrainBudget):
		logger.Warn("scanner: gave up waiting for in-flight alert deliveries", "budget", deliveryDrainBudget)
	}
}

// ScanOnce scans every stored flow once, paginating to cover the entire
// library (not just the first page). Errors are logged, never returned, so a
// single bad flow doesn't abort the sweep.
func (s *Scanner) ScanOnce(ctx context.Context) {
	if s.backend == nil || s.analyze == nil {
		return
	}
	// One sweep at a time ACROSS replicas: every pod runs its own ticker, so
	// without this N replicas re-analysed every flow N times per interval and
	// each of them dispatched the same alert to the org's webhooks (the dedup
	// signature is per process). The Postgres backend exposes a session-level
	// advisory lock; a backend without one (memory, tests) sweeps as before.
	if locker, ok := s.backend.(globalLocker); ok {
		release, acquired, err := locker.TryGlobalLock(ctx, scanLockKey)
		switch {
		case err != nil:
			logger.Warn("scanner: advisory lock unavailable; sweeping without it", "err", err)
		case !acquired:
			logger.Info("scanner: sweep already running on another replica; skipping this tick")
			return
		default:
			defer release()
		}
	}
	// Fresh per-sweep org-channel cache: a channel added mid-sweep may be
	// missed until the next sweep (acceptable) but a DELETED one must stop
	// receiving promptly, and a stale cache would keep delivering to it.
	s.orgChMu.Lock()
	s.orgNotifiers = nil
	s.orgChMu.Unlock()
	const pageSize = 100
	offset := 0
	seen := make(map[string]struct{})
	for {
		if ctx.Err() != nil {
			return
		}
		flows, err := s.backend.ListFlows(ctx, storageif.FlowFilter{
			AllFlows: true,
			// Immutable ordering: this sweep pages with LIMIT/OFFSET, and the
			// default updated_at DESC is mutable — a flow SAVED mid-sweep jumps
			// to the front and pushes the row on the next page boundary out of
			// the walk entirely. Users editing during a sweep is the normal
			// case, so that skipped flow silently goes ungoverned, and the
			// pruneLastSig call below then drops its dedup keys because `seen`
			// looked complete.
			SortBy: storageif.FlowSortIDAsc,
			Limit:  pageSize,
			Offset: offset,
		})
		if err != nil {
			logger.Warn("scanner: list flows failed", "offset", offset, "err", err)
			return
		}
		if len(flows) == 0 {
			break
		}
		for _, f := range flows {
			if ctx.Err() != nil {
				return
			}
			seen[f.ID] = struct{}{}
			fctx, cancel := context.WithTimeout(ctx, perFlowScanTimeout)
			s.scanFlow(fctx, f)
			cancel()
		}
		offset += len(flows)
		if len(flows) < pageSize {
			break // last page
		}
	}
	// Only reached via a complete, uninterrupted sweep, so seen holds every
	// currently-stored flow ID — safe to prune dedup entries for flows deleted
	// since the last sweep (a partial/cancelled sweep must not prune, since
	// seen would then be missing flows that still exist).
	s.pruneLastSig(seen)

	// H20: surface loop liveness so ops can alert on a hung scanner
	// (time() - last_tick > 2 * interval). Without this the only signal of a
	// deadlock is "logs stopped" — invisible to /healthz which always returns 200.
	metrics.RecordBackgroundLoopTick("scanner")
}

// globalLocker mirrors api.GlobalLocker (the scanner cannot import the api
// package): the Postgres backend's TryGlobalLock. acquired=false means another
// session holds the key; release must be called exactly once when acquired.
type globalLocker interface {
	TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error)
}

// scanLockKey namespaces the sweep's advisory lock. Arbitrary but stable —
// every replica must use the same key, and it must differ from the migration
// runner's.
const scanLockKey int64 = 0x62616b697363616e // "bakiscan"

// pruneLastSig drops lastSig entries for flows no longer present, so a
// deleted flow's dedup keys don't accumulate in memory for the life of the
// process.
func (s *Scanner) pruneLastSig(seen map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.lastSig {
		flowID, _, ok := strings.Cut(key, "|")
		if !ok {
			continue
		}
		if _, present := seen[flowID]; !present {
			delete(s.lastSig, key)
		}
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
			// A flow the sweep cannot analyze never produces drift/regression
			// alerts; route it to the exception sink so it is not lost in logs.
			errreport.CaptureError(ctx, "scanner.analyze", err, errreport.Attrs{"flowId": lf.ID})
		}
		return
	}

	for _, ev := range s.events(ctx, lf, report) {
		if s.shouldAlert(lf.ID, ev) {
			// Persist the alert to the in-app inbox (best-effort: a store error
			// must not block the external-channel dispatch). The scanner runs
			// with a system context, so RLS is bypassed (NOT app_rls_active()).
			s.recordAlert(ctx, lf, ev)
			// Push a real-time ping to connected clients who can see this flow
			// so the notifications bell updates instantly (no 60s poll wait).
			s.pushAlertSSE(ctx, lf, ev)
			s.notifier.Dispatch(ctx, ev)
			// Per-org channels (R2-3): the flow's org ALSO routes to its own
			// configured destinations — ownership routing instead of one
			// deployment-global blast.
			s.dispatchOrgChannels(ctx, lf, ev)
		}
	}
}

// pushAlertSSE delivers a lightweight "governance:alert" event over SSE to every
// connected client who can see the flow (owner + collaborators + org members).
// The payload is a hint, not the alert itself: the client re-fetches the
// authoritative, RLS-scoped unread count + list via REST, so no cross-tenant
// data can leak (a recipient who lacks access simply sees no change).
func (s *Scanner) pushAlertSSE(ctx context.Context, lf *storageif.FlowDocument, ev notify.Event) {
	if s.sse == nil {
		return
	}
	for _, uid := range s.alertRecipients(ctx, lf) {
		s.sse.EmitTo(uid, "governance:alert", map[string]any{
			"flowId":   lf.ID,
			"flowName": lf.Name,
			"type":     string(ev.Type),
			"title":    ev.Title,
			"severity": alertSeverity(ev),
		})
	}
}

// alertRecipients resolves the set of user IDs allowed to see the flow
// (mirroring the gov_alerts RLS visibility policy): the owner, explicit
// collaborators, and — when the flow is org-scoped — all org members. Errors are
// logged and skipped: a failed lookup must not suppress delivery to the users we
// did resolve. De-duplicated.
func (s *Scanner) alertRecipients(ctx context.Context, lf *storageif.FlowDocument) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	add := func(uid string) {
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	add(lf.OwnerID)
	if s.backend != nil {
		if cols, err := s.backend.ListCollaborators(ctx, lf.ID, 0); err == nil {
			for _, c := range cols {
				add(c.UserID)
			}
		} else {
			logger.Warn("scanner: list collaborators for SSE recipients failed", "flowId", lf.ID, "err", err)
		}
		if lf.OrganizationID != "" {
			if org, err := s.backend.LoadOrg(ctx, lf.OrganizationID); err == nil {
				for _, m := range org.Members {
					add(m.UserID)
				}
			} else {
				logger.Warn("scanner: load org for SSE recipients failed", "orgId", lf.OrganizationID, "err", err)
			}
		}
	}
	return out
}

// recordAlert writes a governance event to the in-app alert store so the
// notifications bell can surface it. Best-effort: failures are logged and never
// propagated (the external-channel dispatch is the primary delivery path). The
// alert ID is derived from (flowID|eventType|signature) so a re-alert for the
// same persistent regression reuses the same row (ON CONFLICT DO NOTHING), and
// acknowledging it once sticks until the regression shape changes.
func (s *Scanner) recordAlert(ctx context.Context, lf *storageif.FlowDocument, ev notify.Event) {
	if s.backend == nil {
		return
	}
	alert := &storageif.GovernanceAlert{
		ID:          fmt.Sprintf("%s|%s|%s", lf.ID, ev.Type, signature(ev)),
		FlowID:      lf.ID,
		FlowName:    lf.Name,
		OrgID:       lf.OrganizationID,
		Type:        string(ev.Type),
		Title:       ev.Title,
		Message:     ev.Message,
		Severity:    alertSeverity(ev),
		NewErrors:   ev.NewErrors,
		NewWarnings: ev.NewWarnings,
		HealthScore: ev.HealthScore,
		PrevHealth:  ev.PrevHealth,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.backend.RecordGovernanceAlert(ctx, alert); err != nil {
		logger.Warn("scanner: persist governance alert failed", "flowId", lf.ID, "type", ev.Type, "err", err)
	}
}

// alertSeverity maps a governance event to an inbox severity: a drift with new
// errors (or any health regression) is "error"-tier; drift with only new
// warnings is "warning"-tier.
func alertSeverity(ev notify.Event) string {
	if ev.NewErrors > 0 || ev.Type == notify.EventHealthRegression {
		return "error"
	}
	return "warning"
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
	// Stored content is client-supplied: never trust its desktop file path
	// (see resolveStoredFlow in internal/service/document_provider.go).
	doc.FilePath = ""
	doc.RebuildIndexes()
	return &doc
}

func flowLabel(lf *storageif.FlowDocument) string {
	if lf.Name != "" {
		return lf.Name
	}
	return lf.ID
}

// dispatchOrgChannels delivers an event to the flow's org's own configured
// channels (webhook/Teams/Slack). Best-effort and fully parallel to the
// global dispatch: per-channel failures are logged inside notify.Deliver and
// never affect the scan loop. A short-lived per-org channel cache avoids
// re-reading the table on every flow of the same org within one sweep.
func (s *Scanner) dispatchOrgChannels(ctx context.Context, lf *storageif.FlowDocument, ev notify.Event) {
	if s.backend == nil || lf.OrganizationID == "" {
		return
	}
	notifiers, ok := s.notifiersForOrg(ctx, lf.OrganizationID)
	if !ok {
		return
	}
	for _, n := range notifiers {
		// Detach from the per-flow scan context: it is canceled the moment
		// scanFlow returns, which would kill the delivery goroutine mid-POST.
		// The dispatcher's own timeout bounds the work.
		delivery := context.WithoutCancel(ctx)

		// Acquire BEFORE spawning, so a sweep with thousands of alerting flows
		// slows down instead of launching a goroutine per alert. Honour the
		// sweep context while waiting: on shutdown, stop queueing new work.
		select {
		case s.deliverySem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		s.deliveryWG.Add(1)
		go func(n notify.AlertNotifier) {
			defer s.deliveryWG.Done()
			defer func() { <-s.deliverySem }()
			notify.Deliver(delivery, n, ev)
		}(n)
	}
}

// notifiersForOrg returns the org's built notifiers, constructing them at most
// once per sweep. The second return is false when the org's channels could not
// be listed (already logged).
//
// The build is cached, not just the channel rows: BuildChannelNotifier clones
// http.DefaultTransport, so one per event per channel meant a brand-new
// connection pool for every alert — no keep-alive reuse, a fresh DNS lookup
// each time (validateAlertURL resolves), and a transport left behind with idle
// connections nobody closes.
func (s *Scanner) notifiersForOrg(ctx context.Context, orgID string) ([]notify.AlertNotifier, bool) {
	s.orgChMu.Lock()
	cached, ok := s.orgNotifiers[orgID]
	s.orgChMu.Unlock()
	if ok {
		return cached, true
	}

	channels, err := s.backend.ListOrgChannels(ctx, orgID, true)
	if err != nil {
		logger.Warn("scanner: list org channels failed", "orgId", orgID, "err", err)
		return nil, false
	}
	built := make([]notify.AlertNotifier, 0, len(channels))
	for _, ch := range channels {
		n, err := notify.BuildChannelNotifierWith(ctx, s.channelClient, ch.Kind, ch.URL, ch.Secret, s.notifyPolicy)
		if err != nil {
			logger.Warn("scanner: invalid org channel", "orgId", orgID, "channel", ch.Name, "err", err)
			continue
		}
		built = append(built, n)
	}

	s.orgChMu.Lock()
	if s.orgNotifiers == nil {
		s.orgNotifiers = make(map[string][]notify.AlertNotifier)
	}
	s.orgNotifiers[orgID] = built
	s.orgChMu.Unlock()
	return built, true
}
