package api

import (
	"context"
	"log/slog"
	"net/http"
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

func auditWorker(backend storageif.StorageBackend) {
	defer auditWg.Done()
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("audit worker panicked", "err", r)
		}
	}()
	for event := range auditCh {
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTTL)
		if err := backend.SaveAuditEvent(ctx, event); err != nil {
			slog.Warn("audit log write failed", "action", event.Action, "err", err)
		}
		cancel()
	}
}

// auditFallback emits an event to the structured-log sink (stderr → container
// logs → Log Analytics) when it can't be enqueued to the DB pool. This keeps the
// event discoverable instead of silently lost; pair with the pad_audit_dropped_total
// metric to alert when the DB sink falls behind.
func auditFallback(reason string, event *storageif.AuditEvent) {
	metrics.RecordAuditDropped(reason)
	slog.Error("audit event diverted to log fallback sink",
		"reason", reason,
		"audit_id", event.ID,
		"user_id", event.UserID,
		"email", event.Email,
		"action", event.Action,
		"resource_type", event.ResourceType,
		"resource_id", event.ResourceID,
		"ip", event.IP,
		"meta", event.Meta,
		"created_at", event.CreatedAt,
	)
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
