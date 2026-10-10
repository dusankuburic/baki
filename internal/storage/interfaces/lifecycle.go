package interfaces

import "time"

// UserDataExport is the data-subject access/portability bundle returned by ExportUserData: the user's own data only; collaborator content on shared flows is out of scope.
type UserDataExport struct {
	User        *User                `json:"user"`
	Flows       []*FlowDocument      `json:"flows"`
	Settings    *AppSettings         `json:"settings"`
	AuditEvents []ExportedAuditEvent `json:"auditEvents"`
	APITokens   []*APIToken          `json:"apiTokens"`
	// Sections added after the endpoint claimed "everything stored about the user": own-flow chat history, authored comments/triage, orgs, sessions.
	Conversations   []*ExportedConversation `json:"conversations"`
	Comments        []*FindingComment       `json:"comments"`
	TriageDecisions []*FindingStatus        `json:"triageDecisions"`
	Organisations   []*Organisation         `json:"organisations"`
	Sessions        []RefreshTokenInfo      `json:"sessions"`
	// AzureRegistry is the user's personal Azure AI Foundry resources and models — metadata only, never a secret.
	AzureRegistry *ExportedAzureRegistry `json:"azureRegistry"`
	ExportedAt    time.Time              `json:"exportedAt"`
}

// ExportedAuditEvent is an AuditEvent with the camelCase keys the bundle uses; AuditEvent stays untagged because the on-disk spill queue persists it as JSON.
type ExportedAuditEvent struct {
	ID           string            `json:"id"`
	UserID       string            `json:"userId"`
	Email        string            `json:"email"`
	Action       string            `json:"action"`
	ResourceType string            `json:"resourceType"`
	ResourceID   string            `json:"resourceId"`
	IP           string            `json:"ip"`
	Meta         map[string]string `json:"meta"`
	CreatedAt    time.Time         `json:"createdAt"`
}

// NewExportedAuditEvents maps stored events onto the export shape.
func NewExportedAuditEvents(events []*AuditEvent) []ExportedAuditEvent {
	out := make([]ExportedAuditEvent, 0, len(events))
	for _, e := range events {
		if e == nil {
			continue
		}
		out = append(out, ExportedAuditEvent{
			ID: e.ID, UserID: e.UserID, Email: e.Email, Action: e.Action,
			ResourceType: e.ResourceType, ResourceID: e.ResourceID, IP: e.IP,
			Meta: e.Meta, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// ExportedConversation is one stored chat conversation on a flow the user owns, keyed by the ConversationStore's (flowId, scope) pair.
type ExportedConversation struct {
	FlowID   string        `json:"flowId"`
	Scope    string        `json:"scope"`
	Messages []ChatMessage `json:"messages"`
}

// PurgeResult reports how many stale rows a PurgeExpiredData run removed.
type PurgeResult struct {
	RefreshTokens       int `json:"refreshTokens"`
	APITokens           int `json:"apiTokens"`
	UserTokens          int `json:"userTokens"`
	OrgInvites          int `json:"orgInvites"`
	AuditEvents         int `json:"auditEvents"`
	FlowAnalysisHistory int `json:"flowAnalysisHistory"`
	UsageMetrics        int `json:"usageMetrics"`
	TokenBlacklist      int `json:"tokenBlacklist"`
	ShareTokens         int `json:"shareTokens"`
	GovAlerts           int `json:"govAlerts"`
	RuleNotices         int `json:"ruleNotices"`
	IdempotencyKeys     int `json:"idempotencyKeys"`
}
