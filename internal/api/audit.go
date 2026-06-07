package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Audit action constants — keep them stable; admin UIs and SIEM rules filter by these.
const (
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
)

// logAudit records an event to the backend in a background goroutine so it
// never blocks the request path. Failures are logged but not surfaced.
func logAudit(ctx context.Context, backend storageif.StorageBackend, r *http.Request, action, resourceType, resourceID string, meta map[string]string) {
	if backend == nil {
		return
	}
	userID := ""
	email := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
		email = claims.Email
	}
	ip := clientIP(r)
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
	go func() {
		if err := backend.SaveAuditEvent(context.Background(), event); err != nil {
			slog.Warn("audit log write failed", "action", action, "err", err)
		}
	}()
}

func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.SplitN(xff, ",", 2)[0]
	}
	host := r.RemoteAddr
	if colon := strings.LastIndex(host, ":"); colon != -1 {
		host = host[:colon]
	}
	return host
}
