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
		if auditClosed.Load() {
			// Pool is shutting down: drop rather than spawn an unbounded number
			// of detached goroutines as requests drain.
			slog.Warn("audit pool closed, dropping event", "action", action)
			return
		}
		select {
		case auditCh <- event:
		default:
			slog.Warn("audit pool full, dropping event", "action", action)
		}
		return
	}
	// Pool was never initialized (e.g. local mode / tests): best-effort save.
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
