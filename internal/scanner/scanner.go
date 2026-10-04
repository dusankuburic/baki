// Package scanner runs the continuous-governance loop: on an interval it
// re-analyzes every stored flow, compares against the accepted baseline
// (drift) and last recorded health (regression), and alerts on anything new.
// Best-effort, read-only, cloud-only.
package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"sort"
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

// perFlowScanTimeout bounds the analysis of a single flow within a tick (the
// tick-level context bounds the whole sweep). A var so tests can shorten it.
var perFlowScanTimeout = 30 * time.Second

// pageReadTimeout bounds each of a page's batched storage reads (a failed or
// timed-out batch falls back to per-flow reads). A var so tests can shorten it.
var pageReadTimeout = perFlowScanTimeout

// AnalyzeFunc analyzes a parsed flow document; in production
// AnalysisService.AnalyzeFlowBackground, a stub in tests.
type AnalyzeFunc func(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error)

// RuleSetDigestFunc returns a digest of the rule set and settings AnalyzeFunc
// applies to a flow's org, or "" — without one the scanner re-analyzes every flow every sweep.
type RuleSetDigestFunc func(ctx context.Context, orgID string) string

// SSENotifier pushes real-time events to connected frontend clients; nil disables
// push. Defined here so the scanner stays decoupled from the service layer.
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
	// lastScan maps flowID -> the scanInputs key of its last complete scan
	// (guarded by mu); an unchanged key skips the flow.
	lastScan map[string]string

	// ruleSetDigest is optional; nil disables skipping. ruleDigests memoizes
	// it per org for the current sweep (under orgChMu, reset with orgNotifiers).
	ruleSetDigest RuleSetDigestFunc
	ruleDigests   map[string]string

	// orgNotifiers caches the BUILT notifiers per org for the current sweep
	// (under orgChMu) — see notifiersForOrg for why the build is cached.
	orgChMu      sync.Mutex
	orgNotifiers map[string][]notify.AlertNotifier
	// orgRosters caches each org's member user IDs for the current sweep
	// (alertRecipients), under orgChMu.
	orgRosters map[string][]string

	// notifyPolicy decides whether org-channel deliveries may reach loopback/
	// private addresses; the zero value blocks them (safe default).
	notifyPolicy notify.TargetPolicy
	// channelClient is the one guarded HTTP client every org-channel notifier
	// shares (D-25), so deliveries reuse a single connection pool.
	channelClient *http.Client

	// deliverySem bounds concurrent org-channel deliveries (backpressure
	// instead of an unbounded fan-out); deliveryWG lets Stop wait for them.
	deliverySem chan struct{}
	deliveryWG  sync.WaitGroup

	loop lifecycle.TickerLoop
}

// New creates a Scanner. interval <= 0 disables periodic scanning (Start is a
// no-op); ScanOnce can still be invoked directly.
func New(backend storageif.StorageBackend, analyze AnalyzeFunc, notifier *notify.Dispatcher, interval time.Duration, notifyPolicy notify.TargetPolicy) *Scanner {
	return &Scanner{
		backend:       backend,
		analyze:       analyze,
		notifier:      notifier,
		interval:      interval,
		lastSig:       make(map[string]string),
		lastScan:      make(map[string]string),
		notifyPolicy:  notifyPolicy,
		channelClient: notify.NewChannelClient(notifyPolicy),
		deliverySem:   make(chan struct{}, maxConcurrentDeliveries),
	}
}

// maxConcurrentDeliveries bounds in-flight org-channel POSTs: high enough that
// a normal sweep never blocks, low enough not to exhaust file descriptors.
const maxConcurrentDeliveries = 32

// deliveryDrainBudget bounds how long Stop waits for in-flight deliveries,
// mirroring lifecycle.stopDrainBudget so both stay inside the 25s fx.StopTimeout.
const deliveryDrainBudget = 5 * time.Second

// SetEventNotifier injects an SSE push sink so new alerts reach connected
// clients in real time; nil leaves push disabled.
func (s *Scanner) SetEventNotifier(n SSENotifier) { s.sse = n }

// SetRuleSetDigest injects the rule-set digest that lets a sweep skip flows
// whose scan inputs have not changed. Call before Start.
func (s *Scanner) SetRuleSetDigest(fn RuleSetDigestFunc) { s.ruleSetDigest = fn }

// enabled reports whether periodic scanning should run: a backend, an analyzer
// and a positive interval; PAD_NOTIFY_* is NOT required (in-app alerts still record).
func (s *Scanner) enabled() bool {
	return s != nil && s.backend != nil && s.analyze != nil && s.interval > 0
}

// Start launches the periodic scan loop in a background goroutine; a no-op
// when disabled. Does not sweep immediately — waits for the first tick.
func (s *Scanner) Start() {
	if !s.enabled() {
		logger.Info("scanner: disabled (needs a storage backend and a positive PAD_SCAN_INTERVAL)")
		return
	}
	logger.Info("scanner: starting", "interval", s.interval.String())
	s.loop.Start(s.interval, false, func(ctx context.Context) {
		// Derive the tick ctx from the loop's root ctx so Stop cancels an
		// in-flight sweep; ScanOnce checks ctx.Err() between flows.
		tickCtx, cancel := context.WithTimeout(ctx, s.interval)
		defer cancel()
		s.ScanOnce(tickCtx)
	}, func(r any) {
		logger.Error("scanner: loop panicked", "recover", r)
	})
}

// Stop halts the scan loop and cancels any in-flight sweep; safe to call
// multiple times or when Start was never invoked.
func (s *Scanner) Stop() {
	s.loop.Stop()

	// Wait for deliveries the last sweep spawned: they are deliberately
	// detached (cancelling mid-POST would abandon a half-sent alert).
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
// library. Errors are logged, never returned — one bad flow doesn't abort the
// sweep.
func (s *Scanner) ScanOnce(ctx context.Context) {
	if s.backend == nil || s.analyze == nil {
		return
	}
	// One sweep at a time ACROSS replicas: every pod runs its own ticker and the
	// dedup signature is per process, so N replicas would re-analyze every flow
	// N times per interval. Postgres exposes a session-level advisory lock; a
	// backend without one sweeps as before.
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
	// Fresh per-sweep caches: a channel/member DELETED mid-sweep must stop
	// receiving promptly; a mid-sweep rule change is seen by the next sweep.
	s.orgChMu.Lock()
	s.orgNotifiers = nil
	s.ruleDigests = nil
	s.orgRosters = nil
	s.orgChMu.Unlock()
	const pageSize = 100
	after := ""
	seen := make(map[string]struct{})
	for {
		if ctx.Err() != nil {
			return
		}
		lctx, lcancel := context.WithTimeout(ctx, pageReadTimeout)
		flows, err := s.backend.ListFlows(lctx, storageif.FlowFilter{
			AllFlows: true,
			// Immutable ordering: updated_at DESC is mutable — a flow SAVED
			// mid-sweep jumps to the front and pushes the next page-boundary
			// row out of the walk (silently ungoverned).
			SortBy: storageif.FlowSortIDAsc,
			// Keyset cursor, not OFFSET: the database never re-reads earlier
			// rows, and a flow INSERTED mid-sweep cannot shift the cursor.
			AfterID: after,
			Limit:   pageSize,
			// Headers only: the skip check needs no content; scanFlow loads
			// the document of a flow it actually re-analyzes.
			MetadataOnly: true,
		})
		lcancel()
		if err != nil {
			logger.Warn("scanner: list flows failed", "after", after, "err", err)
			return
		}
		if len(flows) == 0 {
			break
		}
		// A backend that ignored AfterID would serve the first page forever —
		// detected as a page whose first id was already seen, NOT by comparing
		// ids (ORDER BY id follows the column collation, not Go byte order).
		// Stop; being incomplete, do not prune below.
		if _, dup := seen[flows[0].ID]; dup {
			logger.Warn("scanner: flow page did not advance; stopping sweep", "after", after, "first", flows[0].ID)
			return
		}
		page := s.loadPageInputs(ctx, flows)
		for _, f := range flows {
			if ctx.Err() != nil {
				return
			}
			seen[f.ID] = struct{}{}
			fctx, cancel := context.WithTimeout(ctx, perFlowScanTimeout)
			s.scanFlow(fctx, f, page)
			cancel()
		}
		after = flows[len(flows)-1].ID
		if len(flows) < pageSize {
			break // last page
		}
	}
	// Only reached via a complete, uninterrupted sweep, so seen holds every
	// stored flow ID — safe to prune dedup entries for deleted flows.
	s.pruneLastSig(seen)

	// Surface loop liveness so ops can alert on a hung scanner (time() -
	// last_tick > 2 * interval), which /healthz cannot see.
	metrics.RecordBackgroundLoopTick("scanner")
}

// globalLocker mirrors api.GlobalLocker (the scanner cannot import the api
// package): the Postgres backend's TryGlobalLock; release exactly once when acquired.
type globalLocker interface {
	TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error)
}

// scanLockKey namespaces the sweep's advisory lock: every replica must use the
// same key, and it must differ from the migration runner's.
const scanLockKey int64 = 0x62616b697363616e // "bakiscan"

// pruneLastSig drops lastSig and lastScan entries for flows no longer present,
// so a deleted flow's dedup keys don't accumulate in memory.
func (s *Scanner) pruneLastSig(seen map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for flowID := range s.lastScan {
		if _, present := seen[flowID]; !present {
			delete(s.lastScan, flowID)
		}
	}
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

// scanFlow scans one flow of a page: hdr is the flow's header row, page the
// page's batched baselines and health.
func (s *Scanner) scanFlow(ctx context.Context, hdr *storageif.FlowDocument, page pageInputs) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("scanner: flow scan panicked", "flowId", hdr.ID, "recover", r)
		}
	}()

	// Skip a flow whose inputs match its last complete scan: the analysis and
	// its events would be identical, and every event was already de-duplicated.
	// A skipped flow's content is never read.
	in := page.inputsFor(ctx, s, hdr)
	if in.key != "" && s.lastScanKey(hdr.ID) == in.key {
		return
	}
	// Forget the previous key until this scan completes: a scan that fails or
	// panics part-way must not leave a key a later sweep matches.
	s.setLastScanKey(hdr.ID, "")

	lf, err := s.backend.LoadFlow(ctx, hdr.ID)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			return // deleted since the page was listed; pruned next sweep
		}
		logger.Warn("scanner: load flow failed", "flowId", hdr.ID, "err", err)
		return
	}
	// The document may have been saved between the page listing and this load;
	// key the scan on what is actually analyzed (lf), not the stale header.
	in.key = page.keyOf(ctx, s, lf, in)

	doc := parseStored(lf.Content)
	if doc == nil {
		// Unparseable content stays unparseable until it changes.
		s.setLastScanKey(lf.ID, in.key)
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

	for _, ev := range s.events(lf, report, in) {
		if s.shouldAlert(lf.ID, ev) {
			// Persist to the in-app inbox (best-effort); the system context
			// bypasses RLS.
			s.recordAlert(ctx, lf, ev)
			// Ping connected clients who can see this flow so the bell updates.
			s.pushAlertSSE(ctx, lf, ev)
			s.notifier.Dispatch(ctx, ev)
			// The flow's org ALSO routes to its own configured destinations.
			s.dispatchOrgChannels(ctx, lf, ev)
		}
	}
	s.setLastScanKey(lf.ID, in.key)
}

// scanInputs holds one flow's non-analysis inputs (baseline, last health).
// key identifies the FULL input set — flow ID/version/owner/org, the org's
// rule-set digest, the baseline's keys, the last health score — so equal keys
// mean an identical analysis and the flow is skipped; key "" never skips or
// records (unreadable input or no digest wired).
type scanInputs struct {
	baseline *storageif.FlowBaseline
	health   *storageif.HealthSnapshot
	// baselineOK/healthOK report whether the load succeeded (nil + ok = none recorded).
	baselineOK, healthOK bool
	key                  string
}

// pageInputs holds the baselines and recorded health of one page of flows, read
// in two batched queries rather than two per flow. The *Failed sets hold flows
// whose read failed; their key is "" (neither skipped nor recorded).
type pageInputs struct {
	baselines      map[string]*storageif.FlowBaseline
	health         map[string]*storageif.HealthSnapshot
	baselineFailed map[string]struct{}
	healthFailed   map[string]struct{}
}

// loadPageInputs reads the page's baselines and health, each in one batched
// query under pageReadTimeout, falling back to per-flow reads when a batch fails.
func (s *Scanner) loadPageInputs(ctx context.Context, flows []*storageif.FlowDocument) pageInputs {
	ids := make([]string, len(flows))
	for i, f := range flows {
		ids[i] = f.ID
	}
	page := pageInputs{
		baselineFailed: make(map[string]struct{}),
		healthFailed:   make(map[string]struct{}),
	}
	page.baselines = readPageInput(ctx, ids, page.baselineFailed, "baseline",
		s.backend.GetFlowBaselinesBatch, s.backend.GetFlowBaseline)
	page.health = readPageInput(ctx, ids, page.healthFailed, "health",
		s.backend.LoadFlowHealthBatch, s.backend.LoadFlowHealth)
	return page
}

// readPageInput reads one per-flow input ("baseline"/"health") for the whole
// page: one batched query, per-flow fallback when the batch fails (undecodable
// row, timeout), so only the genuinely unreadable flow loses its check.
func readPageInput[T any](ctx context.Context, ids []string, failed map[string]struct{}, what string,
	batch func(context.Context, []string) (map[string]*T, error),
	one func(context.Context, string) (*T, error)) map[string]*T {
	bctx, cancel := context.WithTimeout(ctx, pageReadTimeout)
	m, err := batch(bctx, ids)
	cancel()
	if err == nil {
		return m
	}
	logger.Warn("scanner: load "+what+" batch failed; reading per flow", "flows", len(ids), "err", err)
	m = make(map[string]*T, len(ids))
	for _, id := range ids {
		fctx, cancel := context.WithTimeout(ctx, perFlowScanTimeout)
		v, err := one(fctx, id)
		cancel()
		if err != nil {
			logger.Warn("scanner: load "+what+" failed", "flowId", id, "err", err)
			failed[id] = struct{}{}
			continue
		}
		if v != nil {
			m[id] = v
		}
	}
	return m
}

// inputsFor picks one flow's inputs out of the page and derives its scan key
// from the flow's header.
func (p pageInputs) inputsFor(ctx context.Context, s *Scanner, hdr *storageif.FlowDocument) scanInputs {
	var in scanInputs
	if _, failed := p.baselineFailed[hdr.ID]; !failed {
		in.baseline, in.baselineOK = p.baselines[hdr.ID], true
	}
	if _, failed := p.healthFailed[hdr.ID]; !failed {
		in.health, in.healthOK = p.health[hdr.ID], true
	}
	in.key = p.keyOf(ctx, s, hdr, in)
	return in
}

// keyOf derives the scan key (see scanInputs); "" when either input could not
// be read or no digest is wired. Only hdr's ID/Version/OwnerID/OrgID are read.
func (p pageInputs) keyOf(ctx context.Context, s *Scanner, hdr *storageif.FlowDocument, in scanInputs) string {
	if !in.baselineOK || !in.healthOK {
		return ""
	}
	rules := s.ruleDigestFor(ctx, hdr.OrganizationID)
	if rules == "" {
		return ""
	}

	h := sha256.New()
	field := func(v string) { fmt.Fprintf(h, "%d:%s;", len(v), v) }
	field(hdr.ID)
	fmt.Fprintf(h, "version:%d;", hdr.Version)
	field(hdr.OwnerID)
	field(hdr.OrganizationID)
	field(rules)
	if in.baseline == nil {
		field("no-baseline")
	} else {
		fmt.Fprintf(h, "baseline:%d;", len(in.baseline.Keys))
		for _, k := range in.baseline.Keys {
			field(k)
		}
	}
	if in.health == nil {
		field("no-health")
	} else {
		fmt.Fprintf(h, "health:%d;", in.health.HealthScore)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ruleDigestFor returns the org's rule-set digest, resolved at most once per
// sweep; "" without a RuleSetDigestFunc.
func (s *Scanner) ruleDigestFor(ctx context.Context, orgID string) string {
	if s.ruleSetDigest == nil {
		return ""
	}
	s.orgChMu.Lock()
	d, ok := s.ruleDigests[orgID]
	s.orgChMu.Unlock()
	if ok {
		return d
	}
	d = s.ruleSetDigest(ctx, orgID)
	s.orgChMu.Lock()
	if s.ruleDigests == nil {
		s.ruleDigests = make(map[string]string)
	}
	s.ruleDigests[orgID] = d
	s.orgChMu.Unlock()
	return d
}

func (s *Scanner) lastScanKey(flowID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastScan[flowID]
}

// setLastScanKey records key as flowID's last complete scan; "" forgets it.
func (s *Scanner) setLastScanKey(flowID, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		delete(s.lastScan, flowID)
		return
	}
	s.lastScan[flowID] = key
}

// DigestRuleSet digests an analysis configuration for RuleSetDigestFunc: rule
// IDs (each with its RuleDigest, as the analyzer's cache key does) and the
// per-rule settings as JSON, so any change counts.
func DigestRuleSet(settings *models.AppSettings, rules []analyzer.Rule) string {
	ids := make([]string, 0, len(rules))
	for _, r := range rules {
		if d, ok := r.(analyzer.RuleDigester); ok {
			ids = append(ids, r.ID()+"@"+d.RuleDigest())
			continue
		}
		ids = append(ids, r.ID())
	}
	sort.Strings(ids)
	h := sha256.New()
	writeLenPrefixed(h, fmt.Sprintf("rules:%d", len(ids)))
	for _, id := range ids {
		writeLenPrefixed(h, id)
	}
	if settings == nil {
		writeLenPrefixed(h, "no-settings")
	} else {
		// encoding/json sorts map keys, so equal settings encode equally.
		b, err := json.Marshal(settings.Analysis.Rules)
		if err != nil {
			return ""
		}
		writeLenPrefixed(h, string(b))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeLenPrefixed(h hash.Hash, v string) {
	fmt.Fprintf(h, "%d:%s;", len(v), v)
}

// pushAlertSSE delivers a lightweight "governance:alert" SSE event to every
// connected client who can see the flow. The payload is a hint: the client
// re-fetches the authoritative, RLS-scoped data via REST (no cross-tenant leak).
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

// alertRecipients resolves the user IDs allowed to see the flow (mirroring the
// gov_alerts RLS visibility policy): owner, collaborators, and — when
// org-scoped — all org members. De-duplicated; errors logged and skipped.
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
			for _, uid := range s.orgRoster(ctx, lf.OrganizationID) {
				add(uid)
			}
		}
	}
	return out
}

// orgRoster returns the org's member user IDs, loading the org at most once
// per sweep; a failed load is cached as an empty roster (one read, not per alert).
func (s *Scanner) orgRoster(ctx context.Context, orgID string) []string {
	s.orgChMu.Lock()
	cached, ok := s.orgRosters[orgID]
	s.orgChMu.Unlock()
	if ok {
		return cached
	}

	var roster []string
	if org, err := s.backend.LoadOrg(ctx, orgID); err == nil {
		roster = make([]string, 0, len(org.Members))
		for _, m := range org.Members {
			roster = append(roster, m.UserID)
		}
	} else {
		logger.Warn("scanner: load org for SSE recipients failed", "orgId", orgID, "err", err)
	}

	s.orgChMu.Lock()
	if s.orgRosters == nil {
		s.orgRosters = make(map[string][]string)
	}
	s.orgRosters[orgID] = roster
	s.orgChMu.Unlock()
	return roster
}

// recordAlert writes a governance event to the in-app alert store (best-effort).
// The ID derives from (flowID|eventType|signature) so a re-alert reuses the
// same row (ON CONFLICT DO NOTHING); one acknowledgement sticks.
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

// alertSeverity maps an event to an inbox severity: drift with new errors (or
// any health regression) is error-tier; warnings-only drift is warning-tier.
func alertSeverity(ev notify.Event) string {
	if ev.NewErrors > 0 || ev.Type == notify.EventHealthRegression {
		return "error"
	}
	return "warning"
}

// events derives the alerts for one freshly-analyzed flow: drift (new findings
// since the accepted baseline) and health regression (health below last recorded).
func (s *Scanner) events(lf *storageif.FlowDocument, report *models.AnalysisReport, in scanInputs) []notify.Event {
	var events []notify.Event
	name := lf.Name

	// Drift vs. accepted baseline. Only meaningful once a baseline exists;
	// otherwise every finding is "new" and would alert on the first scan.
	if bl := in.baseline; bl != nil {
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
		if prior := in.health; prior != nil && report.Metrics.HealthScore < prior.HealthScore {
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

// shouldAlert de-duplicates: true only when the event's signature differs from
// the last one sent for the same (flow, type), so a persistent regression is
// reported once, not on every tick.
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
// channels (webhook/Teams/Slack), best-effort, parallel to the global dispatch;
// per-channel failures are logged inside notify.Deliver.
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
		// scanFlow returns, which would kill the delivery mid-POST; the
		// dispatcher's own timeout bounds the work.
		delivery := context.WithoutCancel(ctx)

		// Acquire BEFORE spawning so a sweep with thousands of alerting flows
		// slows down instead of spawning a goroutine per alert; honour the sweep
		// context while waiting so shutdown stops queueing work.
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

// notifiersForOrg returns the org's built notifiers, at most once per sweep.
// The BUILD is cached, not just the channel rows: BuildChannelNotifier clones
// http.DefaultTransport, so one per event meant a fresh connection pool per alert.
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
