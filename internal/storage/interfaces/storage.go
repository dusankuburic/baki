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

// User represents a system user in the storage backend.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Password  string    `json:"-"` // Bcrypt hash — never serialized to clients
	Role      auth.Role `json:"role"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
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

	// Conversation operations
	SaveConversation(ctx context.Context, flowID, scope string, messages []ChatMessage) error
	LoadConversation(ctx context.Context, flowID, scope string) ([]ChatMessage, error)

	// User operations
	SaveUser(ctx context.Context, user *User) error
	LoadUserByEmail(ctx context.Context, email string) (*User, error)
	LoadUserByID(ctx context.Context, id string) (*User, error)
	CountUsers(ctx context.Context) (int, error)
	ListUsers(ctx context.Context) ([]*User, error)

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

	// Health check
	Ping(ctx context.Context) error
	Close() error
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
	Query          string     // case-insensitive name substring match
	IsPublic       *bool
	CreatedAfter   *time.Time
	CreatedBefore  *time.Time
	Limit          int
	Offset         int
}

// FlowDocument represents a flow document (simplified for interface)
type FlowDocument struct {
	ID             string
	Name           string
	Description    string
	Content        json.RawMessage // raw JSON — avoids base64 double-encoding in Postgres JSONB
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

// AppSettings represents application settings
type AppSettings struct {
	Version   int
	General   GeneralSettings
	Appearance AppearanceSettings
	Layout     LayoutSettings
	AI        AISettings
	Parser    ParserSettings
	Telemetry TelemetrySettings
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

// Settings types (simplified)
type GeneralSettings struct {
	FirstRunCompleted bool
	LastUsedVersion   string
	CheckForUpdates    string
}

type AppearanceSettings struct {
	Theme   string
	Density string
}

type LayoutSettings struct {
	SidebarWidth    int
	InspectorWidth int
	ChatPanelHeight *int
}

type AISettings struct {
	ActiveProvider string
	DemoMode       DemoModeSettings
}

type ParserSettings struct {
	MaxFileSizeMB int
}

type TelemetrySettings struct {
	Enabled bool
}

type DemoModeSettings struct {
	Enabled bool
	DailyLimit int
}