package api

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

const (
	auditQueueSize = 256
	auditWorkers   = 8
	auditWriteTTL  = 5 * time.Second

	// Batching knobs: the worker flushes on the first of (auditFlushSize reached | auditFlushDelay
	// elapsed | shutdown) — one multi-row INSERT per batch, so the 256-buffer rarely fills and drops.
	auditFlushSize    = 64
	auditFlushDelay   = 100 * time.Millisecond
	auditWriteRetries = 1 // bounded retry on a transient flush failure
	auditRetryDelay   = 200 * time.Millisecond

	AuditActionLogin           = "auth.login"
	AuditActionLogout          = "auth.logout"
	AuditActionRegister        = "auth.register"
	AuditActionPasswordChange  = "auth.password_change"
	AuditActionRoleChange      = "admin.role_change"
	AuditActionFlowUpload      = "flow.upload"
	AuditActionFlowAnalyze     = "flow.analyze"
	AuditActionFlowExport      = "flow.export"
	AuditActionFlowDelete      = "flow.delete"
	AuditActionFlowShare       = "flow.share"
	AuditActionFlowSave        = "flow.save"
	AuditActionFlowVersion     = "flow.version_save"
	AuditActionFlowRestore     = "flow.version_restore"
	AuditActionFindingTriage   = "finding.triage"
	AuditActionBaselineSet     = "flow.baseline_set"
	AuditActionBaselineClear   = "flow.baseline_clear"
	AuditActionChatStream      = "chat.stream"
	AuditActionSettingsChange  = "settings.change"
	AuditActionKeysSave        = "keys.save"
	AuditActionOrgInviteCreate = "org.invite_create"
	AuditActionOrgInviteRevoke = "org.invite_revoke"
	AuditActionOrgInviteAccept = "org.invite_accept"
	AuditActionOrgMemberAdd    = "org.member_add"
	AuditActionOrgMemberRemove = "org.member_remove"
	AuditActionOrgMemberRole   = "org.member_role"
	AuditActionProfileUpdate   = "user.profile_update"
	AuditActionAccountDelete   = "user.account_delete"
	AuditActionDataExport      = "user.data_export"
	AuditActionSessionRevoke   = "auth.session_revoke"
	AuditActionSSOLogin        = "auth.sso_login"
	AuditActionLoginFailure    = "auth.login_failure"
	AuditActionAccountLock     = "auth.account_lock"
	AuditActionTokenCreate     = "token.create"
	AuditActionTokenRevoke     = "token.revoke"
	AuditActionMigrationStart  = "admin.migration_start"
	// Added by the feature-completeness audit: these mutations previously left no trail at all
	// (share links and policy rewrites being the highest blast-radius non-admin actions).
	AuditActionCommentAdd       = "finding.comment_add"
	AuditActionCommentDelete    = "finding.comment_delete"
	AuditActionShareLinkCreate  = "flow.share_link_create"
	AuditActionShareLinkRevoke  = "flow.share_link_revoke"
	AuditActionPolicySave       = "policy.save"
	AuditActionPolicyDelete     = "policy.delete"
	AuditActionKnowledgeUpload  = "org.knowledge_upload"
	AuditActionKnowledgeDelete  = "org.knowledge_delete"
	AuditActionKnowledgeReindex = "org.knowledge_reindex"
	AuditActionOrgCreate        = "org.create"
	AuditActionOrgUpdate        = "org.update"
	AuditActionOrgDelete        = "org.delete"
	AuditActionScanNow          = "admin.scan_now"
	AuditActionAlertDismiss     = "governance.alert_dismiss"
	AuditActionAlertClear       = "governance.alert_clear"
	// These four were emitted as raw strings before; the values are kept so existing rows stay filterable.
	AuditActionOrgChannelSave   = "org_channel_save"
	AuditActionOrgChannelDelete = "org_channel_delete"
	AuditActionOrgRuleSave      = "org_rule_save"
	AuditActionOrgRuleDelete    = "org_rule_delete"
	AuditActionSSOLink          = "auth.sso_link"
	AuditActionSSOUnlink        = "auth.sso_unlink"
	// AuditActionProvidersTest: an org admin's billed test of the organisation's shared provider
	// credential (the per-user test of one's own key is not audited).
	AuditActionProvidersTest = "providers.test"
	// The Azure AI Foundry model registry: a resource or model created/updated/removed — the
	// caller's own (azure_connection / azure_model) or an organisation's (org). Meta never a secret.
	AuditActionAzureConnectionSave   = "azure.connection_save"
	AuditActionAzureConnectionDelete = "azure.connection_delete"
	AuditActionAzureModelSave        = "azure.model_save"
	AuditActionAzureModelDelete      = "azure.model_delete"
)

// AllAuditActions is the closed set of audit action strings the handlers emit; the admin audit UI
// builds its filter from it, so a new constant must be added here too (audit_actions_test.go enforces).
func AllAuditActions() []string {
	return []string{
		AuditActionLogin, AuditActionLogout, AuditActionRegister, AuditActionPasswordChange,
		AuditActionRoleChange, AuditActionFlowUpload, AuditActionFlowAnalyze, AuditActionFlowExport,
		AuditActionFlowDelete, AuditActionFlowShare, AuditActionFlowSave, AuditActionFlowVersion,
		AuditActionFlowRestore, AuditActionFindingTriage, AuditActionBaselineSet, AuditActionBaselineClear,
		AuditActionChatStream, AuditActionSettingsChange, AuditActionKeysSave, AuditActionOrgInviteCreate,
		AuditActionOrgInviteRevoke, AuditActionOrgInviteAccept, AuditActionOrgMemberAdd,
		AuditActionOrgMemberRemove, AuditActionOrgMemberRole, AuditActionProfileUpdate,
		AuditActionAccountDelete, AuditActionDataExport, AuditActionSessionRevoke, AuditActionSSOLogin,
		AuditActionLoginFailure, AuditActionAccountLock, AuditActionTokenCreate, AuditActionTokenRevoke,
		AuditActionMigrationStart, AuditActionCommentAdd, AuditActionCommentDelete,
		AuditActionShareLinkCreate, AuditActionShareLinkRevoke, AuditActionPolicySave,
		AuditActionPolicyDelete, AuditActionKnowledgeUpload, AuditActionKnowledgeDelete,
		AuditActionKnowledgeReindex, AuditActionOrgCreate, AuditActionOrgUpdate, AuditActionOrgDelete,
		AuditActionScanNow, AuditActionAlertDismiss, AuditActionAlertClear, AuditActionOrgChannelSave,
		AuditActionOrgChannelDelete, AuditActionOrgRuleSave, AuditActionOrgRuleDelete,
		AuditActionSSOLink, AuditActionSSOUnlink, AuditActionProvidersTest,
		AuditActionAzureConnectionSave, AuditActionAzureConnectionDelete,
		AuditActionAzureModelSave, AuditActionAzureModelDelete,
	}
}

var (
	auditCh     chan *storageif.AuditEvent
	auditWg     sync.WaitGroup
	auditOnce   sync.Once
	auditClosed atomic.Bool // guards against send-on-closed-channel panic

	// auditSpiller is the optional on-disk overflow sink for events that don't fit the in-memory
	// pool; configured via SetAuditSpillConfig before InitAuditPool, nil keeps the log fallback.
	auditSpiller *auditSpillStore
	// spillStopCh signals auditSpillReaper to exit; spillReaperWg waits for it.
	spillStopCh   chan struct{}
	spillReaperWg sync.WaitGroup
)

// SetAuditSpillConfig enables the on-disk spill queue for overflow events (dir "" → temp dir;
// maxBytes <=0 → 10 MB); call before InitAuditPool. A reaper drains spilled events back once the
// pool has capacity, so only overload beyond the spill cap degrades.
func SetAuditSpillConfig(dir string, maxBytes int64) error {
	s, err := newAuditSpiller(dir, maxBytes)
	if err != nil {
		return err
	}
	auditSpiller = s
	return nil
}

func InitAuditPool(backend storageif.StorageBackend) {
	if backend == nil {
		return
	}
	auditOnce.Do(func() {
		auditCh = make(chan *storageif.AuditEvent, auditQueueSize)
		auditWg.Add(auditWorkers)
		for i := 0; i < auditWorkers; i++ {
			go auditWorker(backend)
		}
		// If a spill store is configured, drain it back into the pool whenever
		// capacity returns. This recovers events a burst pushed past the
		// in-memory buffer (and any left from a previous boot).
		if auditSpiller != nil {
			spillStopCh = make(chan struct{})
			spillReaperWg.Add(1)
			go auditSpillReaper()
		}
	})
}

// auditShutdownBudget bounds how long ShutdownAuditPool waits for the workers to flush: an
// unbounded wait once blew past fx's StopTimeout and the process was SIGKILLed mid-shutdown.
// Events still queued at expiry are dropped with a warning — the lesser harm. A var for tests.
var auditShutdownBudget = 5 * time.Second

func ShutdownAuditPool() {
	if auditCh == nil {
		return
	}
	auditClosed.Store(true)
	close(auditCh)
	if !waitWithBudget(&auditWg, auditShutdownBudget) {
		slog.Warn("audit: workers did not drain within the shutdown budget; remaining queued events are dropped", "budget", auditShutdownBudget)
		return
	}
	// Stop the reaper AFTER the workers have drained, so a final sweep can push
	// any remaining spill into the closing channel (no-op if nothing's left).
	if spillStopCh != nil {
		close(spillStopCh)
		if !waitWithBudget(&spillReaperWg, auditShutdownBudget) {
			slog.Warn("audit: spill reaper did not stop within the shutdown budget", "budget", auditShutdownBudget)
		}
	}
}

// waitWithBudget waits for wg for at most d. Returns false on timeout; the
// helper goroutine it leaves behind exits when wg eventually completes.
func waitWithBudget(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// auditEnqueue attempts to deliver event to the audit worker pool: (true, "") enqueued,
// (false, "closed") when shutting down, (false, "full") when the bounded queue is full. The
// atomic flag is a fast path; the recover closes the residual TOCTOU window of a send racing close.
func auditEnqueue(event *storageif.AuditEvent) (sent bool, reason string) {
	defer func() {
		if r := recover(); r != nil {
			sent, reason = false, "closed"
		}
	}()
	if auditClosed.Load() {
		return false, "closed"
	}
	select {
	case auditCh <- event:
		return true, ""
	default:
		return false, "full"
	}
}

// auditBatchSink is an optional StorageBackend capability: persisting a batch of audit events in
// one round-trip. Postgres implements it; backends that don't fall back to per-event writes.
type auditBatchSink interface {
	SaveAuditEvents(ctx context.Context, events []*storageif.AuditEvent) error
}

// auditWriter is the narrow write capability the audit worker needs, declared separately so tests
// can supply a minimal fake without implementing the whole StorageBackend interface.
type auditWriter interface {
	SaveAuditEvent(ctx context.Context, event *storageif.AuditEvent) error
}

func auditWorker(backend storageif.StorageBackend) {
	defer auditWg.Done()
	batchSink, _ := backend.(auditBatchSink)
	var buf []*storageif.AuditEvent

	// On an unrecoverable panic outside the flush path, divert buffered-but-unflushed events to
	// the fallback sink rather than dropping them (events mid-write are salvaged by the flush-local
	// recover below).
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("audit worker panicked, salvaging buffered events", "err", r, "buffered", len(buf))
			salvageAuditBatch(backend, buf)
		}
	}()

	var timer *time.Timer
	var timerC <-chan time.Time

	flush := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
		if len(buf) == 0 {
			return
		}
		batch := buf
		buf = nil
		// A panic inside the write path would otherwise lose the whole in-flight batch and kill
		// the worker: isolate the batch per-event and let the worker keep running.
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("audit flush panicked, isolating in-flight batch", "err", r, "events", len(batch))
				salvageAuditBatch(backend, batch)
			}
		}()
		writeAuditBatch(backend, batchSink, batch)
	}
	for {
		select {
		case event, ok := <-auditCh:
			if !ok {
				flush() // drain the accumulated batch on shutdown
				return
			}
			buf = append(buf, event)
			if len(buf) >= auditFlushSize {
				flush()
			} else if timer == nil {
				timer = time.NewTimer(auditFlushDelay)
				timerC = timer.C
			}
		case <-timerC:
			flush()
		}
	}
}

// auditSpillReaper drains the on-disk spill queue back into the pool whenever there is capacity,
// on a 500 ms tick (sub-second replay latency without contending with the hot write path).
func auditSpillReaper() {
	defer spillReaperWg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-spillStopCh:
			return
		case <-ticker.C:
			replaySpilled()
		}
	}
}

// replaySpilled moves drained spill events into the pool, only while it has ample room (< half
// full) so replay never starves fresh events. A partial-batch failure re-spills the unconsumed
// remainder (without double-counting the metric) and yields.
func replaySpilled() {
	if auditSpiller == nil {
		return
	}
	for {
		if len(auditCh) >= auditQueueSize/2 {
			return // leave headroom for fresh events
		}
		batch := auditSpiller.Drain(auditFlushSize)
		if len(batch) == 0 {
			return
		}
		var failed []*storageif.AuditEvent
		for _, e := range batch {
			if sent, _ := auditEnqueue(e); !sent {
				failed = append(failed, e)
			} else {
				metrics.RecordAuditSpillReplayed()
			}
		}
		if len(failed) > 0 {
			// Pool filled/closed mid-batch — put the unconsumed remainder back
			// (reSpill doesn't bump the spilled metric) and yield this tick.
			auditSpiller.reSpill(failed)
			return
		}
	}
}

// writeAuditBatch persists a batch with a bounded retry: a single batched write when the backend
// supports it, falling back to per-event writes so a bad row can't poison the batch. Events still
// failing after retry go to the log fallback sink (pad_audit_dropped_total{reason="write_failed"}).
func writeAuditBatch(backend auditWriter, sink auditBatchSink, events []*storageif.AuditEvent) {
	if sink != nil {
		if err := saveAuditBatchWithRetry(sink, events); err == nil {
			return
		}
		// Batch failed (likely one bad row) — isolate via per-event writes below.
	}
	for _, e := range events {
		writeOneAuditEvent(backend, e)
	}
}

// saveAuditBatchWithRetry attempts the batched INSERT up to auditWriteRetries+1
// times with a short backoff between attempts.
func saveAuditBatchWithRetry(sink auditBatchSink, events []*storageif.AuditEvent) error {
	var lastErr error
	for attempt := 0; attempt <= auditWriteRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTTL)
		err := sink.SaveAuditEvents(ctx, events)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < auditWriteRetries {
			time.Sleep(auditRetryDelay)
		}
	}
	return lastErr
}

// writeOneAuditEvent saves a single event with one retry, diverting to the log
// fallback sink (and bumping the drop metric) on persistent failure.
func writeOneAuditEvent(backend auditWriter, e *storageif.AuditEvent) {
	save := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTTL)
		defer cancel()
		return backend.SaveAuditEvent(ctx, e)
	}
	if err := save(); err != nil {
		time.Sleep(auditRetryDelay)
		if err := save(); err != nil {
			slog.Warn("audit log write failed after retry", "action", e.Action, "err", err)
			auditFallback("write_failed", e)
		}
	}
}

// auditFallback emits an event to the structured-log sink when it can't be enqueued to the DB pool,
// keeping it discoverable (pair with pad_audit_dropped_total). PII is REDACTED here: container logs
// are more broadly readable than the audit table, and a DB outage would otherwise dump every
// event's email/IP/meta into them — masked values plus user_id keep the event attributable.
func auditFallback(reason string, event *storageif.AuditEvent) {
	metrics.RecordAuditDropped(reason)
	slog.Error("audit event diverted to log fallback sink",
		"reason", reason,
		"audit_id", event.ID,
		"user_id", event.UserID,
		"email", redactEmail(event.Email),
		"action", event.Action,
		"resource_type", event.ResourceType,
		"resource_id", event.ResourceID,
		"ip", redactIP(event.IP),
		// Meta values may carry free-form PII; the keys alone say what KIND of change happened.
		"meta_keys", metaKeys(event.Meta),
		"created_at", event.CreatedAt,
	)
}

// redactEmail keeps the first character of the local part and the full domain — enough to
// correlate log lines with a known account without exposing the address itself.
func redactEmail(email string) string {
	if email == "" {
		return ""
	}
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// redactIP keeps the network prefix and masks the host part ("203.0.113.7" → "203.0.x.x") —
// enough to distinguish "office vs unknown network" without logging the full address.
func redactIP(ip string) string {
	if ip == "" {
		return ""
	}
	if parts := strings.Split(ip, "."); len(parts) == 4 {
		return parts[0] + "." + parts[1] + ".x.x"
	}
	if i := strings.IndexByte(ip, ':'); i > 0 {
		return ip[:i] + ":…"
	}
	return "***"
}

// metaKeys returns the sorted key set of an audit meta map (values omitted — free-form, may carry PII).
func metaKeys(meta map[string]string) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// salvageAuditBatch persists events that were in flight when the worker hit a panic: each written
// independently with its own panic guard (a re-panic can't abort the salvage), failures to auditFallback.
func salvageAuditBatch(backend auditWriter, events []*storageif.AuditEvent) {
	for _, e := range events {
		func() {
			defer func() {
				if r := recover(); r != nil {
					auditFallback("salvage_failed", e)
				}
			}()
			writeOneAuditEvent(backend, e)
		}()
	}
}

func logAudit(ctx context.Context, backend storageif.StorageBackend, r *http.Request, trustedProxies []string, action, resourceType, resourceID string, meta map[string]string) {
	if backend == nil {
		return
	}
	userID := ""
	email := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
		email = claims.Email
	}
	ip := middleware.ClientIP(r, trustedProxies)
	event := &storageif.AuditEvent{
		ID:           uuid.NewString(),
		UserID:       userID,
		Email:        email,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		IP:           ip,
		Meta:         meta,
		CreatedAt:    time.Now().UTC(),
	}
	if auditCh != nil {
		if _, reason := auditEnqueue(event); reason != "" {
			// On "full", spill to the durable on-disk queue first so a transient overload can be
			// replayed; only divert to the log fallback with no spill store or at its cap.
			// "closed" (shutting down) always goes to the fallback — the reaper is stopping too.
			if reason == "full" && auditSpiller != nil && auditSpiller.Spill(event) {
				return
			}
			auditFallback(reason, event)
		}
		return
	}
	// Pool was never initialized (e.g. local mode / tests): best-effort save.
	// #nosec G118 -- detached best-effort audit write; must not be tied to request ctx.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("audit fallback goroutine panicked", "action", action, "err", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTTL)
		defer cancel()
		if err := backend.SaveAuditEvent(ctx, event); err != nil {
			slog.Warn("audit log write failed", "action", action, "err", err)
		}
	}()
}
