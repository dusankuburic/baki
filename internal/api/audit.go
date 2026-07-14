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

	// Batching knobs. The worker accumulates events and flushes on the first of
	// (auditFlushSize reached | auditFlushDelay elapsed | shutdown). A single
	// multi-row INSERT then persists the batch in one round-trip, multiplying
	// throughput so the 256-buffer is far less likely to fill and drop.
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
)

var (
	auditCh     chan *storageif.AuditEvent
	auditWg     sync.WaitGroup
	auditOnce   sync.Once
	auditClosed atomic.Bool // guards against send-on-closed-channel panic
)

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
	})
}

func ShutdownAuditPool() {
	if auditCh == nil {
		return
	}
	auditClosed.Store(true)
	close(auditCh)
	auditWg.Wait()
}

// auditEnqueue attempts to deliver event to the audit worker pool. Returns
// (true, "") when enqueued. When the pool is shutting down it returns
// (false, "closed"); when the bounded queue is full it returns (false, "full").
//
// It is safe to call concurrently with ShutdownAuditPool: a send that races
// with close() is recovered rather than panicking. The atomic flag above is a
// fast path — ShutdownAuditPool sets the flag and closes the channel as two
// separate statements, so a goroutine that observed auditClosed == false just
// before the close() would otherwise panic on the send. The recover closes that
// residual TOCTOU window.
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

// auditBatchSink is an optional capability of StorageBackend: persisting a
// batch of audit events in one round-trip. The Postgres backend implements it;
// backends that don't (filesystem, fakes) fall back to per-event writes.
type auditBatchSink interface {
	SaveAuditEvents(ctx context.Context, events []*storageif.AuditEvent) error
}

// auditWriter is the narrow write capability the audit worker needs. Declared
// separately so the worker depends only on what it uses and tests can supply a
// minimal fake without implementing the whole StorageBackend interface.
type auditWriter interface {
	SaveAuditEvent(ctx context.Context, event *storageif.AuditEvent) error
}

func auditWorker(backend storageif.StorageBackend) {
	defer auditWg.Done()
	batchSink, _ := backend.(auditBatchSink)
	var buf []*storageif.AuditEvent

	// On an unrecoverable panic outside the flush path, divert any buffered-but-
	// unflushed events to the fallback sink rather than dropping them silently.
	// (Events mid-write are salvaged by the flush-local recover below.)
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
		// A panic inside the write path (e.g. a driver-level fault) would
		// otherwise lose the whole in-flight batch and kill the worker.
		// Isolate the batch per-event and let the worker keep running.
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

// writeAuditBatch persists a batch with a bounded retry. It prefers a single
// batched write (when the backend supports it); on any failure it falls back to
// per-event writes so a single bad row can't poison the whole batch. Events that
// still fail after retry are diverted to the log fallback sink and metered, so a
// DB hiccup is visible (pad_audit_dropped_total{reason="write_failed"}) rather
// than silently lost.
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

// auditFallback emits an event to the structured-log sink (stderr → container
// logs → Log Analytics) when it can't be enqueued to the DB pool. This keeps the
// event discoverable instead of silently lost; pair with the pad_audit_dropped_total
// metric to alert when the DB sink falls behind.
//
// PII is REDACTED here: container logs are far more broadly readable (and
// retained/indexed differently) than the access-controlled audit table, and a
// DB outage would otherwise dump every event's email/IP/meta into them. The
// user_id plus masked email/IP keep the event attributable — an operator can
// resolve the full identity through the users table — without the raw values
// living in log storage.
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
		// Meta values may carry free-form PII (names, emails, target ids
		// typed by users); the keys alone say what KIND of change happened.
		"meta_keys", metaKeys(event.Meta),
		"created_at", event.CreatedAt,
	)
}

// redactEmail keeps the first character of the local part and the full domain
// ("dusan@example.com" → "d***@example.com"): enough to correlate log lines
// with a known account without exposing the address itself.
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

// redactIP keeps the network prefix and masks the host part: the first two
// groups of an IPv4 ("203.0.113.7" → "203.0.x.x") or the first segment of an
// IPv6 — enough to distinguish "office vs unknown network" in an incident
// without logging the full client address.
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

// metaKeys returns the sorted key set of an audit meta map (values omitted —
// they are free-form and may carry PII).
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

// salvageAuditBatch persists events that were in flight when the worker hit a
// panic. Each event is written independently with its own panic guard: a
// re-panic on the same faulty driver can't abort the salvage, and an event that
// still can't be written is diverted to the log fallback sink (auditFallback)
// so it stays discoverable rather than silently lost.
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
			// "closed" (shutting down) or "full" (pool saturated under load):
			// divert the event to structured logs so it isn't silently dropped
			// (the DB sink is the system of record, container logs are the
			// durable fallback).
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
