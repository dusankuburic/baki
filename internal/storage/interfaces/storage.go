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

// User represents a system user in the storage backend.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Password  string    `json:"-"` // Bcrypt hash — never serialized to clients
	Role      auth.Role `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// StorageBackend defines the interface for storage implementations
// This allows abstraction between local file system and cloud database storage
type StorageBackend interface {
	// Flow document operations
	SaveFlow(ctx context.Context, flow *FlowDocument) error
	LoadFlow(ctx context.Context, id string) (*FlowDocument, error)
	ListFlows(ctx context.Context, filter FlowFilter) ([]*FlowDocument, error)
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
	ListUsers(ctx context.Context) ([]*User, error)
	ListAdmins(ctx context.Context) ([]*User, error)
	UpdateUserRole(ctx context.Context, id string, role auth.Role) error
	UpdateUserPassword(ctx context.Context, id string, passwordHash string) error

	// Organisation operations
	SaveOrg(ctx context.Context, org *Organisation) error
	LoadOrg(ctx context.Context, id string) (*Organisation, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*Organisation, error)
	DeleteOrg(ctx context.Context, id string) error

	// Sharing operations
	ListCollaborators(ctx context.Context, flowID string) ([]*Collaborator, error)
	AddCollaborator(ctx context.Context, flowID string, c *Collaborator) error
	UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error
	RemoveCollaborator(ctx context.Context, flowID, userID string) error

	// Usage tracking
	SaveUsageMetric(ctx context.Context, metric *UsageMetric) error
	GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error)

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

	// Flow versioning
	SaveFlowVersion(ctx context.Context, v *FlowVersion) error
	ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*FlowVersion, error)
	LoadFlowVersion(ctx context.Context, flowID string, version int) (*FlowVersion, error)
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

// Collaborator represents a user with access to a specific flow.
type Collaborator struct {
	UserID     string    `json:"userId"`
	Email      string    `json:"email"`
	Permission string    `json:"permission"`
	GrantedAt  time.Time `json:"grantedAt"`
}

// FlowFilter defines filtering options for listing flows
type FlowFilter struct {
	UserID         string
	OrganizationID string
	Query          string
	IsPublic       *bool
	CreatedAfter   *time.Time
	CreatedBefore  *time.Time
	Limit          int
	Offset         int
	// MetadataOnly skips loading each flow's (potentially large) Content when
	// the caller only needs listing metadata. Backends leave FlowDocument.Content
	// empty when set. List/library endpoints set this; the migrator does not.
	MetadataOnly bool
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
	ID             string
	Name           string
	Description    string
	Content        json.RawMessage
	Metadata       FlowMetadata
	OwnerID        string
	OrganizationID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
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

// ChatMessage represents a chat message
type ChatMessage struct {
	ID        string
	Role      string
	Content   string
	Timestamp string
	TokensIn  int
	TokensOut int
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
