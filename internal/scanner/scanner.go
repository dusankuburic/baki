// Package scanner runs the continuous-governance loop: on an interval it
// re-analyzes every stored flow, compares against the accepted baseline
// (drift) and last recorded health (regression), and alerts on anything new.
// Findings of rules newer than the baseline's or snapshot's rule set raise one
// "new rules" notice per scope instead (ADR 0016). Best-effort, read-only,
// cloud-only.
package scanner

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"pad-analyzer/internal/platform/lock"
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

// HealthRecorder records a flow's analysis as its health snapshot (DashboardService.RecordAnalysis): what an
// interactive analysis or a CI run records, with the rule set it was scored under.
type HealthRecorder func(ctx context.Context, doc *models.FlowDocument, report *models.AnalysisReport)

// OrgChannels builds and delivers to an organisation's own notification channels (service.ChannelService): the
// sweep builds an org's channels once and hands each delivery to its runner.
type OrgChannels interface {
	// OrgNotifiers builds the org's enabled channels.
	OrgNotifiers(ctx context.Context, orgID string) ([]notify.AlertNotifier, error)
	// Enqueue delivers ev to n in the background, waiting for a delivery slot while ctx lives; done runs when the
	// delivery has ended, or at once when it was not started.
	Enqueue(ctx context.Context, n notify.AlertNotifier, ev notify.Event, done func()) bool
}

// SSENotifier pushes real-time events to connected frontend clients; nil disables
// push. Defined here so the scanner stays decoupled from the service layer.
type SSENotifier interface {
	EmitTo(userID, name string, data any)
}

// Scanner periodically scans stored flows for governance regressions.
type Scanner struct {
	backend  storageif.StorageBackend
	leaser   lock.Leaser // one sweep at a time across replicas; nil (no database) sweeps alone
	analyze  AnalyzeFunc
	notifier *notify.Dispatcher
	interval time.Duration
	sse      SSENotifier // optional real-time SSE push; nil = disabled

	mu      sync.Mutex
	lastSig map[string]string // (flowID|eventType) -> last alert signature, for dedup
	// lastScan maps flowID -> the scanInputs key of its last complete scan
	// (guarded by mu); an unchanged key skips the flow. lastNotes holds the
	// "new rules" note that scan made, if any, which a skip notes again.
	lastScan  map[string]string
	lastNotes map[string]*noticeNote

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
	// ruleSets caches the recorded rule sets resolved by digest for the
	// current sweep (notices.go), under orgChMu.
	ruleSets map[string]resolvedRuleSet

	// singleTenant: a deployment without accounts, whose notices are all the
	// deployment's ("app" scope) rather than an org's or a user's.
	singleTenant bool
	// recordHealth re-records a legacy health snapshot under the current rules (nil: none is).
	recordHealth HealthRecorder
	// noticeMu guards notices: the current sweep's "new rules" notices, by
	// notice id, flushed after every page (notices.go).
	noticeMu sync.Mutex
	notices  map[string]*pendingNotice

	// channels delivers to the organisations' own channels (nil: none are); its runner bounds the deliveries in
	// flight, and deliveryWG lets Stop wait for the ones this scanner started.
	channels   OrgChannels
	deliveryWG sync.WaitGroup
	// stopBudget overrides defaultStopBudget when non-zero (tests).
	stopBudget time.Duration

	// job runs the sweeps, scheduled and on demand, on one replica at a time (ADR 0023).
	job *lifecycle.Singleton
}

// scanJob is the sweep's identity across replicas. Every pod runs its own schedule and the dedup signature is per
// process, so without the lock N replicas would re-analyze every flow N times per interval.
var scanJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "scanner",
	LegacyKey: scanLockKey,
	Schedule:  "every PAD_SCAN_INTERVAL (cloud), and on demand (admin scan-now)",
})

// lockRetry is how soon a sweep skipped for a lock error is tried again, when sooner than the interval.
const lockRetry = 5 * time.Minute

// Option configures a Scanner.
type Option func(*Scanner)

// WithLeaser runs the sweeps under leases, which end a sweep whose lock was lost while it ran; it replaces New's
// locker.
func WithLeaser(l lock.Leaser) Option {
	return func(s *Scanner) { s.leaser = l }
}

// New creates a Scanner. interval <= 0 disables periodic scanning (Start is a
// no-op); ScanOnce can still be invoked directly. locker is the cross-replica
// lock (nil without a database). notifier is the deployment's channels and
// channels the organisations' (either may be nil). The backend keeps the
// sweep's run state, which tells every replica when no sweep has succeeded
// anywhere for too long.
func New(backend storageif.StorageBackend, locker lock.GlobalLocker, analyze AnalyzeFunc, notifier *notify.Dispatcher, interval time.Duration, channels OrgChannels, opts ...Option) *Scanner {
	s := &Scanner{
		backend:   backend,
		leaser:    lock.AsLeaser(locker),
		analyze:   analyze,
		notifier:  notifier,
		interval:  interval,
		lastSig:   make(map[string]string),
		lastScan:  make(map[string]string),
		lastNotes: make(map[string]*noticeNote),
		channels:  channels,
	}
	for _, o := range opts {
		o(s)
	}
	var state lifecycle.JobState
	if backend != nil {
		state = backend
	}
	s.job = lifecycle.NewSingleton(lifecycle.Job{
		Spec:  scanJob,
		Every: max(interval, 0),
		// A sweep is bounded by the interval: the next one is due then.
		Timeout:          max(interval, 0),
		RetryOnLockError: lockRetry,
		Run:              s.sweep,
	}, s.leaser, state)
	return s
}

// defaultStopBudget bounds Stop as a whole, the sweep's unwinding (within the job's own stop budget, which is no
// longer) and then the deliveries it started: the scanner's row of the server's stop budget (ADR 0024), which must
// leave the audit flush and the pool close inside fx.StopTimeout. A delivery still running at it is the "notify"
// runner's, which the server's stop drains next.
const defaultStopBudget = 5 * time.Second

// SetEventNotifier injects an SSE push sink so new alerts reach connected
// clients in real time; nil leaves push disabled.
func (s *Scanner) SetEventNotifier(n SSENotifier) { s.sse = n }

// SetRuleSetDigest injects the rule-set digest that lets a sweep skip flows
// whose scan inputs have not changed. Call before Start.
func (s *Scanner) SetRuleSetDigest(fn RuleSetDigestFunc) { s.ruleSetDigest = fn }

// SetHealthRecorder injects the recorder that replaces a legacy health snapshot, which the regression check does
// not compare, with the sweep's analysis, so the next change of the flow is compared like for like. Without one a
// legacy snapshot stays until an interactive analysis or a CI run records the flow. Call before Start.
func (s *Scanner) SetHealthRecorder(fn HealthRecorder) { s.recordHealth = fn }

// SetSingleTenant marks a deployment without accounts: every "new rules"
// notice is the deployment's, visible to its one user. Call before Start.
func (s *Scanner) SetSingleTenant(single bool) { s.singleTenant = single }

// SingleTenant reports what SetSingleTenant set.
func (s *Scanner) SingleTenant() bool { return s.singleTenant }

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
	s.job.Start()
}

// Stop halts the scan loop and cancels any in-flight sweep, also one an admin
// started, within defaultStopBudget; safe to call multiple times or when Start
// was never invoked.
func (s *Scanner) Stop() {
	budget := cmp.Or(s.stopBudget, defaultStopBudget)
	deadline := time.Now().Add(budget)
	s.job.Stop()

	// Wait for deliveries the last sweep spawned, for what is left of the budget: they are deliberately
	// detached (cancelling mid-POST would abandon a half-sent alert).
	done := make(chan struct{})
	go func() {
		s.deliveryWG.Wait()
		close(done)
	}()
	left := time.NewTimer(time.Until(deadline))
	defer left.Stop()
	select {
	case <-done:
	case <-left.C:
		// With nothing left of the budget both may be ready at once.
		select {
		case <-done:
		default:
			logger.Warn("scanner: gave up waiting for in-flight alert deliveries", "budget", budget)
		}
	}
}

// ScanOnce scans every stored flow once, paginating to cover the entire
// library, as the scan job: on one replica at a time (a replica that finds
// the lock held, or cannot ask for it, skips), cancelled by Stop. Errors are
// logged, never returned — one bad flow doesn't abort the sweep.
func (s *Scanner) ScanOnce(ctx context.Context) {
	if s.backend == nil || s.analyze == nil {
		return
	}
	_ = s.job.RunOnce(ctx) // the job logs and counts what it skipped or why it failed
}

// sweep is the scan job's run: the error is why the sweep did not complete.
func (s *Scanner) sweep(ctx context.Context) error {
	// Fresh per-sweep caches: a channel/member DELETED mid-sweep must stop
	// receiving promptly; a mid-sweep rule change is seen by the next sweep.
	s.orgChMu.Lock()
	s.orgNotifiers = nil
	s.ruleDigests = nil
	s.orgRosters = nil
	s.ruleSets = nil
	s.orgChMu.Unlock()
	s.resetNotices()
	// Whatever the sweep noted and no page flush stored, also when the tick's
	// context ends mid-page; before the lock is released.
	defer s.flushNoticesDetached(ctx)
	const pageSize = 100
	after := ""
	seen := make(map[string]struct{})
	for {
		if ctx.Err() != nil {
			return ctx.Err()
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
			return fmt.Errorf("scanner: list flows after %q: %w", after, err)
		}
		if len(flows) == 0 {
			break
		}
		// A backend that ignored AfterID would serve the first page forever —
		// detected as a page whose first id was already seen, NOT by comparing
		// ids (ORDER BY id follows the column collation, not Go byte order).
		// Stop; being incomplete, do not prune below.
		if _, dup := seen[flows[0].ID]; dup {
			return fmt.Errorf("scanner: flow page after %q did not advance (first %q); sweep stopped", after, flows[0].ID)
		}
		page := s.loadPageInputs(ctx, flows)
		for _, f := range flows {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			seen[f.ID] = struct{}{}
			fctx, cancel := context.WithTimeout(ctx, perFlowScanTimeout)
			s.scanFlow(fctx, f, page)
			cancel()
		}
		s.flushNotices(ctx, false)
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
	return nil
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
	for flowID := range s.lastNotes {
		if _, present := seen[flowID]; !present {
			delete(s.lastNotes, flowID)
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
	// A skipped flow's content is never read. The notice it fed is still
	// raised: noting it again keeps the notice from the retention sweep.
	in := page.inputsFor(ctx, s, hdr)
	if in.key != "" {
		if key, note := s.lastScanOf(hdr.ID); key == in.key {
			s.addNote(hdr.ID, note, true)
			return
		}
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

	events, newRules, compared := s.events(ctx, lf, report, in, page)
	complete := compared
	if compared && s.recordHealth != nil && in.health != nil && in.health.RuleSetDigest == "" &&
		report.RuleSet != nil && report.Metrics != nil {
		// A legacy snapshot is not compared (events): replace it with this analysis, scored under the current
		// rules, so the regression check resumes for flows nobody analyzes interactively.
		s.recordHealth(ctx, doc, report)
	}
	for _, ev := range events {
		// Every channel, retry and replica sends the inbox row's event id.
		ev.ID = notify.EventID(alertRowID(lf.ID, ev))
		if !s.shouldAlert(lf.ID, ev) {
			continue
		}
		// Persist to the in-app inbox; the system context bypasses RLS.
		inserted, err := s.recordAlert(ctx, lf, ev)
		if err != nil {
			// Not in the inbox, so not delivered: the next sweep scans the flow
			// again and retries.
			s.forgetAlert(lf.ID, ev)
			complete = false
			continue
		}
		if !inserted {
			// Already in the inbox, so already delivered when it was recorded:
			// by this process before a restart, or by another replica.
			continue
		}
		// Ping connected clients who can see this flow so the bell updates.
		s.pushAlertSSE(ctx, lf, ev)
		s.notifier.Dispatch(ctx, ev)
		// The flow's org ALSO routes to its own configured destinations.
		s.dispatchOrgChannels(ctx, lf, ev)
	}
	note := s.newRulesNote(lf, report.RuleSet, newRules)
	s.addNote(lf.ID, note, false)
	if complete {
		s.setLastScan(lf.ID, in.key, note)
	}
}

// scanInputs holds one flow's non-analysis inputs (baseline, last health).
// key identifies the FULL input set — flow ID/version/owner/org, the org's
// rule-set digest, the baseline's keys and recorded rule set, the last health
// score and its rule set — so equal keys mean an identical analysis and the
// flow is skipped; key "" never skips or records (unreadable input or no
// digest wired).
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
		field(in.baseline.RuleSetDigest)
	}
	if in.health == nil {
		field("no-health")
	} else {
		fmt.Fprintf(h, "health:%d;", in.health.HealthScore)
		field(in.health.RuleSetDigest)
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
	key, _ := s.lastScanOf(flowID)
	return key
}

// lastScanOf returns flowID's last complete scan: its key and its note.
func (s *Scanner) lastScanOf(flowID string) (string, *noticeNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastScan[flowID], s.lastNotes[flowID]
}

// setLastScanKey records key as flowID's last complete scan, without a note;
// "" forgets it.
func (s *Scanner) setLastScanKey(flowID, key string) { s.setLastScan(flowID, key, nil) }

// setLastScan records key and note (nil: none) as flowID's last complete
// scan; key "" forgets it.
func (s *Scanner) setLastScan(flowID, key string, note *noticeNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		delete(s.lastScan, flowID)
		delete(s.lastNotes, flowID)
		return
	}
	s.lastScan[flowID] = key
	if note == nil {
		delete(s.lastNotes, flowID)
		return
	}
	s.lastNotes[flowID] = note
}

// DigestRuleSet digests an analysis configuration for RuleSetDigestFunc: analyzer.ProfileDigest, the digest the
// analyzer's cache keys carry, so any change of rules, their configuration or their settings counts.
func DigestRuleSet(settings *models.AppSettings, rules []analyzer.Rule) string {
	return analyzer.ProfileDigest(settings, rules)
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

// recordAlert writes a governance event to the in-app alert store and reports
// whether it is new there. The ID derives from (flowID|eventType|signature) so
// a re-alert reuses the same row (ON CONFLICT DO NOTHING); one acknowledgement
// sticks, and one delivery: only an inserted alert is dispatched.
func (s *Scanner) recordAlert(ctx context.Context, lf *storageif.FlowDocument, ev notify.Event) (bool, error) {
	alert := &storageif.GovernanceAlert{
		ID:          alertRowID(lf.ID, ev),
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
	inserted, err := s.backend.InsertGovernanceAlert(ctx, alert)
	if err != nil {
		logger.Warn("scanner: persist governance alert failed", "flowId", lf.ID, "type", ev.Type, "err", err)
	}
	return inserted, err
}

// alertRowID is the inbox row of a flow's event: (flowID|type|signature), so a re-alert reuses the row.
func alertRowID(flowID string, ev notify.Event) string {
	return fmt.Sprintf("%s|%s|%s", flowID, ev.Type, signature(ev))
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
// since the accepted baseline) and health regression (health below last
// recorded), over the rules the baseline knew and the rules the snapshot's
// score counted (unscoredRules). newRules are
// the rules newer than either that report findings (the flow's part of a
// "new rules" notice); preview rules raise nothing (ADR 0016). compared is
// false when a recorded rule set could not be read: nothing is raised then.
func (s *Scanner) events(ctx context.Context, lf *storageif.FlowDocument, report *models.AnalysisReport, in scanInputs, page pageInputs) (events []notify.Event, newRules []string, compared bool) {
	name := lf.Name

	// Drift vs. accepted baseline. Only meaningful once a baseline exists;
	// otherwise every finding is "new" and would alert on the first scan.
	if bl := in.baseline; bl != nil {
		keys := bl.Keys
		if keys == nil {
			keys = []string{}
		}
		if drift := analyzer.ComputeDrift(lf.ID, report, keys); len(drift.New) > 0 {
			recorded, ok := s.recordedRuleSet(ctx, page, bl.RuleSetDigest, bl.RuleSet, report.RuleSet)
			if !ok {
				return nil, nil, false
			}
			split := analyzer.SplitDrift(drift.New, analyzer.NewRuleAge(recorded, report.RuleSet))
			newRules = append(newRules, split.NewRuleIDs...)
			if len(split.Drift) > 0 {
				c := analyzer.CountGating(split.Drift)
				events = append(events, notify.Event{
					At:          time.Now().UTC(),
					Type:        notify.EventDrift,
					FlowID:      lf.ID,
					FlowName:    name,
					Title:       fmt.Sprintf("New findings in %s", flowLabel(lf)),
					Message:     fmt.Sprintf("%d new finding(s) since baseline (%d error, %d warning).", len(split.Drift), c.Errors, c.Warnings),
					NewErrors:   c.Errors,
					NewWarnings: c.Warnings,
				})
			}
		}
	}

	// Health regression vs. last recorded snapshot. Requires metrics (a parsed,
	// analyzable flow) and a prior snapshot to compare against. The current
	// score leaves out the rules the snapshot's score did not count: a rule it
	// did not have lowers the score without the flow having changed. A legacy
	// snapshot (no rule set: recorded before rule sets, or rewritten by the
	// previous release) is not compared at all: its release graded severities
	// differently (Phase 4a: hardcoded-credential's default, parse errors and
	// tainted SQL under settings) without moving a fingerprint or a revision, so
	// every unchanged flow would "regress" on the first sweep. The sweep records
	// this analysis as the flow's snapshot instead (SetHealthRecorder), so the
	// next change is compared like for like; its new-rule findings still join
	// the notice.
	if prior := in.health; report.Metrics != nil && prior != nil {
		legacySnapshot := prior.RuleSetDigest == "" && report.RuleSet != nil
		full, cur := report.Metrics.HealthScore, report.Metrics.HealthScore
		if rs := report.RuleSet; rs != nil && prior.RuleSetDigest != rs.Digest {
			recorded, ok := s.recordedRuleSet(ctx, page, prior.RuleSetDigest, nil, rs)
			if !ok {
				return nil, nil, false
			}
			newRules = append(newRules, analyzer.SplitDrift(report.Findings, analyzer.NewRuleAge(recorded, rs)).NewRuleIDs...)
			if unscored := unscoredRules(recorded, rs); len(unscored) > 0 {
				kept := make([]models.Finding, 0, len(report.Findings))
				for _, f := range report.Findings {
					if !unscored[f.RuleID] {
						kept = append(kept, f)
					}
				}
				if len(kept) < len(report.Findings) {
					cur = analyzer.HealthOf(kept, report.Metrics)
				}
			}
		}
		if cur < prior.HealthScore && !legacySnapshot {
			msg := fmt.Sprintf("Health score dropped from %d to %d.", prior.HealthScore, cur)
			if full != cur {
				msg = fmt.Sprintf("Health score dropped from %d to %d over the rules it was last recorded with (%d with the newer ones).", prior.HealthScore, cur, full)
			}
			events = append(events, notify.Event{
				At:          time.Now().UTC(),
				Type:        notify.EventHealthRegression,
				FlowID:      lf.ID,
				FlowName:    name,
				Title:       fmt.Sprintf("Health regressed in %s", flowLabel(lf)),
				Message:     msg,
				HealthScore: cur,
				PrevHealth:  prior.HealthScore,
			})
		}
	}
	return events, newRules, true
}

// unscoredRules lists the current rules whose findings a snapshot's score did
// not count, for the like-for-like regression check: those the recorded set
// lacks by ID, or has as a preview rule. A revised rule or an edited custom
// rule is not among them: the snapshot counted its earlier revision, and
// leaving it out of the current score only would hide a regression by up to
// that penalty. A legacy record (nil, or rules that do not hash to its digest)
// counted every custom rule and every built-in of rule-set version 1. Two
// refs under one ID are unscored only if both are. nil when all count.
func unscoredRules(recorded, current *models.RuleSet) map[string]bool {
	if current == nil || (recorded != nil && recorded.Digest == current.Digest) {
		return nil
	}
	legacy := recorded == nil || analyzer.NewRuleSet(recorded.Rules).Digest != recorded.Digest
	inRecord := map[string]bool{}
	if !legacy {
		for _, r := range recorded.Rules {
			if r.Stage != string(analyzer.StagePreview) {
				inRecord[r.ID] = true
			}
		}
	}
	counted := make(map[string]bool, len(current.Rules))
	for _, ref := range current.Rules {
		c := inRecord[ref.ID]
		if legacy {
			m, builtin := analyzer.LookupMeta(ref.ID)
			c = analyzer.KnownToLegacyRecords(ref) || (builtin && m.IntroducedIn <= 1)
		}
		counted[ref.ID] = counted[ref.ID] || c
	}
	var out map[string]bool
	for id, c := range counted {
		if !c {
			if out == nil {
				out = map[string]bool{}
			}
			out[id] = true
		}
	}
	return out
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

// forgetAlert undoes shouldAlert for an alert that could not be recorded, so
// the next sweep raises it again.
func (s *Scanner) forgetAlert(flowID string, ev notify.Event) {
	key := flowID + "|" + string(ev.Type)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSig[key] == signature(ev) {
		delete(s.lastSig, key)
	}
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
	if lf.OrganizationID == "" {
		return
	}
	s.deliverToOrg(ctx, lf.OrganizationID, ev)
}

// deliverToOrg hands ev to each of the org's channels on the channel runner. At the runner's cap the sweep waits for
// a slot (honouring its context, so shutdown stops queueing) instead of spawning a goroutine per alert; the delivery
// itself is detached from the sweep, and Stop waits for it.
func (s *Scanner) deliverToOrg(ctx context.Context, orgID string, ev notify.Event) {
	notifiers, ok := s.notifiersForOrg(ctx, orgID)
	if !ok {
		return
	}
	// One id and time for every channel of the event.
	ev = ev.Stamped(time.Now())
	for _, n := range notifiers {
		// A channel subscribed to other events would take a delivery slot for nothing.
		if !notify.Receives(n, ev.Type) {
			continue
		}
		s.deliveryWG.Add(1)
		if !s.channels.Enqueue(ctx, n, ev, s.deliveryWG.Done) {
			return
		}
	}
}

// notifiersForOrg returns the org's built notifiers, at most once per sweep.
// The BUILD is cached, not just the channel rows: one build per event meant a
// fresh validation lookup per alert.
func (s *Scanner) notifiersForOrg(ctx context.Context, orgID string) ([]notify.AlertNotifier, bool) {
	if s.channels == nil || s.backend == nil {
		return nil, false
	}
	s.orgChMu.Lock()
	cached, ok := s.orgNotifiers[orgID]
	s.orgChMu.Unlock()
	if ok {
		return cached, true
	}

	built, err := s.channels.OrgNotifiers(ctx, orgID)
	if err != nil {
		logger.Warn("scanner: list org channels failed", "orgId", orgID, "err", err)
		return nil, false
	}

	s.orgChMu.Lock()
	if s.orgNotifiers == nil {
		s.orgNotifiers = make(map[string][]notify.AlertNotifier)
	}
	s.orgNotifiers[orgID] = built
	s.orgChMu.Unlock()
	return built, true
}
