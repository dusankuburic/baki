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

// ErrEmailExists is returned by CreateUser when a user with the same email
// already exists. The caller should map this to HTTP 409.
var ErrEmailExists = errors.New("email already in use")

// ErrNotCommentAuthor is returned by DeleteFindingComment when an author
// filter is set and the comment belongs to someone else. The caller should
// map this to HTTP 403.
var ErrNotCommentAuthor = errors.New("cannot delete another user's comment")

// ErrOrgInviteExists is returned when creating an org invite for an
// org+email pair that already has an active (unaccepted) invite. The caller
// should map this to HTTP 409.
var ErrOrgInviteExists = errors.New("an active invite for this email already exists")

// ErrOrgInviteAlreadyAccepted is returned by MarkOrgInviteAccepted when the
// invite has already been accepted (concurrent AcceptInvite race) or no longer
// exists. The single-use contract is enforced at the storage layer via
// `WHERE accepted_at IS NULL` so two concurrent callers cannot both succeed.
var ErrOrgInviteAlreadyAccepted = errors.New("org invite has already been accepted or no longer exists")

// ErrVersionConflict is returned by SaveFlow when the expected version does
// not match the current row version (optimistic concurrency check failed).
// The caller should map this to HTTP 409.
var ErrVersionConflict = errors.New("version conflict: the flow was modified by another user")

// ErrTokenAlreadyRevoked is returned by RevokeRefreshToken when the token was
// already revoked (e.g. by a concurrent refresh). Callers should treat this as
// evidence of token replay and revoke all of the user's sessions.
var ErrTokenAlreadyRevoked = errors.New("refresh token already revoked")

// IdentityLink ties an external IdP identity (provider + stable subject ID)
// to a local user account, enabling OIDC single sign-on.
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

// RefreshTokenInfo describes a single issued refresh token, surfaced to users
// as an "active session" in profile/session-management UIs.
type RefreshTokenInfo struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// UserAgent and IP identify the device/location the session was issued
	// to (captured fresh on each login/refresh), so the client can show a
	// friendly device label and let a user recognize/revoke unfamiliar
	// sessions. Both may be empty for tokens issued before this was tracked.
	UserAgent string `json:"userAgent,omitempty"`
	IP        string `json:"ip,omitempty"`
}

// UserDataExport is the data-subject access / portability bundle returned by
// ExportUserData. It contains only the user's own data; collaborator-authored
// content on shared flows is out of scope.
type UserDataExport struct {
	User        *User                `json:"user"`
	Flows       []*FlowDocument      `json:"flows"`
	Settings    *AppSettings         `json:"settings"`
	AuditEvents []ExportedAuditEvent `json:"auditEvents"`
	APITokens   []*APIToken          `json:"apiTokens"`
	// The sections below were missing from the bundle while the endpoint
	// described itself as "everything stored about the user": chat history on
	// the user's own flows, the comments and triage decisions they authored,
	// the organisations they belong to and their active sessions.
	Conversations   []*ExportedConversation `json:"conversations"`
	Comments        []*FindingComment       `json:"comments"`
	TriageDecisions []*FindingStatus        `json:"triageDecisions"`
	Organisations   []*Organisation         `json:"organisations"`
	Sessions        []RefreshTokenInfo      `json:"sessions"`
	// AzureRegistry is the user's personal Azure AI Foundry resources and
	// models (Part 9) — metadata and public configuration, never a secret.
	AzureRegistry *ExportedAzureRegistry `json:"azureRegistry"`
	ExportedAt    time.Time              `json:"exportedAt"`
}

// ExportedAuditEvent is an AuditEvent with the camelCase keys the rest of the
// bundle uses. AuditEvent itself stays untagged because the on-disk spill queue
// persists it as JSON, so its field names are a storage contract.
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

// ExportedConversation is one stored chat conversation on a flow the user
// owns, keyed by the same (flowId, scope) pair the ConversationStore uses.
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

// StorageBackend defines the interface for storage implementations
// This allows abstraction between local file system and cloud database storage
// FlowStore covers flow document CRUD/listing. Callers that only need to
// load/save/list flows (not the full StorageBackend) should depend on this
// narrower interface — e.g. services that shouldn't be able to touch users,
// orgs, or audit logs.
type FlowStore interface {
	SaveFlow(ctx context.Context, flow *FlowDocument) error
	// UpdateFlowTags replaces a flow's organizational tags WITHOUT a content
	// round-trip or version bump (tags are metadata, not flow content).
	// Implementations normalize (lowercase, char whitelist, ≤32 chars/tag,
	// ≤20 tags) and reject invalid input.
	UpdateFlowTags(ctx context.Context, flowID string, tags []string) error
	// SearchFlowContents returns METADATA-ONLY documents whose stored content
	// mentions needle (block names, action types, property values), newest
	// first, within the filter's visibility scope. Backends without a
	// queryable content column return ErrContentSearchUnsupported.
	SearchFlowContents(ctx context.Context, filter FlowFilter, needle string, limit int) ([]*FlowDocument, error)
	LoadFlow(ctx context.Context, id string) (*FlowDocument, error)
	// LoadFlowHeader returns a flow's metadata (owner, org, version, …) WITHOUT
	// its content. Callers that only authorize, check existence, or read the
	// version should use this so they avoid a content fetch (and, for the
	// database backend, do not depend on blob-storage availability). Content is
	// always nil on the returned document.
	LoadFlowHeader(ctx context.Context, id string) (*FlowDocument, error)
	ListFlows(ctx context.Context, filter FlowFilter) ([]*FlowDocument, error)
	// CountFlows returns the total number of flows matching the filter,
	// ignoring Limit/Offset — used for list pagination totals.
	CountFlows(ctx context.Context, filter FlowFilter) (int, error)
	DeleteFlow(ctx context.Context, id string) error
}

// FlowVersionStore covers flow version history (snapshots taken on save).
type FlowVersionStore interface {
	// SaveFlowVersion assigns v.Version atomically (any caller-set value is
	// overwritten), so callers must read it back from v after a successful save.
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

// UserStore covers user account CRUD, role/password/profile updates, and the
// GDPR-related bulk operations (export, erasure, retention purge).
type UserStore interface {
	SaveUser(ctx context.Context, user *User) error
	CreateUser(ctx context.Context, user *User) error
	LoadUserByEmail(ctx context.Context, email string) (*User, error)
	LoadUserByID(ctx context.Context, id string) (*User, error)
	// LoadUsersByIDs resolves many users in one round trip. Missing IDs are
	// simply absent from the returned map (no error). Used to avoid N+1 lookups
	// when decorating lists (e.g. owner display names).
	LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*User, error)
	CountUsers(ctx context.Context) (int, error)
	// ListUsers returns up to limit users starting at offset, ordered by
	// created_at descending. A non-positive limit returns all users (used by
	// callers that only need to check for emptiness, e.g. first-admin bootstrap).
	ListUsers(ctx context.Context, limit, offset int) ([]*User, error)
	ListAdmins(ctx context.Context) ([]*User, error)
	// QueryUsers returns one page of the admin user list — the users matching
	// q's filters, ordered by created_at descending then id, sliced by
	// q.Offset/q.Limit — together with the number of matching users. The page
	// is never nil.
	QueryUsers(ctx context.Context, q UserQuery) ([]*User, int, error)
	// LoadUserStats counts the whole user table by role and state for the
	// admin overview.
	LoadUserStats(ctx context.Context) (*UserStats, error)
	UpdateUserRole(ctx context.Context, id string, role auth.Role) error
	UpdateUserPassword(ctx context.Context, id string, passwordHash string) error
	// UpdateUserProfile updates the user's display name and avatar URL. Either
	// value may be empty (to clear it).
	UpdateUserProfile(ctx context.Context, id string, displayName, avatarURL string) error

	// DeleteUser performs GDPR-style account erasure for the given user. It
	// deletes the user's owned flows (cascading versions/analysis/triage) and
	// all per-user rows (tokens, settings, usage, org memberships, invites for
	// their email), and anonymizes PII they authored on shared rows
	// (audit_events email/IP, flow_versions.created_by, finding_status
	// .updated_by) so the security/forensic trail is retained without personal
	// data. Idempotent: a missing user is not an error.
	DeleteUser(ctx context.Context, userID string) error
	// ExportUserData assembles a data-subject access / portability export for
	// the user (profile, owned flows, settings, audit history). Returns
	// ErrNotFound if the user does not exist.
	ExportUserData(ctx context.Context, userID string) (*UserDataExport, error)
	// PurgeExpiredData removes stale rows whose retention has elapsed: expired
	// refresh/api/user tokens, expired or accepted org invites, and audit_events
	// older than auditRetentionDays (0 = keep audit history indefinitely). It
	// returns counts of what was removed. Intended for a periodic background job.
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
	// ListCollaborators returns up to limit rows (limit <= 0 = all). The
	// handler clamps its response; without a limit here the store still
	// fetched and allocated the whole set first.
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

// DashboardStore covers the persisted flow-health snapshots and the rollup
// queries backing the portfolio/home dashboards.
type DashboardStore interface {
	SaveFlowAnalysis(ctx context.Context, fa *FlowAnalysis) error
	// LoadFlowHealth returns the most recent persisted analysis snapshot for a
	// single flow, or (nil, nil) when the flow has never been analyzed. Caller
	// authorization is the caller's responsibility — this is a pure read.
	LoadFlowHealth(ctx context.Context, flowID string) (*HealthSnapshot, error)
	// LoadFlowHealthBatch resolves health snapshots for many flows in one query
	// (avoids N+1 in the portfolio view). The returned map omits flows that have
	// never been analyzed; it is never nil. Caller authorization is the caller's
	// responsibility — pass only flow IDs the caller may see.
	LoadFlowHealthBatch(ctx context.Context, flowIDs []string) (map[string]*HealthSnapshot, error)
	FlowDashboardData(ctx context.Context, ownerID string, days int) (*DashboardData, error)
	// FlowDashboardAdvanced returns trend, cost-by-provider, rule-frequency,
	// activity-feed, complexity-scatter, and security-posture data.
	FlowDashboardAdvanced(ctx context.Context, ownerID string, days int) (*DashboardAdvancedData, error)

	// Analytics reads (GET /api/dashboard/analytics). The service authorises
	// the flow id set; these are pure reads scoped by it.
	LoadAnalysisSnapshots(ctx context.Context, flowIDs []string) ([]AnalysisSnapshotRow, error)
	LoadAnalysisHistory(ctx context.Context, w AnalyticsWindow) (*AnalysisHistoryRows, error)
	LoadTriageAnalytics(ctx context.Context, w AnalyticsWindow) (*TriageRows, error)
	// mode: "user" (the caller's calls), "org" (the scoped orgs'), "both".
	LoadUsageAnalytics(ctx context.Context, userID string, orgIDs []string, mode string, w AnalyticsWindow) (*UsageRows, error)
	LoadActivityAnalytics(ctx context.Context, userID string, w AnalyticsWindow, limit int) (*ActivityRows, error)
	LoadSecurityAnalytics(ctx context.Context, userID, email string, includeLocked bool) (*SecurityRows, error)
}

// AnalyticsWindow scopes an analytics read: the flow ids the caller may see
// (authorised by the service) and the instants of the current window and,
// when comparing, of the previous one. Half-open [From, To). TZ is the IANA
// zone timestamps are bucketed into days with.
type AnalyticsWindow struct {
	FlowIDs  []string
	From     time.Time
	To       time.Time
	PrevFrom time.Time
	PrevTo   time.Time
	TZ       string
}

// AnalysisSnapshotRow is one flow in scope with its latest persisted analysis
// (Analyzed=false when it never had one).
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

// AnalysisHistoryRow is a flow's latest analysis on one caller-local day
// (Runs = analyses that day), or — for the baseline — its latest analysis
// before the window (Day empty).
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

// UsageModelRow: ServedModel is the model that answered when it differs from
// Model (see UsageMetric.ServedModel); rows are grouped by it, so a deployment
// alias served by two models over the window yields two rows.
type UsageModelRow struct {
	Provider    string
	Model       string
	ServedModel string
	TokensIn    int
	TokensOut   int
	Calls       int
	Cost        float64
}

// UsageRows: Daily spans the previous and current windows; the breakdowns
// cover the current one.
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

// SecurityRows: LockedRead reports whether the locked-account count was read
// (only when asked for; it is instance-wide).
type SecurityRows struct {
	FailedLogins24h int
	LockedAccounts  int
	LockedRead      bool
}

// KnowledgeStore covers the org knowledge base (RAG source documents + chunks).
type KnowledgeStore interface {
	SaveKnowledgeDocument(ctx context.Context, doc *KnowledgeDocument) error
	// DeleteKnowledgeDocument removes a document only when it belongs to orgID,
	// so a caller scoped to one org cannot delete another org's documents.
	DeleteKnowledgeDocument(ctx context.Context, orgID, id string) error
	// DeleteKnowledgeDocumentByName removes every document with the given
	// filename in the org — re-upload REPLACE semantics (chunks cascade).
	DeleteKnowledgeDocumentByName(ctx context.Context, orgID, filename string) error
	// ListKnowledgeDocuments returns up to limit rows (limit <= 0 = all).
	ListKnowledgeDocuments(ctx context.Context, orgID string, limit int) ([]*KnowledgeDocument, error)
	// SaveKnowledgeChunks inserts chunk rows RLS-scoped to userID so the
	// knowledge_chunks WITH CHECK policy enforces org membership. userID is
	// the calling user's identity ("" only in non-authenticated local mode).
	SaveKnowledgeChunks(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]KnowledgeChunk, error)
	// ListKnowledgeChunkContents returns up to limit chunks (ID+DocID+Content,
	// no embeddings) strictly after the cursor, in (doc_id, id) order — one
	// page of a re-index walk after an embedding-provider change. limit <= 0
	// means the backend's page cap.
	ListKnowledgeChunkContents(ctx context.Context, orgID string, after KnowledgeChunkCursor, limit int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddings returns up to n of the org's chunks WITH
	// their stored embeddings (ID, DocID, Content, Embedding), one chunk per
	// document, deterministic order, pgvector-indexed picks first. The RAG
	// layer re-embeds their text with the caller's model to tell whether that
	// model produced the corpus (embedding-model mismatch guard). Chunks
	// without an embedding are skipped.
	SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddingsUpTo returns up to n of the org's chunks
	// at or before the cursor in (doc_id, id) order, NEAREST first, WITH
	// their stored embeddings (chunks without one are skipped). A resumed
	// re-index compares them — the chunks the previous request converted —
	// with the caller's model before continuing (rag.ReindexOrg).
	SampleKnowledgeChunkEmbeddingsUpTo(ctx context.Context, orgID string, upTo KnowledgeChunkCursor, n int) ([]KnowledgeChunk, error)
	// UpdateKnowledgeChunkEmbeddings replaces chunk embeddings in place
	// (RLS-scoped to userID like SaveKnowledgeChunks) and refreshes the
	// pgvector column for same-dimension vectors.
	UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	// CountKnowledgeChunks reports org totals: all chunks and how many are in
	// the pgvector index at the configured dimension (dim-mismatch detection).
	CountKnowledgeChunks(ctx context.Context, orgID string) (total, vectorIndexed int, err error)
}

// HealthChecker covers backend liveness/lifecycle, independent of any one
// storage domain — every backend must support it regardless of which other
// role interfaces it implements.
type HealthChecker interface {
	Ping(ctx context.Context) error
	Close() error
}

// AuditStore covers the append-only security/governance audit log.
type AuditStore interface {
	SaveAuditEvent(ctx context.Context, event *AuditEvent) error
	ListAuditEvents(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error)
}

// TriageStore covers finding triage state (status overrides) and per-flow
// accepted-findings baselines.
type TriageStore interface {
	// SetFindingStatus upserts the triage state for one finding (keyed by
	// FlowID + FindingKey).
	SetFindingStatus(ctx context.Context, st *FindingStatus) error
	// BatchSetFindingStatus upserts triage state for multiple findings of the
	// same flow atomically. Implementations must either commit all or none;
	// the caller can rely on a nil error meaning every item was persisted.
	// userID is the caller's identity (RLS scoping on Postgres); pass "" only
	// in non-authenticated local mode where RLS is disabled.
	BatchSetFindingStatus(ctx context.Context, flowID, userID string, items []*FindingStatus) error
	// ListFindingStatuses returns all persisted triage states for a flow.
	// Implementations return a non-nil (possibly empty) slice.
	ListFindingStatuses(ctx context.Context, flowID string) ([]*FindingStatus, error)
	// DeleteFindingStatus removes triage state for one finding, resetting it to
	// the implicit "open" state. Missing records are not an error (idempotent).
	DeleteFindingStatus(ctx context.Context, flowID, findingKey string) error
	// GetFlowBaseline returns the flow's accepted-findings baseline, or
	// (nil, nil) when none has been set.
	GetFlowBaseline(ctx context.Context, flowID string) (*FlowBaseline, error)
	// SetFlowBaseline replaces the flow's baseline (one baseline per flow).
	SetFlowBaseline(ctx context.Context, b *FlowBaseline) error
	// ClearFlowBaseline removes the flow's baseline. Idempotent.
	ClearFlowBaseline(ctx context.Context, flowID string) error
}

// CommentStore covers team-shared review threads on individual findings.
type CommentStore interface {
	AddFindingComment(ctx context.Context, c *FindingComment) error
	ListFindingComments(ctx context.Context, flowID, findingKey string) ([]*FindingComment, error)
	// DeleteFindingComment removes a comment. When authorID is non-empty the
	// delete only applies if the comment was written by that user; a mismatch
	// returns ErrNotCommentAuthor. Empty authorID deletes unconditionally
	// (flow-admin moderation). Deleting an absent comment is a no-op.
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
	// GetAPITokenByHash resolves a token by its hash for authentication, or
	// returns ErrNotFound. Revocation is a delete, so a revoked token is not found.
	GetAPITokenByHash(ctx context.Context, tokenHash string) (*APIToken, error)
	// ListAPITokens returns a user's tokens (metadata only; never the hash to clients).
	ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error)
	// DeleteAPIToken removes a token owned by userID. Scoped to the owner so one
	// user cannot revoke another's; idempotent.
	DeleteAPIToken(ctx context.Context, userID, id string) error
}

// UserTokenStore covers one-shot user tokens (password reset, email
// verification). Only the hash is stored; the raw value lives only in the
// email link sent to the user.
type UserTokenStore interface {
	CreateUserToken(ctx context.Context, t *UserToken) error
	// ConsumeUserToken atomically redeems a valid (unused, unexpired) token of
	// the given purpose, marks it used, and returns the owning user ID. It
	// returns ErrNotFound when no such token exists, so a token cannot be
	// replayed.
	ConsumeUserToken(ctx context.Context, purpose, tokenHash string) (userID string, err error)
	// InvalidateUserTokens marks every unused, unexpired token of the given
	// purposes for userID as used. Used when the user changes (or resets) their
	// password so that all other outstanding password-reset / email-verify
	// links for that account stop working immediately — otherwise an older
	// reset link leaked to an attacker would still redeem after the user
	// believes the account is recovered (account takeover). A no-op (nil error)
	// when no matching rows exist.
	InvalidateUserTokens(ctx context.Context, userID string, purposes ...string) error
	// SetUserEmailVerified marks a user's email as verified.
	SetUserEmailVerified(ctx context.Context, userID string) error
}

// StorageBackend is the full storage capability set, composed from the role
// interfaces above. It exists so the DI container and the two concrete
// backends (Postgres, filesystem) have one thing to implement/inject; most
// consumers should instead depend on the narrowest role interface(s) they
// actually use (e.g. ConversationStore, DashboardStore) so a change to one
// domain doesn't ripple through unrelated callers.
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

// PolicyStore covers org-scoped governance policies (named rule sets with a
// pass/fail gate severity). Policies are team-shared in cloud mode; the
// filesystem backend stubs these as no-ops (local mode has no orgs).
type PolicyStore interface {
	SavePolicy(ctx context.Context, p *models.Policy) error
	GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error)
	ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error)
	DeletePolicy(ctx context.Context, orgID, id string) error
}

// GovernanceAlertStore is the in-app inbox for governance signals (baseline
// drift, health regressions) detected by the continuous-governance scanner.
// Alerts are team-shared: anyone who can see a flow can see and acknowledge its
// alerts (mirroring the finding_status/comment visibility model). The scanner
// writes alerts (system context, no RLS principal); users read/ack/dismiss them
// through the API (RLS-scoped to flows they can see).
type GovernanceAlertStore interface {
	// RecordGovernanceAlert persists a new alert. The ID and CreatedAt are
	// stamped by the caller (the scanner); the store treats ID as the primary
	// key. A duplicate ID is a no-op (the scanner is de-duplicated upstream, but
	// this keeps a retry safe).
	RecordGovernanceAlert(ctx context.Context, a *GovernanceAlert) error
	// ListGovernanceAlerts returns alerts visible to the caller, newest first,
	// respecting the filter's pagination + include-dismissed flag. Implementations
	// return a non-nil (possibly empty) slice.
	ListGovernanceAlerts(ctx context.Context, filter GovernanceAlertFilter) ([]*GovernanceAlert, error)
	// UnreadGovernanceAlertCount returns the number of visible alerts that have
	// not yet been acknowledged (read_at IS NULL and not dismissed).
	UnreadGovernanceAlertCount(ctx context.Context) (int, error)
	// UnreadGovernanceAlertCountFor scopes the badge count to one caller:
	// team-wide alerts plus that user's targeted ones.
	UnreadGovernanceAlertCountFor(ctx context.Context, userID string) (int, error)
	// MarkGovernanceAlertRead stamps read_at = NOW() on one alert. Idempotent.
	MarkGovernanceAlertRead(ctx context.Context, userID, alertID string) error
	// MarkAllGovernanceAlertsRead stamps read_at = NOW() on every visible unread
	// alert (the "open the panel → clear the badge" action). Idempotent.
	MarkAllGovernanceAlertsRead(ctx context.Context, userID string) error
	// DismissGovernanceAlert stamps dismissed_at = NOW() on one alert. A dismissed
	// alert is hidden from the default list view but not deleted. Idempotent.
	DismissGovernanceAlert(ctx context.Context, userID, alertID string) error
	// ClearGovernanceAlerts permanently deletes the caller's visible alerts that
	// have been dismissed. Non-dismissed alerts are retained.
	ClearGovernanceAlerts(ctx context.Context, userID string) error
}

// UserToken purposes.
const (
	TokenPurposePasswordReset = "password_reset"
	TokenPurposeEmailVerify   = "email_verify"
)

// UserToken is a single-use credential for an out-of-band flow (password reset
// or email verification). Stored hashed; redeemed via ConsumeUserToken.
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

// OrgInvite represents a pending (or resolved) invitation for a user to join
// an organisation. The raw invite token is never persisted — only its SHA-256
// hash (TokenHash), so a database read alone cannot be used to join the org.
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

// FlowSort enumerates the supported sort orders for ListFlows. The zero value
// (FlowSortUpdatedDesc) is the historical default.
type FlowSort int

const (
	FlowSortUpdatedDesc FlowSort = iota
	FlowSortUpdatedAsc
	FlowSortNameAsc
	FlowSortNameDesc
	FlowSortBlocksDesc
	// FlowSortIDAsc orders by the primary key. It is not a user-facing sort —
	// it exists for callers that WALK THE WHOLE TABLE with LIMIT/OFFSET and
	// need each row exactly once: the storage migrator and the governance
	// scanner.
	//
	// Every other sort orders by a MUTABLE column. Under offset pagination that
	// is unsound even with a unique tiebreaker: a flow saved mid-walk gets a
	// fresh updated_at, jumps to the front of an `updated_at DESC` ordering, and
	// shifts every later row down one — so the row sitting on the next page
	// boundary is never returned. Verified against Postgres: a single UPDATE
	// between two pages of a 30-row walk skipped one row and repeated another.
	// id never changes, so an UPDATE cannot move a row in this ordering.
	//
	// Residual, deliberately not solved here: rows INSERTED mid-walk can still
	// land before the cursor and shift it. Truly exact enumeration needs keyset
	// pagination (WHERE id > $last). For the migrator the source is quiesced;
	// for the scanner a brand-new flow has no baseline to drift from and is
	// picked up on the next tick.
	FlowSortIDAsc
)

// FlowFilter defines filtering options for listing flows
type FlowFilter struct {
	UserID         string
	OrganizationID string
	// OrganizationIDs widens the org scope to multiple orgs the caller belongs
	// to. When non-empty, a flow matches if its org_id is in this list (in
	// addition to UserID-owned and collaborator matches). Service-layer code
	// must verify membership before populating this — handlers must never bind
	// it directly from user input.
	OrganizationIDs []string
	Query           string
	CreatedAfter    *time.Time
	CreatedBefore   *time.Time
	Limit           int
	Offset          int
	SortBy          FlowSort
	// MetadataOnly skips loading each flow's (potentially large) Content when
	// the caller only needs listing metadata. Backends leave FlowDocument.Content
	// empty when set. List/library endpoints set this; the migrator does not.
	MetadataOnly bool
	// AllFlows is an explicit opt-in for operational enumeration (e.g. the
	// filesystem→cloud migrator) that bypasses owner/org scoping and returns
	// every flow. Without it, a filter that sets neither UserID nor
	// OrganizationID matches nothing — a defense-in-depth guard so a caller
	// who forgets to scope cannot accidentally dump the whole table. This flag
	// is set only by server-side operational code and must never be bound from
	// user-controlled input.
	AllFlows bool
	// SharedOnly limits results to flows where the caller is a collaborator
	// (not the owner and not via org membership). Used by the "Shared with me"
	// scope in the library UI.
	SharedOnly bool
	// Tags matches flows carrying ANY of the listed tags (a tag-picker's
	// any-of semantics). Normalization is the storage layer's concern.
	Tags []string
}

// ErrContentSearchUnsupported is returned by SearchFlowContents on backends
// that cannot push content matching into the store (no content column —
// filesystem backends; or content offloaded to blob storage, where the DB
// holds only a placeholder). Callers fall back to their legacy scan.
var ErrContentSearchUnsupported = errors.New("content search not supported by this backend")

// HealthSnapshot is the persisted per-flow analysis summary surfaced on the
// single-flow GET. A nil pointer means the flow has never been analyzed.
type HealthSnapshot struct {
	HealthScore int       `json:"healthScore"`
	Errors      int       `json:"errors"`
	Warnings    int       `json:"warnings"`
	Info        int       `json:"info"`
	AnalyzedAt  time.Time `json:"analyzedAt"`
}

// UsageMetric is one AI call's usage row. Model is the id the caller asked
// for; ServedModel is the model the provider reported answering when it
// differs (an Azure AI Foundry deployment alias, a model-router route, a
// Foundry agent's underlying model) and empty when unknown or identical.
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

// FlowAnalysis is the persisted summary of a flow's most recent analysis run,
// upserted on every analyze so the welcome dashboard can render health/findings
// aggregates that survive restarts and span replicas (instead of relying on the
// in-memory, per-process analyzer cache). ByCategory maps finding category →
// count. Owner/org scoping is resolved by JOINing flows at read time, so this
// row intentionally does not denormalize ownership.
type FlowAnalysis struct {
	FlowID      string         `json:"flowId"`
	HealthScore int            `json:"healthScore"`
	Errors      int            `json:"errors"`
	Warnings    int            `json:"warnings"`
	Info        int            `json:"info"`
	ByCategory  map[string]int `json:"byCategory"`
	ByRule      map[string]int `json:"byRule"`
	AnalyzedAt  time.Time      `json:"analyzedAt"`
	// ByConfidence distributes this flow's findings across confidence tiers
	// (high/medium/low). Persisted so the dashboard can roll up a "how much to
	// trust these results" donut across the org without re-analyzing.
	ByConfidence map[string]int `json:"byConfidence,omitempty"`
	// AutoFixableCount is how many of this flow's findings carry a one-click
	// deterministic fix (Finding.AutoFix != ""). Rolled up into the dashboard's
	// "fix availability" KPI.
	AutoFixableCount int `json:"autoFixableCount,omitempty"`
	// TotalFindings is the sum of Errors+Warnings+Info, denormalized so org-wide
	// SUMs avoid repeating the per-row arithmetic in SQL.
	TotalFindings int `json:"totalFindings,omitempty"`
}

// RecentFlowHealth is one row of the dashboard's "recent flows" list: a flow
// with its latest persisted health score (nil when the flow has never been
// analyzed — a LEFT JOIN miss).
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

// DashboardData is the storage-layer aggregate that backs GET /api/dashboard/home
// in cloud mode. The service maps it to models.DashboardHomeData. Sections that
// have no data are zero/empty with their availability conveyed by HealthCount
// (0 ⇒ nothing analyzed yet) and len(TokenUsage).
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
	// TotalFindings is the org-wide sum of all findings; AutoFixable is how
	// many carry a one-click deterministic fix. Confidence distributes those
	// findings across certainty tiers. Backs the dashboard's fix-availability
	// and confidence-distribution KPIs.
	TotalFindings int
	AutoFixable   int
	Confidence    map[string]int
	// HealthBuckets is the org-wide health-score histogram (5 buckets of 20),
	// exposing the distribution the single AvgHealth number hides.
	HealthBuckets []HealthBucket
}

// DailySeverityPoint is one day of the org-wide severity trend (error/warning/info
// summed across every flow analyzed that day). Backs the stacked-area trend chart.
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

// DashboardAdvancedData extends DashboardData with trend, cost, rule, activity,
// complexity, and security sections. Returned by FlowDashboardAdvanced.
type DashboardAdvancedData struct {
	HealthTrend []DailyHealthPoint
	CostByProv  []ProviderCost
	RuleFreq    []RuleFrequency
	Activity    []ActivityEntry
	Complexity  []FlowComplexityPoint
	Security    DashboardSecurity
	// SeverityTrend is the org-wide daily error/warning/info series for the
	// stacked-area "is my fleet getting healthier?" chart.
	SeverityTrend []DailySeverityPoint
	// Workflow is the team-triage funnel + resolution stats (MTTR, stale). Cloud-
	// only: local mode has no persistent triage, so the service leaves Available
	// false and the UI shows a placeholder.
	Workflow WorkflowData
}

// WorkflowData holds the cloud-mode team-triage analytics: how findings are
// distributed across triage states, the mean time to resolve, and how many
// findings have been sitting open long enough to count as stale.
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

// RuleFrequency is one rule's finding count across all of the owner's flows.
// Severity tinting is a catalog concern, resolved in the service layer.
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

// DashboardSecurity summarizes the security posture for the dashboard.
// Available is false when any of its counts could not be read: each is a
// separate best-effort query, and a failed one leaves its field at 0 — which
// the UI would otherwise present as "no security issues detected".
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
	// ChunkCount / VectorIndexed are list-view extras (filled by
	// ListKnowledgeDocuments): how many chunks the doc has and how many of
	// those are in the pgvector index (same dimension as configured). A doc
	// with chunks but 0 indexed is stranded by an embedding-provider switch —
	// the management UI surfaces it and offers re-index.
	ChunkCount    int `json:"chunkCount,omitempty"`
	VectorIndexed int `json:"vectorIndexed,omitempty"`
}

type KnowledgeChunk struct {
	ID        string    `json:"id"`
	DocID     string    `json:"docId"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
	// Filename of the source document (joined at retrieval time) so the RAG
	// assembly can attribute each chunk to its source — empty when unknown.
	Filename string `json:"filename,omitempty"`
}

// KnowledgeChunkCursor is a keyset position in an org's chunks ordered by
// (doc_id, id); the zero value starts from the beginning. Both columns are
// TEXT, so "" sorts before every real value. It is also the re-index
// resume token (rag.ReindexResult.NextCursor, the re-index body's
// "cursor"), hence the JSON tags; clients treat it as opaque.
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
	// Source is the raw PAD source text (single-file flows). Cloud-mode
	// apply-fix/preview-fix patch this line-based text and re-parse, rather than
	// the parsed Content. Empty for legacy rows and multi-file flows (deferred).
	Source         string       `json:"source,omitempty"`
	Metadata       FlowMetadata `json:"metadata"`
	OwnerID        string       `json:"ownerId"`
	OrganizationID string       `json:"organizationId"`
	CreatedAt      time.Time    `json:"createdAt"`
	UpdatedAt      time.Time    `json:"updatedAt"`
	Version        int          `json:"version"`
	// Tags are the flow's organizational labels (business unit, criticality,
	// environment) — normalized by storage (lowercase, comma-free, ≤32
	// chars, ≤20 tags). Empty for untagged flows.
	Tags []string `json:"tags,omitempty"`
}

// FlowMetadata contains metadata about a flow
// FlowMetadata is stored as the flows.metadata JSON column and READ BY SQL
// (ORDER BY metadata->>'BlockCount', the dashboard's subflow sums, and an index
// built on that expression). Its JSON keys are therefore a storage contract:
// renaming them silently turns those queries into zeroes. API responses that
// need camelCase shape it at the boundary (see the version view in
// internal/api), they do not retag this struct.
type FlowMetadata struct {
	BlockCount   int
	SubflowCount int
	MaxDepth     int
	ParsedAt     time.Time
	FileSize     int
	RawLineCount int
}

// AppSettings represents application settings.
//
// IMPORTANT: this struct's field set and JSON tags MUST stay in parity with
// models.AppSettings. SystemService.{toModel,fromModel} bridge the two via a
// JSON round-trip, so any field present on one but missing (or mis-tagged) on
// the other is silently dropped when settings are persisted in cloud/database
// mode. service.TestSettingsRoundTrip guards this invariant.
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

// ChatMessage represents a chat message. It is kept in field/JSON-tag parity
// with pad-core/models.ChatMessage (the domain type) so the service-layer bridge
// (toStorageMessages/toModelMessages) is lossless. Timestamp is an RFC3339 string
// here; the bridge formats/parses it from the model's time.Time.
// ToolCallRecord mirrors models.ToolCallRecord (same JSON tags) — the
// per-message tool transparency trail persisted with the conversation.
type ToolCallRecord struct {
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	Ok         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// FixItemSnapshot mirrors models.FixItemSnapshot — one fix inside a batch
// approval record.
type FixItemSnapshot struct {
	RuleID     string `json:"ruleId"`
	FixType    string `json:"fixType"`
	BlockLabel string `json:"blockLabel"`
	Line       int    `json:"line"`
	Summary    string `json:"summary"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// FixProposalSnapshot mirrors models.FixProposalSnapshot — the persisted
// apply_fix approval record (single or batch).
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

// ChatMessage mirrors models.ChatMessage for backend persistence (the
// service bridges them field by field). ServedModel and the billed rates
// ride inside the conversation's messages JSON — no column of their own.
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

// Settings types — kept in parity with the matching types in internal/models
// (same fields, same JSON tags) so the toModel/fromModel JSON bridge is lossless.
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
	// EmbeddingModel mirrors models.AISettings.EmbeddingModel — keep the JSON tags
	// in parity or the settings round-trip drops the value silently.
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
	// ReasoningEffort mirrors models.AIProviderConfig.ReasoningEffort — keep
	// the JSON tags in parity or the settings round-trip drops the value
	// silently.
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

// AuditEvent records a user action for compliance and security visibility.
// AuditEvent is one entry of the audit trail. The on-disk spill queue persists
// these as JSON lines, so the field names are a storage contract too; the admin
// API renders them through a view (internal/api) that carries the camelCase
// keys clients read.
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
	// IncludeSubject also matches events whose subject is UserID (resource_type
	// "user" and resource_id UserID): sign-ins, failed sign-ins, lockouts,
	// registration and SSO logins carry no actor, so a plain UserID filter
	// misses them. The admin audit log sets it; the GDPR export must not, so a
	// user's export never carries the IP of whoever failed to sign in as them.
	IncludeSubject bool
}

// FlowVersion stores a snapshot of a flow at a point in time.
// FlowVersion is one stored snapshot of a flow. Rendered through an API view
// (internal/api) rather than directly: the nested FlowMetadata carries storage
// JSON keys that SQL depends on, so the wire shape is built at the boundary.
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

// FindingStatus is the persisted, team-shared triage state for one finding,
// keyed by the flow and the finding's stable key (models.Finding.Key —
// "ruleID:blockID"). Because the key is content-derived (not a per-run
// positional index), triage state survives re-analysis: suppressing or
// resolving a finding sticks even as other findings come and go.
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

// GovernanceAlert is one in-app governance signal (baseline drift or health
// regression) for a flow, written by the continuous-governance scanner and
// surfaced in the notifications bell. Alerts are team-shared: visible to anyone
// who can see the flow (RLS inherits flow visibility, same as finding_status).
// The read/dismiss timestamps are team-global — acknowledging an alert clears
// the badge for the whole team (governance/incident-ack semantics).
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
	// TargetUser is the delivery target for PERSONAL alerts (assignment/
	// comment notifications): the alert is visible only to that user. Empty =
	// team-wide (governance semantics — visible to anyone who can see the
	// flow).
	TargetUser string `json:"targetUser,omitempty"`
}

// GovernanceAlertFilter narrows a ListGovernanceAlerts query. Limit/Offset
// paginate (Limit <= 0 ⇒ a sensible default cap). IncludeDismissed controls
// whether dismissed alerts appear (default false hides them). UserID scopes
// visibility: an alert matches when its TargetUser is empty (team-wide) OR
// equals UserID. Empty UserID matches ONLY team-wide alerts.
type GovernanceAlertFilter struct {
	Limit            int
	Offset           int
	IncludeDismissed bool
	UserID           string
}

// OrgChannel is one org-configured notification destination (R2-3): governance
// events for the org's flows are delivered here in addition to the
// deployment-global channels. URL kinds: webhook / teams / slack.
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

// OrgCustomRule is one user-authored analyzer rule owned by an org.
//
// Config is an analyzer.CustomRuleConfig serialized as JSON. It is carried
// opaquely here on purpose: the rule schema belongs to the analyzer, and the
// storage layer should not need a release to learn a new matcher field. The API
// validates every write through the analyzer before it reaches storage, so an
// unparseable config cannot land.
//
// RuleID is the author's rule id — the one that appears on findings. It is
// unique PER ORG, not globally: two orgs may both define "house-style", and the
// analysis cache accounts for that (analyzer.ruleSetDigest folds each rule's
// full config, not just its id).
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
	// SaveOrgCustomRule upserts a rule. The (org_id, rule_id) pair is unique,
	// so re-saving the same author id under one org replaces it; the same id
	// under a DIFFERENT org is a distinct rule and must not collide.
	SaveOrgCustomRule(ctx context.Context, rule *OrgCustomRule) error
	DeleteOrgCustomRule(ctx context.Context, orgID, id string) error
	// ListOrgCustomRules returns the org's rules; enabledOnly for the analysis
	// path, which must not compile disabled rules.
	ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgCustomRule, error)
}

// OrgChannelStore covers per-org notification channel management.
type OrgChannelStore interface {
	// SaveOrgChannel upserts a channel (ID-keyed). Kind/URL validation is the
	// caller's (API) concern; storage persists what it's given.
	SaveOrgChannel(ctx context.Context, ch *OrgChannel) error
	DeleteOrgChannel(ctx context.Context, orgID, id string) error
	// ListOrgChannels returns the org's channels; enabledOnly for dispatch.
	ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgChannel, error)
}

// APIToken is a scoped, revocable machine credential (personal access token)
// that authenticates programmatic API calls as its owning user — no interactive
// login. Only the token's SHA-256 hash is persisted; the raw value is shown once
// at creation and is unrecoverable afterwards. Revocation deletes the row.
type APIToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Name      string     `json:"name"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil ⇒ no expiry
	// Scopes restricts a PAT's capabilities (empty = unscoped = full access).
	// Members of auth.ValidTokenScopes only.
	Scopes []string `json:"scopes,omitempty"`
}

// FlowBaseline is the set of finding keys accepted as a flow's baseline.
// Findings whose key is in the set are "known"; anything else is "new since
// baseline" — the basis for ratcheting/gating (only fail on new findings).
type FlowBaseline struct {
	FlowID    string    `json:"flowId"`
	Keys      []string  `json:"keys"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// FindingComment is a team-shared review comment on a single finding, keyed by
// the finding's stable key (same key used for triage). Comments persist across
// re-analysis and are visible to all flow collaborators.
type FindingComment struct {
	ID         string    `json:"id"`
	FlowID     string    `json:"flowId"`
	FindingKey string    `json:"findingKey"`
	AuthorID   string    `json:"authorId"`
	AuthorName string    `json:"authorName,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ShareToken is a revocable, read-only public link to a flow's findings report.
// Only the token's SHA-256 hash is persisted; the raw value is shown once at
// creation. An unauthenticated viewer redeems the hash for a snapshot of the
// flow's current analysis report.
type ShareToken struct {
	ID        string     `json:"id"`
	FlowID    string     `json:"flowId"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedBy string     `json:"createdBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // nil ⇒ no expiry
}

// ── Flow-tag normalization (R2-4) ───────────────────────────────────────────
//
// Shared by every backend so the wire contract is identical: tags are
// lowercase letters/digits/'-'/'_', ≤32 chars each, ≤20 per flow, deduped.

const (
	MaxFlowTags   = 20
	MaxFlowTagLen = 32
)

// NormalizeFlowTags canonicalizes a tag list: trim, lowercase, drop empties
// and duplicates, enforce the character whitelist and the per-tag/per-flow
// caps. Returns an error naming the first rejected tag.
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
