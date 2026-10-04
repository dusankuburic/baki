package interfaces

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	"pad-core/models"
)

// ErrNotFound is returned by backend methods when the requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrEmailExists is returned by CreateUser for a duplicate email (map to HTTP 409).
var ErrEmailExists = errors.New("email already in use")

// ErrNotCommentAuthor is returned by DeleteFindingComment when an author filter is set and the comment belongs to someone else (map to HTTP 403).
var ErrNotCommentAuthor = errors.New("cannot delete another user's comment")

// ErrOrgInviteExists is returned when an org+email pair already has an active (unaccepted) invite (map to HTTP 409).
var ErrOrgInviteExists = errors.New("an active invite for this email already exists")

// ErrOrgInviteAlreadyAccepted is returned by MarkOrgInviteAccepted when the invite was already accepted or no longer exists; single-use is enforced via `WHERE accepted_at IS NULL`.
var ErrOrgInviteAlreadyAccepted = errors.New("org invite has already been accepted or no longer exists")

// ErrVersionConflict is returned by SaveFlow when the expected version does not match the current row version (optimistic concurrency; map to HTTP 409).
var ErrVersionConflict = errors.New("version conflict: the flow was modified by another user")

// ErrRLSUnavailable is returned by every statement of a request whose RLS transaction could not be started: a mutation must not fall through to the pool, where RLS short-circuits to allow-all (HTTP 503).
var ErrRLSUnavailable = errors.New("rls: scoped transaction unavailable")

// ErrTokenAlreadyRevoked is returned by RevokeRefreshToken when the token was already revoked — treat as replay and revoke all of the user's sessions.
var ErrTokenAlreadyRevoked = errors.New("refresh token already revoked")

// IdentityLink ties an external IdP identity (provider + stable subject ID) to a local user account for OIDC SSO.
type IdentityLink struct {
	Provider  string    `json:"provider"`
	Subject   string    `json:"subject"`
	UserID    string    `json:"userId"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// User represents a system user in the storage backend.
type User struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	EmailVerified       bool       `json:"emailVerified"`
	FailedLoginAttempts int        `json:"-"`
	LockedUntil         *time.Time `json:"-"`
	Password            string     `json:"-"` // Bcrypt hash — never serialized to clients
	Role                auth.Role  `json:"role"`
	DisplayName         string     `json:"displayName"`
	AvatarURL           string     `json:"avatarUrl"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

// RefreshTokenInfo describes a single issued refresh token, surfaced to users as an "active session".
type RefreshTokenInfo struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// UserAgent and IP identify the device/location so the user can recognize and revoke unfamiliar sessions; may be empty on old tokens.
	UserAgent string `json:"userAgent,omitempty"`
	IP        string `json:"ip,omitempty"`
}

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
}

// FlowStore covers flow document CRUD/listing; depend on it rather than the full StorageBackend when that is all you need.
type FlowStore interface {
	SaveFlow(ctx context.Context, flow *FlowDocument) error
	// UpdateFlowTags replaces a flow's tags WITHOUT a content round-trip or version bump; implementations normalize (see NormalizeFlowTags).
	UpdateFlowTags(ctx context.Context, flowID string, tags []string) error
	// SearchFlowContents returns METADATA-ONLY documents whose content mentions needle, newest first, in the filter's scope; backends without a queryable content column return ErrContentSearchUnsupported.
	SearchFlowContents(ctx context.Context, filter FlowFilter, needle string, limit int) ([]*FlowDocument, error)
	LoadFlow(ctx context.Context, id string) (*FlowDocument, error)
	// LoadFlowHeader returns a flow's metadata WITHOUT content, so authorization/existence/version checks avoid the content fetch and blob availability.
	LoadFlowHeader(ctx context.Context, id string) (*FlowDocument, error)
	ListFlows(ctx context.Context, filter FlowFilter) ([]*FlowDocument, error)
	// CountFlows returns the total matching the filter, ignoring Limit/Offset (list pagination totals).
	CountFlows(ctx context.Context, filter FlowFilter) (int, error)
	DeleteFlow(ctx context.Context, id string) error
}

// FlowPageLister is the optional capability of backends that serve a filtered, paginated flow list AND its total in one statement; prefer via type assertion over ListFlows + CountFlows.
type FlowPageLister interface {
	ListFlowsWithTotal(ctx context.Context, filter FlowFilter) ([]*FlowDocument, int, error)
}

// FlowVersionStore covers flow version history (snapshots taken on save).
type FlowVersionStore interface {
	// SaveFlowVersion assigns v.Version atomically (caller-set value overwritten); read it back after a successful save.
	SaveFlowVersion(ctx context.Context, v *FlowVersion) error
	ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*FlowVersion, error)
	LoadFlowVersion(ctx context.Context, flowID string, version int) (*FlowVersion, error)
}

// AppSettingsStore covers global/user/org application settings.
type AppSettingsStore interface {
	SaveSettings(ctx context.Context, settings *AppSettings) error
	LoadSettings(ctx context.Context) (*AppSettings, error)
	SaveUserSettings(ctx context.Context, userID string, settings *AppSettings) error
	LoadUserSettings(ctx context.Context, userID string) (*AppSettings, error)
	SaveOrgSettings(ctx context.Context, orgID string, settings *AppSettings) error
	LoadOrgSettings(ctx context.Context, orgID string) (*AppSettings, error)
}

// ConversationStore covers persisted chat conversation history.
type ConversationStore interface {
	SaveConversation(ctx context.Context, flowID, scope string, messages []ChatMessage) error
	LoadConversation(ctx context.Context, flowID, scope string) ([]ChatMessage, error)
	DeleteConversation(ctx context.Context, flowID, scope string) error
}

// UserStore covers user account CRUD, role/password/profile updates, and the GDPR bulk operations (export, erasure, retention purge).
type UserStore interface {
	SaveUser(ctx context.Context, user *User) error
	CreateUser(ctx context.Context, user *User) error
	LoadUserByEmail(ctx context.Context, email string) (*User, error)
	LoadUserByID(ctx context.Context, id string) (*User, error)
	// LoadUsersByIDs resolves many users in one round trip; missing IDs are absent from the map (avoids N+1).
	LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*User, error)
	CountUsers(ctx context.Context) (int, error)
	// ListUsers returns up to limit users from offset, created_at desc; non-positive limit returns all.
	ListUsers(ctx context.Context, limit, offset int) ([]*User, error)
	ListAdmins(ctx context.Context) ([]*User, error)
	// QueryUsers returns one page of the admin user list (q's filters, created_at desc then id, q.Offset/q.Limit) plus the total; page never nil.
	QueryUsers(ctx context.Context, q UserQuery) ([]*User, int, error)
	// LoadUserStats counts the whole user table by role and state for the admin overview.
	LoadUserStats(ctx context.Context) (*UserStats, error)
	UpdateUserRole(ctx context.Context, id string, role auth.Role) error
	UpdateUserPassword(ctx context.Context, id string, passwordHash string) error
	// UpdateUserProfile updates the display name and avatar URL; an empty value clears it.
	UpdateUserProfile(ctx context.Context, id string, displayName, avatarURL string) error

	// DeleteUser performs GDPR erasure: deletes owned flows and per-user rows, anonymizes PII on shared rows (forensic trail retained); idempotent — a missing user is not an error.
	DeleteUser(ctx context.Context, userID string) error
	// ExportUserData assembles the data-subject export; ErrNotFound if the user does not exist.
	ExportUserData(ctx context.Context, userID string) (*UserDataExport, error)
	// PurgeExpiredData removes retention-elapsed rows (expired tokens/invites, audit_events older than auditRetentionDays — 0 keeps audit forever); returns removal counts.
	PurgeExpiredData(ctx context.Context, auditRetentionDays int) (*PurgeResult, error)
}

// OrgStore covers organisation CRUD and membership lookup.
type OrgStore interface {
	SaveOrg(ctx context.Context, org *Organisation) error
	LoadOrg(ctx context.Context, id string) (*Organisation, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*Organisation, error)
	DeleteOrg(ctx context.Context, id string) error
}

// SharingStore covers per-flow collaborator management.
type SharingStore interface {
	// ListCollaborators returns up to limit rows (limit <= 0 = all).
	ListCollaborators(ctx context.Context, flowID string, limit int) ([]*Collaborator, error)
	ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*Collaborator, error)
	AddCollaborator(ctx context.Context, flowID string, c *Collaborator) error
	UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error
	RemoveCollaborator(ctx context.Context, flowID, userID string) error
}

// UsageStore covers AI usage/cost tracking for budget enforcement.
type UsageStore interface {
	SaveUsageMetric(ctx context.Context, metric *UsageMetric) error
	GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error)
}

// DashboardStore covers the persisted flow-health snapshots and the dashboard rollup queries.
type DashboardStore interface {
	SaveFlowAnalysis(ctx context.Context, fa *FlowAnalysis) error
	// LoadFlowHealth returns the flow's latest persisted analysis snapshot or (nil, nil) when never analyzed; caller-side authorization.
	LoadFlowHealth(ctx context.Context, flowID string) (*HealthSnapshot, error)
	// LoadFlowHealthBatch resolves many flows' snapshots in one query; map omits never-analyzed flows, never nil; caller-side authorization.
	LoadFlowHealthBatch(ctx context.Context, flowIDs []string) (map[string]*HealthSnapshot, error)
	FlowDashboardData(ctx context.Context, ownerID string, days int) (*DashboardData, error)
	// FlowDashboardAdvanced returns trend, cost-by-provider, rule-frequency, activity, complexity-scatter, and security-posture data.
	FlowDashboardAdvanced(ctx context.Context, ownerID string, days int) (*DashboardAdvancedData, error)

	// Analytics reads (GET /api/dashboard/analytics); the service authorises the flow id set.
	LoadAnalysisSnapshots(ctx context.Context, flowIDs []string) ([]AnalysisSnapshotRow, error)
	LoadAnalysisHistory(ctx context.Context, w AnalyticsWindow) (*AnalysisHistoryRows, error)
	LoadTriageAnalytics(ctx context.Context, w AnalyticsWindow) (*TriageRows, error)
	// mode: "user" (the caller's calls), "org" (the scoped orgs'), "both".
	LoadUsageAnalytics(ctx context.Context, userID string, orgIDs []string, mode string, w AnalyticsWindow) (*UsageRows, error)
	LoadActivityAnalytics(ctx context.Context, userID string, w AnalyticsWindow, limit int) (*ActivityRows, error)
	LoadSecurityAnalytics(ctx context.Context, userID, email string, includeLocked bool) (*SecurityRows, error)
}

// AnalyticsWindow scopes an analytics read: the authorised flow ids, current and previous window instants (half-open [From, To)), and the IANA zone timestamps are bucketed into days with.
type AnalyticsWindow struct {
	FlowIDs  []string
	From     time.Time
	To       time.Time
	PrevFrom time.Time
	PrevTo   time.Time
	TZ       string
}

// AnalysisSnapshotRow is one flow in scope with its latest persisted analysis (Analyzed=false when it never had one).
type AnalysisSnapshotRow struct {
	FlowID        string
	Name          string
	OwnerID       string
	OrgID         string
	BlockCount    int
	UpdatedAt     time.Time
	Analyzed      bool
	HealthScore   int
	Errors        int
	Warnings      int
	Info          int
	AutoFixable   int
	TotalFindings int
	ByRule        map[string]int
	ByCategory    map[string]int
	ByConfidence  map[string]int
	AnalyzedAt    time.Time
}

// AnalysisHistoryRow is a flow's latest analysis on one caller-local day (Runs = analyses that day), or the pre-window baseline (Day empty).
type AnalysisHistoryRow struct {
	FlowID        string
	Day           string
	HealthScore   int
	Errors        int
	Warnings      int
	Info          int
	AutoFixable   int
	TotalFindings int
	Runs          int
	ByRule        map[string]int
	AnalyzedAt    time.Time
}

type AnalysisHistoryRows struct {
	Baseline []AnalysisHistoryRow
	InWindow []AnalysisHistoryRow
}

type TriageStatusRule struct {
	Status string
	RuleID string
	Count  int
}

type TriageResolvedDay struct {
	Date   string
	RuleID string
	Count  int
}

type TriageRows struct {
	ByStatusRule  []TriageStatusRule
	ResolvedByDay []TriageResolvedDay
	MttrHours     float64
	ResolvedCount int
	StaleCount    int
}

type UsageDailyRow struct {
	Date      string
	TokensIn  int
	TokensOut int
	Calls     int
	Cost      float64
}

// UsageModelRow: rows are grouped by ServedModel (the model that answered when it differs from Model), so one alias served by two models yields two rows.
type UsageModelRow struct {
	Provider    string
	Model       string
	ServedModel string
	TokensIn    int
	TokensOut   int
	Calls       int
	Cost        float64
}

// UsageRows: Daily spans the previous and current windows; the breakdowns cover the current one.
type UsageRows struct {
	Daily      []UsageDailyRow
	ByProvider []ProviderCost
	ByModel    []UsageModelRow
}

type ActivityEventRow struct {
	Action       string
	UserID       string
	ActorName    string
	ResourceType string
	ResourceID   string
	Label        string
	CreatedAt    time.Time
}

type ActionCountRow struct {
	Action string
	Count  int
}

type DateCountRow struct {
	Date  string
	Count int
}

type ActivityRows struct {
	Events   []ActivityEventRow
	ByDay    []DateCountRow
	ByAction []ActionCountRow
}

// SecurityRows: LockedRead reports whether the locked-account count was read (only when asked for; instance-wide).
type SecurityRows struct {
	FailedLogins24h int
	LockedAccounts  int
	LockedRead      bool
}

// KnowledgeStore covers the org knowledge base (RAG source documents + chunks).
type KnowledgeStore interface {
	// SaveKnowledgeDocument inserts the document row RLS-scoped to userID; the knowledge_documents WITH CHECK policy enforces org membership.
	SaveKnowledgeDocument(ctx context.Context, userID string, doc *KnowledgeDocument) error
	// DeleteKnowledgeDocument removes a document only when it belongs to orgID (one org cannot delete another's).
	DeleteKnowledgeDocument(ctx context.Context, userID, orgID, id string) error
	// DeleteKnowledgeDocumentByName removes every document with the given filename in the org — re-upload REPLACE semantics (chunks cascade).
	DeleteKnowledgeDocumentByName(ctx context.Context, orgID, filename string) error
	// ListKnowledgeDocuments returns up to limit rows (<=0 = all), newest first, skipping offset rows.
	ListKnowledgeDocuments(ctx context.Context, orgID string, limit, offset int) ([]*KnowledgeDocument, error)
	// ListKnowledgeDocumentIDsByFilename returns the ids of the org's documents named filename (versions a re-upload supersedes) plus the org's total doc count.
	ListKnowledgeDocumentIDsByFilename(ctx context.Context, orgID, filename string) (ids []string, total int, err error)
	// SaveKnowledgeChunks inserts chunk rows RLS-scoped to userID; the knowledge_chunks WITH CHECK policy enforces org membership ("" only in non-authenticated local mode).
	SaveKnowledgeChunks(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]KnowledgeChunk, error)
	// ListKnowledgeChunkContents returns up to limit chunks (no embeddings) strictly after the cursor in (doc_id, id) order — one re-index page; limit <= 0 = backend page cap.
	ListKnowledgeChunkContents(ctx context.Context, orgID string, after KnowledgeChunkCursor, limit int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddings returns up to n chunks WITH stored embeddings, one per document, deterministic order — RAG re-embeds their text as an embedding-model mismatch guard.
	SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddingsUpTo returns up to n chunks at or before the cursor in (doc_id, id) order NEAREST first WITH stored embeddings — a resumed re-index model check.
	SampleKnowledgeChunkEmbeddingsUpTo(ctx context.Context, orgID string, upTo KnowledgeChunkCursor, n int) ([]KnowledgeChunk, error)
	// UpdateKnowledgeChunkEmbeddings replaces chunk embeddings in place (RLS-scoped like SaveKnowledgeChunks) and refreshes the pgvector column for same-dimension vectors.
	UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	// CountKnowledgeChunks reports org totals: all chunks and how many are in the pgvector index at the configured dimension (dim-mismatch detection).
	CountKnowledgeChunks(ctx context.Context, orgID string) (total, vectorIndexed int, err error)
}

// HealthChecker covers backend liveness/lifecycle; every backend must support it regardless of role.
type HealthChecker interface {
	Ping(ctx context.Context) error
	Close() error
}

// AuditStore covers the append-only security/governance audit log.
type AuditStore interface {
	SaveAuditEvent(ctx context.Context, event *AuditEvent) error
	ListAuditEvents(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error)
}

// TriageStore covers finding triage state (status overrides) and per-flow accepted-findings baselines.
type TriageStore interface {
	// SetFindingStatus upserts the triage state for one finding (keyed by FlowID + FindingKey).
	SetFindingStatus(ctx context.Context, st *FindingStatus) error
	// BatchSetFindingStatus upserts triage state for one flow's findings atomically (all or none); userID scopes RLS ("" only in non-authenticated local mode).
	BatchSetFindingStatus(ctx context.Context, flowID, userID string, items []*FindingStatus) error
	// ListFindingStatuses returns all persisted triage states for a flow; never nil.
	ListFindingStatuses(ctx context.Context, flowID string) ([]*FindingStatus, error)
	// DeleteFindingStatus removes triage state for one finding, resetting it to "open"; missing records are not an error.
	DeleteFindingStatus(ctx context.Context, flowID, findingKey string) error
	// GetFlowBaseline returns the flow's accepted-findings baseline, or (nil, nil) when none is set.
	GetFlowBaseline(ctx context.Context, flowID string) (*FlowBaseline, error)
	// GetFlowBaselinesBatch resolves many flows' baselines in one query; map omits flows without one, never nil; caller-side authorization.
	GetFlowBaselinesBatch(ctx context.Context, flowIDs []string) (map[string]*FlowBaseline, error)
	// SetFlowBaseline replaces the flow's baseline (one baseline per flow).
	SetFlowBaseline(ctx context.Context, b *FlowBaseline) error
	// ClearFlowBaseline removes the flow's baseline. Idempotent.
	ClearFlowBaseline(ctx context.Context, flowID string) error
}

// CommentStore covers team-shared review threads on individual findings.
type CommentStore interface {
	AddFindingComment(ctx context.Context, c *FindingComment) error
	ListFindingComments(ctx context.Context, flowID, findingKey string) ([]*FindingComment, error)
	// DeleteFindingComment removes a comment; non-empty authorID restricts to that author (mismatch returns ErrNotCommentAuthor), empty deletes unconditionally; absent is a no-op.
	DeleteFindingComment(ctx context.Context, flowID, commentID, authorID string) error
}

// ShareTokenStore covers read-only public report links.
type ShareTokenStore interface {
	CreateShareToken(ctx context.Context, t *ShareToken) error
	GetShareTokenByHash(ctx context.Context, tokenHash string) (*ShareToken, error)
	ListShareTokens(ctx context.Context, flowID string) ([]*ShareToken, error)
	RevokeShareToken(ctx context.Context, flowID, tokenID string) error
}

// APITokenStore covers machine credentials (personal access tokens).
type APITokenStore interface {
	CreateAPIToken(ctx context.Context, t *APIToken) error
	// GetAPITokenByHash resolves a token by hash for authentication, or ErrNotFound; revocation is a delete, so a revoked token is not found.
	GetAPITokenByHash(ctx context.Context, tokenHash string) (*APIToken, error)
	// ListAPITokens returns a user's tokens (metadata only; never the hash to clients).
	ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error)
	// DeleteAPIToken removes a token owned by userID (one user cannot revoke another's); idempotent.
	DeleteAPIToken(ctx context.Context, userID, id string) error
}

// UserTokenStore covers one-shot user tokens (password reset, email verification); only the hash is stored.
type UserTokenStore interface {
	CreateUserToken(ctx context.Context, t *UserToken) error
	// ConsumeUserToken atomically redeems a valid (unused, unexpired) token of the purpose, marks it used, and returns the owning userID (ErrNotFound — no replay).
	ConsumeUserToken(ctx context.Context, purpose, tokenHash string) (userID string, err error)
	// InvalidateUserTokens marks every unused, unexpired token of the purposes used, so outstanding reset/verify links die after a password change (account-takeover guard); no-op when none match.
	InvalidateUserTokens(ctx context.Context, userID string, purposes ...string) error
	// SetUserEmailVerified marks a user's email as verified.
	SetUserEmailVerified(ctx context.Context, userID string) error
}

// StorageBackend is the full storage capability set composed from the role interfaces above; depend on the narrowest role interface(s) you use.
type StorageBackend interface {
	FlowStore
	OrgChannelStore
	OrgCustomRuleStore
	FlowVersionStore
	AppSettingsStore
	ConversationStore
	UserStore
	OrgStore
	SharingStore
	UsageStore
	DashboardStore
	KnowledgeStore
	HealthChecker
	AuditStore
	TriageStore
	CommentStore
	ShareTokenStore
	APITokenStore
	UserTokenStore
	PolicyStore
	GovernanceAlertStore
}

// PolicyStore covers org-scoped governance policies (named rule sets with a pass/fail gate severity); filesystem backend stubs these as no-ops.
type PolicyStore interface {
	SavePolicy(ctx context.Context, p *models.Policy) error
	GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error)
	ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error)
	DeletePolicy(ctx context.Context, orgID, id string) error
}

// GovernanceAlertStore is the in-app inbox for continuous-governance scanner signals. Alerts are team-shared; the scanner writes without an RLS principal, users read/ack/dismiss through the API.
type GovernanceAlertStore interface {
	// RecordGovernanceAlert persists a new alert (ID and CreatedAt stamped by the scanner); a duplicate ID is a no-op (retry-safe).
	RecordGovernanceAlert(ctx context.Context, a *GovernanceAlert) error
	// ListGovernanceAlerts returns alerts visible to the caller, newest first, per the filter's pagination + include-dismissed flag; never nil.
	ListGovernanceAlerts(ctx context.Context, filter GovernanceAlertFilter) ([]*GovernanceAlert, error)
	// UnreadGovernanceAlertCount returns the number of visible unacknowledged alerts (read_at IS NULL and not dismissed).
	UnreadGovernanceAlertCount(ctx context.Context) (int, error)
	// UnreadGovernanceAlertCountFor scopes the badge count to one caller: team-wide plus their targeted alerts.
	UnreadGovernanceAlertCountFor(ctx context.Context, userID string) (int, error)
	// MarkGovernanceAlertRead stamps read_at = NOW() on one alert. Idempotent.
	MarkGovernanceAlertRead(ctx context.Context, userID, alertID string) error
	// MarkAllGovernanceAlertsRead stamps read_at = NOW() on every visible unread alert. Idempotent.
	MarkAllGovernanceAlertsRead(ctx context.Context, userID string) error
	// DismissGovernanceAlert stamps dismissed_at = NOW() on one alert (hidden from the default view, not deleted). Idempotent.
	DismissGovernanceAlert(ctx context.Context, userID, alertID string) error
	// ClearGovernanceAlerts permanently deletes the caller's visible dismissed alerts.
	ClearGovernanceAlerts(ctx context.Context, userID string) error
}

// UserToken purposes.
const (
	TokenPurposePasswordReset = "password_reset"
	TokenPurposeEmailVerify   = "email_verify"
)

// UserToken is a single-use credential for an out-of-band flow (password reset or email verification); stored hashed, redeemed via ConsumeUserToken.
type UserToken struct {
	TokenHash string    `json:"-"`
	Purpose   string    `json:"purpose"`
	UserID    string    `json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}

// Organisation represents a team or workspace that owns shared flows.
type Organisation struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	OwnerID   string      `json:"ownerId"`
	Members   []OrgMember `json:"members"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// OrgMember represents a user's membership in an organisation.
type OrgMember struct {
	UserID   string    `json:"userId"`
	Role     auth.Role `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

// OrgInvite represents a pending (or resolved) invitation to join an organisation; only the SHA-256 hash of the raw token is persisted.
type OrgInvite struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"orgId"`
	Email      string     `json:"email"`
	Role       auth.Role  `json:"role"`
	InvitedBy  string     `json:"invitedBy"`
	TokenHash  string     `json:"-"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// Collaborator represents a user with access to a specific flow.
type Collaborator struct {
	UserID     string    `json:"userId"`
	Email      string    `json:"email"`
	Permission string    `json:"permission"`
	GrantedAt  time.Time `json:"grantedAt"`
}

// FlowSort enumerates the supported sort orders for ListFlows; the zero value (FlowSortUpdatedDesc) is the historical default.
type FlowSort int

const (
	FlowSortUpdatedDesc FlowSort = iota
	FlowSortUpdatedAsc
	FlowSortNameAsc
	FlowSortNameDesc
	FlowSortBlocksDesc
	// FlowSortIDAsc orders by the primary key so whole-table LIMIT/OFFSET walks (migrator, governance scanner) see each row exactly once; other sorts order by MUTABLE columns, where a mid-walk save shifts later rows.
	FlowSortIDAsc
)

// FlowFilter defines filtering options for listing flows
type FlowFilter struct {
	UserID         string
	OrganizationID string
	// OrganizationIDs widens the org scope to multiple orgs; the service must verify membership before populating it — never bind directly from user input.
	OrganizationIDs []string
	Query           string
	CreatedAfter    *time.Time
	CreatedBefore   *time.Time
	Limit           int
	Offset          int
	// AfterID is a keyset cursor (only flows whose id sorts after it match), meaningful only with FlowSortIDAsc; backends that predate it ignore it — walkers must detect a non-advancing page.
	AfterID string
	SortBy  FlowSort
	// MetadataOnly skips loading each flow's Content; backends leave FlowDocument.Content empty when set.
	MetadataOnly bool
	// AllFlows is the operational-enumeration opt-in (filesystem→cloud migrator) that bypasses owner/org scoping — never bind from user input; without it an unscoped filter matches nothing.
	AllFlows bool
	// SharedOnly limits results to flows where the caller is a collaborator (not owner, not via org).
	SharedOnly bool
	// Tags matches flows carrying ANY of the listed tags (tag-picker any-of semantics); normalization is storage's concern.
	Tags []string
}

// ErrContentSearchUnsupported is returned by SearchFlowContents on backends that cannot push content matching into the store; callers fall back to their legacy scan.
var ErrContentSearchUnsupported = errors.New("content search not supported by this backend")

// HealthSnapshot is the persisted per-flow analysis summary surfaced on the single-flow GET; nil means never analyzed.
type HealthSnapshot struct {
	HealthScore int       `json:"healthScore"`
	Errors      int       `json:"errors"`
	Warnings    int       `json:"warnings"`
	Info        int       `json:"info"`
	AnalyzedAt  time.Time `json:"analyzedAt"`
}

// UsageMetric is one AI call's usage row: Model is the id the caller asked for; ServedModel is the model the provider reported answering when different (alias/router/agent), empty otherwise.
type UsageMetric struct {
	ID               string    `json:"id"`
	UserID           string    `json:"userId"`
	OrgID            string    `json:"orgId"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	ServedModel      string    `json:"servedModel,omitempty"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	EstimatedCost    float64   `json:"estimatedCost"`
	CreatedAt        time.Time `json:"createdAt"`
}

// FlowAnalysis is the upserted summary of a flow's latest analysis (aggregates survive restarts); owner/org scoping is resolved by JOINing flows at read time — no denormalized ownership.
type FlowAnalysis struct {
	FlowID      string         `json:"flowId"`
	HealthScore int            `json:"healthScore"`
	Errors      int            `json:"errors"`
	Warnings    int            `json:"warnings"`
	Info        int            `json:"info"`
	ByCategory  map[string]int `json:"byCategory"`
	ByRule      map[string]int `json:"byRule"`
	AnalyzedAt  time.Time      `json:"analyzedAt"`
	// ByConfidence distributes findings across confidence tiers (high/medium/low) for the dashboard's "trust" donut.
	ByConfidence map[string]int `json:"byConfidence,omitempty"`
	// AutoFixableCount is how many findings carry a one-click deterministic fix (Finding.AutoFix != "").
	AutoFixableCount int `json:"autoFixableCount,omitempty"`
	// TotalFindings is the sum of Errors+Warnings+Info, denormalized for org-wide SUMs.
	TotalFindings int `json:"totalFindings,omitempty"`
}

// RecentFlowHealth is one row of the dashboard's "recent flows" list: a flow with its latest health score (nil when never analyzed).
type RecentFlowHealth struct {
	FlowID      string
	Name        string
	HealthScore *int
	UpdatedAt   time.Time
}

// DailyTokens is one gap-filled day of AI token usage for the dashboard chart.
type DailyTokens struct {
	Date      string
	TokensIn  int
	TokensOut int
}

// DashboardData backs GET /api/dashboard/home in cloud mode; empty sections are conveyed by HealthCount (0 ⇒ nothing analyzed) and len(TokenUsage).
type DashboardData struct {
	TotalFlows    int
	TotalSubflows int
	HealthCount   int // number of analyzed flows contributing to AvgHealth
	AvgHealth     int
	Errors        int
	Warnings      int
	Info          int
	ByCategory    map[string]int
	Recent        []RecentFlowHealth
	TokenUsage    []DailyTokens
	// TotalFindings/AutoFixable/Confidence: org-wide findings sum, one-click-fixable count, certainty-tier distribution (dashboard KPIs).
	TotalFindings int
	AutoFixable   int
	Confidence    map[string]int
	// HealthBuckets is the org-wide health-score histogram (5 buckets of 20) exposing what AvgHealth hides.
	HealthBuckets []HealthBucket
}

// DailySeverityPoint is one day of the org-wide severity trend across every flow analyzed that day (stacked-area chart).
type DailySeverityPoint struct {
	Date     string
	Errors   int
	Warnings int
	Info     int
}

// HealthBucket is one 20-point-wide slice of the health-score histogram.
type HealthBucket struct {
	Label string // "0-20", "20-40", ...
	Lo    int
	Hi    int
	Count int
}

// DashboardAdvancedData extends DashboardData with trend, cost, rule, activity, complexity, and security sections.
type DashboardAdvancedData struct {
	HealthTrend []DailyHealthPoint
	CostByProv  []ProviderCost
	RuleFreq    []RuleFrequency
	Activity    []ActivityEntry
	Complexity  []FlowComplexityPoint
	Security    DashboardSecurity
	// SeverityTrend is the org-wide daily error/warning/info series for the stacked-area health-trend chart.
	SeverityTrend []DailySeverityPoint
	// Workflow is the team-triage funnel + resolution stats (MTTR, stale); cloud-only — local mode leaves Available false.
	Workflow WorkflowData
}

// WorkflowData holds the cloud-mode team-triage analytics: triage-state distribution, MTTR, and stale-finding count.
type WorkflowData struct {
	Funnel        map[string]int // status ("open"/"acknowledged"/"in_progress"/"resolved"/"suppressed") → count
	MttrHours     float64        // mean updated_at−created_at for resolved findings; 0 if none resolved
	StaleCount    int            // open/acknowledged untouched for > 14 days
	ResolvedCount int            // resolved findings contributing to MttrHours
}

// DailyHealthPoint is one day of the health-score trend chart.
type DailyHealthPoint struct {
	Date      string
	AvgHealth int
	FlowCount int
}

// ProviderCost aggregates AI spend by provider for the donut chart.
type ProviderCost struct {
	Provider  string
	Cost      float64
	TokensIn  int
	TokensOut int
}

// RuleFrequency is one rule's finding count across the owner's flows; severity tinting is a service-layer catalog concern.
type RuleFrequency struct {
	Rule  string
	Count int
}

// ActivityEntry is one row of the dashboard activity feed.
type ActivityEntry struct {
	Action    string
	FlowName  string
	CreatedAt time.Time
}

// FlowComplexityPoint is one flow's position on the complexity scatter.
type FlowComplexityPoint struct {
	FlowID       string
	FlowName     string
	BlockCount   int
	FindingCount int
	HealthScore  int
}

// DashboardSecurity summarizes security posture. Available is false when any count failed: each is a best-effort query whose failure leaves 0, which the UI would read as "no issues detected".
type DashboardSecurity struct {
	Available          bool
	FailedLogins24h    int
	LockedAccounts     int
	CredentialFindings int
}

type KnowledgeDocument struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Filename  string    `json:"filename"`
	CreatedAt time.Time `json:"createdAt"`
	// ChunkCount / VectorIndexed are list-view extras: chunks the doc has vs how many are in the pgvector index; chunks>0 with 0 indexed = stranded by an embedding-provider switch.
	ChunkCount    int `json:"chunkCount,omitempty"`
	VectorIndexed int `json:"vectorIndexed,omitempty"`
}

type KnowledgeChunk struct {
	ID    string `json:"id"`
	DocID string `json:"docId"`
	// OrgID is denormalised from the parent document (knowledge_chunks.org_id, v28) so org reads and the RLS policy test the chunk row alone; a chunk saved without it inherits its doc's org.
	OrgID     string    `json:"orgId,omitempty"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
	// Filename of the source document (joined at retrieval time) so RAG can attribute each chunk to its source.
	Filename string `json:"filename,omitempty"`
}

// KnowledgeChunkCursor is a keyset position in an org's chunks ordered by (doc_id, id); zero value = start (TEXT columns, "" sorts first); also the re-index resume token — clients treat it as opaque.
type KnowledgeChunkCursor struct {
	DocID string `json:"docId"`
	ID    string `json:"id"`
}

// IsZero reports whether c is the start-of-corpus cursor.
func (c KnowledgeChunkCursor) IsZero() bool { return c.DocID == "" && c.ID == "" }

// FlowDocument represents a flow document
type FlowDocument struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Content     json.RawMessage `json:"content"`
	// Source is the raw PAD source (single-file flows) that cloud-mode apply/preview-fix patches line-based instead of Content; empty for legacy rows and multi-file flows.
	Source         string       `json:"source,omitempty"`
	Metadata       FlowMetadata `json:"metadata"`
	OwnerID        string       `json:"ownerId"`
	OrganizationID string       `json:"organizationId"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	Version        int          `json:"version"`
	// Tags are the flow's organizational labels, normalized by storage (see NormalizeFlowTags).
	Tags []string `json:"tags,omitempty"`
}

// FlowMetadata is stored as flows.metadata and READ BY SQL (ORDER BY metadata->>'BlockCount', dashboard sums, an expression index) — its JSON keys are a storage contract.
type FlowMetadata struct {
	BlockCount   int
	SubflowCount int
	MaxDepth     int
	ParsedAt     time.Time
	FileSize     int
	RawLineCount int
}

// AppSettings MUST stay in field/tag parity with models.AppSettings — the service bridge round-trips via JSON and silently drops mismatches (service.TestSettingsRoundTrip guards it).
type AppSettings struct {
	Version     int                `json:"version"`
	General     GeneralSettings    `json:"general"`
	Appearance  AppearanceSettings `json:"appearance"`
	Layout      LayoutSettings     `json:"layout"`
	AI          AISettings         `json:"ai"`
	Parser      ParserSettings     `json:"parser"`
	Analysis    AnalysisSettings   `json:"analysis"`
	RecentFiles []RecentFile       `json:"recentFiles"`
}

// ToolCallRecord mirrors models.ToolCallRecord (same JSON tags) — the per-message tool transparency trail.
type ToolCallRecord struct {
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	Ok         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// FixItemSnapshot mirrors models.FixItemSnapshot — one fix inside a batch approval record.
type FixItemSnapshot struct {
	RuleID     string `json:"ruleId"`
	FixType    string `json:"fixType"`
	BlockLabel string `json:"blockLabel"`
	Line       int    `json:"line"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// FixProposalSnapshot mirrors models.FixProposalSnapshot — the persisted apply_fix approval record (single or batch).
type FixProposalSnapshot struct {
	ProposalID string            `json:"proposalId"`
	RuleID     string            `json:"ruleId"`
	FixType    string            `json:"fixType"`
	BlockLabel string            `json:"blockLabel"`
	Line       int               `json:"line"`
	Summary    string            `json:"summary"`
	Status     string            `json:"status"`
	Message    string            `json:"message,omitempty"`
	Items      []FixItemSnapshot `json:"items,omitempty"`
}

// ChatMessage mirrors models.ChatMessage for persistence; ServedModel and billed rates ride inside the messages JSON — no columns of their own.
type ChatMessage struct {
	ID               string                `json:"id"`
	Role             string                `json:"role"`
	Content          string                `json:"content"`
	Timestamp        string                `json:"timestamp"`
	ContextBlockID   string                `json:"contextBlockId,omitempty"`
	ContextSubflowID string                `json:"contextSubflowId,omitempty"`
	TokensIn         int                   `json:"tokensIn,omitempty"`
	TokensOut        int                   `json:"tokensOut,omitempty"`
	Provider         string                `json:"provider,omitempty"`
	Model            string                `json:"model,omitempty"`
	ModelLabel       string                `json:"modelLabel,omitempty"`
	ServedModel      string                `json:"servedModel,omitempty"`
	InputCostPerM    float64               `json:"inputCostPerM,omitempty"`
	OutputCostPerM   float64               `json:"outputCostPerM,omitempty"`
	FinishReason     string                `json:"finishReason,omitempty"`
	ToolCalls        []ToolCallRecord      `json:"toolCalls,omitempty"`
	FixProposal      *FixProposalSnapshot  `json:"fixProposal,omitempty"` // legacy singular
	FixProposals     []FixProposalSnapshot `json:"fixProposals,omitempty"`
}

// Settings types — kept in field/tag parity with internal/models so the toModel/fromModel JSON bridge is lossless.
type GeneralSettings struct {
	FirstRunCompleted bool `json:"firstRunCompleted"`
}

type AppearanceSettings struct {
	Theme        string `json:"theme"`
	Density      string `json:"density"`
	ReduceMotion bool   `json:"reduceMotion"`
	HighContrast bool   `json:"highContrast"`
	ChatTextSize string `json:"chatTextSize,omitempty"`
}

type LayoutSettings struct {
	SidebarWidth           int    `json:"sidebarWidth"`
	InspectorWidth         int    `json:"inspectorWidth"`
	SidebarCollapsed       bool   `json:"sidebarCollapsed"`
	InspectorCollapsed     bool   `json:"inspectorCollapsed"`
	LastActiveInspectorTab string `json:"lastActiveInspectorTab"`
	LastViewMode           string `json:"lastViewMode"`
}

type AIPromptsConfig struct {
	Block             []string `json:"block"`
	Flow              []string `json:"flow"`
	Finding           []string `json:"finding"`
	BlockWithFindings []string `json:"blockWithFindings"`
}

type AISettings struct {
	ActiveProvider    string `json:"activeProvider"`
	EmbeddingProvider string `json:"embeddingProvider"`
	// EmbeddingModel mirrors models.AISettings.EmbeddingModel — keep tag parity or the round-trip drops it.
	EmbeddingModel          string                      `json:"embeddingModel,omitempty"`
	Providers               map[string]AIProviderConfig `json:"providers"`
	ShowCostEstimates       bool                        `json:"showCostEstimates"`
	SaveConversationHistory bool                        `json:"saveConversationHistory"`
	SystemPromptSuffix      string                      `json:"systemPromptSuffix,omitempty"`
	DailyBudget             float64                     `json:"dailyBudget"`
	Prompts                 AIPromptsConfig             `json:"prompts"`
}

type AIProviderConfig struct {
	Enabled      bool    `json:"enabled"`
	DefaultModel string  `json:"defaultModel"`
	Temperature  float64 `json:"temperature"`
	MaxTokens    int     `json:"maxTokens"`
	// ReasoningEffort mirrors models.AIProviderConfig.ReasoningEffort — keep tag parity or the round-trip drops it.
	ReasoningEffort    string `json:"reasoningEffort,omitempty"`
	ContextTokenBudget int    `json:"contextTokenBudget"`
}

type ParserSettings struct {
	MaxFileSizeMB int `json:"maxFileSizeMB"`
}

type AnalysisSettings struct {
	Rules             map[string]RuleConfig `json:"rules"`
	AutoAnalyzeOnOpen bool                  `json:"autoAnalyzeOnOpen"`
}

type RuleConfig struct {
	Enabled  bool                   `json:"enabled"`
	Severity string                 `json:"severity"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

type RecentFile struct {
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	LastOpen time.Time `json:"lastOpen"`
	IsFolder bool      `json:"isFolder"`
}

// AuditEvent records a user action; the on-disk spill queue persists these as JSON, so field names are a storage contract.
type AuditEvent struct {
	ID           string
	UserID       string
	Email        string
	Action       string
	ResourceType string
	ResourceID   string
	IP           string
	Meta         map[string]string
	CreatedAt    time.Time
}

// AuditFilter controls which audit events are returned.
type AuditFilter struct {
	UserID string
	Action string
	Limit  int
	Offset int
	Since  *time.Time
	// IncludeSubject also matches events whose subject is UserID (sign-ins carry no actor); the GDPR export must not set it — a user's export must never carry a failed sign-in's IP.
	IncludeSubject bool
}

// FlowVersion stores a flow snapshot; rendered through an API view — the nested FlowMetadata carries SQL-dependent JSON keys.
type FlowVersion struct {
	ID        string
	FlowID    string
	Version   int
	Comment   string
	Content   json.RawMessage
	Metadata  FlowMetadata
	CreatedBy string
	CreatedAt time.Time
}

// FindingStatus is the team-shared triage state keyed by flow + content-derived finding key ("ruleID:blockID"), so state survives re-analysis.
type FindingStatus struct {
	FlowID        string    `json:"flowId"`
	FindingKey    string    `json:"findingKey"`
	RuleID        string    `json:"ruleId,omitempty"`
	Status        string    `json:"status"` // open, acknowledged, in_progress, resolved, suppressed
	Justification string    `json:"justification,omitempty"`
	AssigneeID    string    `json:"assigneeId,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// GovernanceAlert is one in-app governance signal. Alerts are team-shared (RLS inherits flow visibility) and read/dismiss stamps are team-global — one ack clears all badges.
type GovernanceAlert struct {
	ID          string     `json:"id"`
	FlowID      string     `json:"flowId"`
	FlowName    string     `json:"flowName,omitempty"`
	OrgID       string     `json:"orgId,omitempty"`
	Type        string     `json:"type"` // "drift" | "health_regression" | "finding_assigned" | "comment_added"
	Title       string     `json:"title"`
	Message     string     `json:"message,omitempty"`
	Severity    string     `json:"severity"` // "error" | "warning"
	NewErrors   int        `json:"newErrors,omitempty"`
	NewWarnings int        `json:"newWarnings,omitempty"`
	HealthScore int        `json:"healthScore,omitempty"`
	PrevHealth  int        `json:"prevHealth,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ReadAt      *time.Time `json:"readAt,omitempty"`
	DismissedAt *time.Time `json:"dismissedAt,omitempty"`
	// TargetUser is the delivery target for PERSONAL alerts (assignment/comment notifications): visible only to that user; empty = team-wide.
	TargetUser string `json:"targetUser,omitempty"`
}

// GovernanceAlertFilter narrows ListGovernanceAlerts: Limit/Offset paginate (<=0 ⇒ default cap); an alert matches UserID when TargetUser is empty (team-wide) OR equals it; empty = team-wide only.
type GovernanceAlertFilter struct {
	Limit            int
	Offset           int
	IncludeDismissed bool
	UserID           string
}

// OrgChannel is one org-configured notification destination for the org's governance events; URL kinds: webhook / teams / slack.
type OrgChannel struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"` // HMAC key; never serialized to clients
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// OrgCustomRule is one user-authored analyzer rule owned by an org. Config is a JSON-serialized analyzer.CustomRuleConfig carried opaquely; RuleID (the one on findings) is unique PER ORG.
type OrgCustomRule struct {
	ID        string          `json:"id"`
	OrgID     string          `json:"orgId"`
	RuleID    string          `json:"ruleId"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// OrgCustomRuleStore covers per-org custom analyzer rules.
type OrgCustomRuleStore interface {
	// SaveOrgCustomRule upserts a rule; (org_id, rule_id) is unique — the same id under a different org is a distinct rule.
	SaveOrgCustomRule(ctx context.Context, rule *OrgCustomRule) error
	DeleteOrgCustomRule(ctx context.Context, orgID, id string) error
	// ListOrgCustomRules returns the org's rules; enabledOnly for the analysis path, which must not compile disabled rules.
	ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgCustomRule, error)
}

// OrgChannelStore covers per-org notification channel management.
type OrgChannelStore interface {
	// SaveOrgChannel upserts a channel (ID-keyed); Kind/URL validation is the API's concern.
	SaveOrgChannel(ctx context.Context, ch *OrgChannel) error
	DeleteOrgChannel(ctx context.Context, orgID, id string) error
	// ListOrgChannels returns the org's channels; enabledOnly for dispatch.
	ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgChannel, error)
}

// APIToken is a scoped, revocable machine credential authenticating API calls as its owner; only the SHA-256 hash is persisted, the raw value shown once and unrecoverable.
type APIToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Name      string     `json:"name"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil ⇒ no expiry
	// Scopes restricts a PAT's capabilities (empty = unscoped = full access); members of auth.ValidTokenScopes only.
	Scopes []string `json:"scopes,omitempty"`
}

// FlowBaseline is the set of finding keys accepted as a flow's baseline: in-set = "known", else "new since baseline" — the ratcheting/gating basis.
type FlowBaseline struct {
	FlowID    string    `json:"flowId"`
	Keys      []string  `json:"keys"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// FindingComment is a team-shared review comment keyed by the finding's stable key (same as triage); comments persist across re-analysis.
type FindingComment struct {
	ID         string    `json:"id"`
	FlowID     string    `json:"flowId"`
	FindingKey string    `json:"findingKey"`
	AuthorID   string    `json:"authorId"`
	AuthorName string    `json:"authorName,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ShareToken is a revocable, read-only public link to a flow's findings report; only the SHA-256 hash is persisted, the raw value shown once.
type ShareToken struct {
	ID        string     `json:"id"`
	FlowID    string     `json:"flowId"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedBy string     `json:"createdBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil ⇒ no expiry
}

// ── Flow-tag normalization ───────────────────────────────────────────
// Shared by every backend: lowercase letters/digits/'-'/'_', ≤32 chars each, ≤20 per flow, deduped.

const (
	MaxFlowTags   = 20
	MaxFlowTagLen = 32
)

// NormalizeFlowTags canonicalizes a tag list: trim, lowercase, drop empties and duplicates, enforce the whitelist and caps; returns an error naming the first rejected tag.
func NormalizeFlowTags(tags []string) ([]string, error) {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" {
			continue
		}
		if len(tag) > MaxFlowTagLen {
			return nil, fmt.Errorf("tag %q exceeds %d characters", raw, MaxFlowTagLen)
		}
		for _, r := range tag {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return nil, fmt.Errorf("tag %q may only contain letters, digits, '-' and '_'", raw)
			}
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > MaxFlowTags {
		return nil, fmt.Errorf("a flow carries at most %d tags", MaxFlowTags)
	}
	return out, nil
}

// SplitFlowTags parses the CSV tags column; empty ⇒ nil (untagged).
func SplitFlowTags(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
