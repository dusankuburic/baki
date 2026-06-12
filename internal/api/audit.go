package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
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

	AuditActionLogin          = "auth.login"
	AuditActionLogout         = "auth.logout"
	AuditActionRegister       = "auth.register"
	AuditActionPasswordChange = "auth.password_change"
	AuditActionRoleChange     = "admin.role_change"
	AuditActionFlowUpload     = "flow.upload"
	AuditActionFlowAnalyze    = "flow.analyze"
	AuditActionFlowExport     = "flow.export"
	AuditActionFlowDelete     = "flow.delete"
	AuditActionFlowShare      = "flow.share"
	AuditActionFlowSave       = "flow.save"
	AuditActionFlowVersion    = "flow.version_save"
	AuditActionChatStream     = "chat.stream"
	AuditActionSettingsChange = "settings.change"
	AuditActionKeysSave       = "keys.save"
	AuditActionOrgInviteCreate = "org.invite_create"
	AuditActionOrgInviteRevoke = "org.invite_revoke"
	AuditActionOrgInviteAccept = "org.invite_accept"
	AuditActionProfileUpdate   = "user.profile_update"
	AuditActionSessionRevoke   = "auth.session_revoke"
)

var (
	auditCh   chan *storageif.AuditEvent
	auditWg   sync.WaitGroup
	auditOnce sync.Once
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
	close(auditCh)
	auditWg.Wait()
}

func auditWorker(backend storageif.StorageBackend) {
	defer auditWg.Done()
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
		select {
		case auditCh <- event:
		default:
			slog.Warn("audit pool full, dropping event", "action", action)
		}
		return
	}
	go func() {
		if err := backend.SaveAuditEvent(context.Background(), event); err != nil {
			slog.Warn("audit log write failed", "action", action, "err", err)
		}
	}()
}
