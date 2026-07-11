package interfaces

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"pad-analyzer/internal/auth"
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
	User        *User           `json:"user"`
	Flows       []*FlowDocument `json:"flows"`
	Settings    *AppSettings    `json:"settings"`
	AuditEvents []*AuditEvent   `json:"auditEvents"`
	APITokens   []*APIToken     `json:"apiTokens"`
	ExportedAt  time.Time       `json:"exportedAt"`
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
}

// StorageBackend defines the interface for storage implementations
// This allows abstraction between local file system and cloud database storage
type StorageBackend interface {
	// Flow document operations
	SaveFlow(ctx context.Context, flow *FlowDocument) error
	// TransferFlowOwner changes owner_id and org_id on a flow. This is the
	// ONLY way to reassign ownership — SaveFlow's ON CONFLICT intentionally
	// preserves the original owner_id/org_id to prevent hijacking.
	TransferFlowOwner(ctx context.Context, flowID, newOwnerID, newOrgID string) error
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

	// Settings operations
	SaveSettings(ctx context.Context, settings *AppSettings) error
	LoadSettings(ctx context.Context) (*AppSettings, error)
	SaveUserSettings(ctx context.Context, userID string, settings *AppSettings) error
	LoadUserSettings(ctx context.Context, userID string) (*AppSettings, error)
	SaveOrgSettings(ctx context.Context, orgID string, settings *AppSettings) error
	LoadOrgSettings(ctx context.Context, orgID string) (*AppSettings, error)

	// Conversation operations
	SaveConversation(ctx context.Context, flowID, scope string, messages []ChatMessage) error
	LoadConversation(ctx context.Context, flowID, scope string) ([]ChatMessage, error)
	DeleteConversation(ctx context.Context, flowID, scope string) error

	// User operations
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

	// Organisation operations
	SaveOrg(ctx context.Context, org *Organisation) error
	LoadOrg(ctx context.Context, id string) (*Organisation, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*Organisation, error)
	DeleteOrg(ctx context.Context, id string) error

	// Sharing operations
	ListCollaborators(ctx context.Context, flowID string) ([]*Collaborator, error)
	ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*Collaborator, error)
	AddCollaborator(ctx context.Context, flowID string, c *Collaborator) error
	UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error
	RemoveCollaborator(ctx context.Context, flowID, userID string) error

	// Usage tracking
	SaveUsageMetric(ctx context.Context, metric *UsageMetric) error
	GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error)

	// Dashboard
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

	// Knowledge Base
	SaveKnowledgeDocument(ctx context.Context, doc *KnowledgeDocument) error
	// DeleteKnowledgeDocument removes a document only when it belongs to orgID,
	// so a caller scoped to one org cannot delete another org's documents.
	DeleteKnowledgeDocument(ctx context.Context, orgID, id string) error
	ListKnowledgeDocuments(ctx context.Context, orgID string) ([]*KnowledgeDocument, error)
	SaveKnowledgeChunks(ctx context.Context, chunks []KnowledgeChunk) error
	SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]KnowledgeChunk, error)

	// Health check
	Ping(ctx context.Context) error
	Close() error

	// Audit log
	SaveAuditEvent(ctx context.Context, event *AuditEvent) error
	ListAuditEvents(ctx context.Context, filter AuditFilter) ([]*AuditEvent, error)

	// Flow versioning. SaveFlowVersion assigns v.Version atomically (any
	// caller-set value is overwritten), so callers must read it back from v
	// after a successful save.
	SaveFlowVersion(ctx context.Context, v *FlowVersion) error
	ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*FlowVersion, error)
	LoadFlowVersion(ctx context.Context, flowID string, version int) (*FlowVersion, error)

	// Finding triage & baselines
	// SetFindingStatus upserts the triage state for one finding (keyed by
	// FlowID + FindingKey).
	SetFindingStatus(ctx context.Context, st *FindingStatus) error
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

	// Finding comments (team-shared review threads on individual findings)
	AddFindingComment(ctx context.Context, c *FindingComment) error
	ListFindingComments(ctx context.Context, flowID, findingKey string) ([]*FindingComment, error)
	// DeleteFindingComment removes a comment. When authorID is non-empty the
	// delete only applies if the comment was written by that user; a mismatch
	// returns ErrNotCommentAuthor. Empty authorID deletes unconditionally
	// (flow-admin moderation). Deleting an absent comment is a no-op.
	DeleteFindingComment(ctx context.Context, flowID, commentID, authorID string) error

	// Share tokens (read-only public report links)
	CreateShareToken(ctx context.Context, t *ShareToken) error
	GetShareTokenByHash(ctx context.Context, tokenHash string) (*ShareToken, error)
	ListShareTokens(ctx context.Context, flowID string) ([]*ShareToken, error)
	RevokeShareToken(ctx context.Context, flowID, tokenID string) error

	// API tokens (machine credentials)
	CreateAPIToken(ctx context.Context, t *APIToken) error
	// GetAPITokenByHash resolves a token by its hash for authentication, or
	// returns ErrNotFound. Revocation is a delete, so a revoked token is not found.
	GetAPITokenByHash(ctx context.Context, tokenHash string) (*APIToken, error)
	// ListAPITokens returns a user's tokens (metadata only; never the hash to clients).
	ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error)
	// DeleteAPIToken removes a token owned by userID. Scoped to the owner so one
	// user cannot revoke another's; idempotent.
	DeleteAPIToken(ctx context.Context, userID, id string) error

	// One-shot user tokens (password reset, email verification). Only the hash
	// is stored; the raw value lives only in the email link sent to the user.
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
}

// HealthSnapshot is the persisted per-flow analysis summary surfaced on the
// single-flow GET. A nil pointer means the flow has never been analyzed.
type HealthSnapshot struct {
	HealthScore int       `json:"healthScore"`
	Errors      int       `json:"errors"`
	Warnings    int       `json:"warnings"`
	Info        int       `json:"info"`
	AnalyzedAt  time.Time `json:"analyzedAt"`
}

type UsageMetric struct {
	ID               string    `json:"id"`
	UserID           string    `json:"userId"`
	OrgID            string    `json:"orgId"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
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
type DashboardSecurity struct {
	FailedLogins24h    int
	LockedAccounts     int
	CredentialFindings int
}

type KnowledgeDocument struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Filename  string    `json:"filename"`
	CreatedAt time.Time `json:"createdAt"`
}

type KnowledgeChunk struct {
	ID        string    `json:"id"`
	DocID     string    `json:"docId"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
}

// FlowDocument represents a flow document
type FlowDocument struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Content        json.RawMessage `json:"content"`
	Metadata       FlowMetadata    `json:"metadata"`
	OwnerID        string          `json:"ownerId"`
	OrganizationID string          `json:"organizationId"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Version        int             `json:"version"`
}

// FlowMetadata contains metadata about a flow
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
type ChatMessage struct {
	ID               string `json:"id"`
	Role             string `json:"role"`
	Content          string `json:"content"`
	Timestamp        string `json:"timestamp"`
	ContextBlockID   string `json:"contextBlockId,omitempty"`
	ContextSubflowID string `json:"contextSubflowId,omitempty"`
	TokensIn         int    `json:"tokensIn,omitempty"`
	TokensOut        int    `json:"tokensOut,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	FinishReason     string `json:"finishReason,omitempty"`
}

// Settings types — kept in parity with the matching types in internal/models
// (same fields, same JSON tags) so the toModel/fromModel JSON bridge is lossless.
type GeneralSettings struct {
	FirstRunCompleted bool   `json:"firstRunCompleted"`
	LastUsedVersion   string `json:"lastUsedVersion"`
	CheckForUpdates   string `json:"checkForUpdates"`
	OpenInNewWindow   bool   `json:"openInNewWindow"`
}

type AppearanceSettings struct {
	Theme        string `json:"theme"`
	Density      string `json:"density"`
	CodeFont     string `json:"codeFont"`
	UIFont       string `json:"uiFont"`
	ReduceMotion bool   `json:"reduceMotion"`
	HighContrast bool   `json:"highContrast"`
}

type LayoutSettings struct {
	SidebarWidth           int    `json:"sidebarWidth"`
	InspectorWidth         int    `json:"inspectorWidth"`
	SidebarCollapsed       bool   `json:"sidebarCollapsed"`
	InspectorCollapsed     bool   `json:"inspectorCollapsed"`
	LastActiveInspectorTab string `json:"lastActiveInspectorTab"`
	LastViewMode           string `json:"lastViewMode"`
	ChatPanelHeight        *int   `json:"chatPanelHeight,omitempty"`
}

type AIPromptsConfig struct {
	Block             []string `json:"block"`
	Flow              []string `json:"flow"`
	Finding           []string `json:"finding"`
	BlockWithFindings []string `json:"blockWithFindings"`
}

type AISettings struct {
	ActiveProvider          string                      `json:"activeProvider"`
	EmbeddingProvider       string                      `json:"embeddingProvider"`
	Providers               map[string]AIProviderConfig `json:"providers"`
	DemoMode                DemoModeSettings            `json:"demoMode"`
	ShowCostEstimates       bool                        `json:"showCostEstimates"`
	SaveConversationHistory bool                        `json:"saveConversationHistory"`
	SystemPromptSuffix      string                      `json:"systemPromptSuffix,omitempty"`
	DailyBudget             float64                     `json:"dailyBudget"`
	Prompts                 AIPromptsConfig             `json:"prompts"`
}

type AIProviderConfig struct {
	Enabled            bool    `json:"enabled"`
	DefaultModel       string  `json:"defaultModel"`
	Temperature        float64 `json:"temperature"`
	MaxTokens          int     `json:"maxTokens"`
	ContextTokenBudget int     `json:"contextTokenBudget"`
}

type ParserSettings struct {
	MaxFileSizeMB     int  `json:"maxFileSizeMB"`
	PreserveComments  bool `json:"preserveComments"`
	TreatTabsAsSpaces bool `json:"treatTabsAsSpaces"`
	SpacesPerIndent   int  `json:"spacesPerIndent"`
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

type DemoModeSettings struct {
	Enabled    bool   `json:"enabled"`
	DailyLimit int    `json:"dailyLimit"`
	DailyUsed  int    `json:"dailyUsed"`
	ResetDate  string `json:"resetDate"`
}

// AuditEvent records a user action for compliance and security visibility.
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
}

// FlowVersion stores a snapshot of a flow at a point in time.
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
